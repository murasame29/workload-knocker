package slackbot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/observability/audit"
	"github.com/murasame29/workload-knocker/internal/policy/authz"
	"github.com/murasame29/workload-knocker/internal/policy/runlimit"
	"github.com/murasame29/workload-knocker/internal/schedule/reservation"
)

const (
	slashSchedule = "schedule"

	callbackScheduleModal = "schedule_modal"
	blockReserveIn        = "block_reserve_in"
	actionReserveIn       = "action_reserve_in"
	blockReserveHours     = "block_reserve_hours"
	actionReserveHours    = "action_reserve_hours"
	actionReservationStop = "reservation_cancel"

	// reservationCallTimeout bounds a create or cancel (persisted through the cluster API).
	reservationCallTimeout = 10 * time.Second
	// buttonsPerActionBlock is Slack's cap of elements per actions block.
	buttonsPerActionBlock = 25
)

// reservations is the part of reservation.Service the bot needs.
type reservations interface {
	Create(ctx context.Context, req reservation.CreateRequest) (reservation.Reservation, error)
	Cancel(ctx context.Context, env, id string, sub authz.Subject, source audit.Source) (reservation.Reservation, error)
	Visible(ctx context.Context, sub authz.Subject, env string) []reservation.Reservation
	Warmup(env, namespace string, names []string) time.Duration
}

// SetReservations enables the schedule argument and the reservation list in status.
func (b *Bot) SetReservations(r reservations) { b.reservations = r }

// scheduleMetadata is the private_metadata of the schedule modal; Namespaces holds the chosen
// namespace once picked.
type scheduleMetadata struct {
	ChannelID  string   `json:"c"`
	UserID     string   `json:"u"`
	Cluster    string   `json:"e"`
	Namespaces []string `json:"n"`
}

// handleSchedule opens the schedule flow at its namespace select.
func (b *Bot) handleSchedule(cmd slack.SlashCommand, cl config.Cluster) {
	b.openView(cmd.TriggerID, b.scheduleStart(b.subject(cmd.UserID), cl, cmd.ChannelID, cmd.UserID))
}

// scheduleModal renders the reservation form of one namespace: the namespace select, the
// unchecked workloads, the hours until they are Ready and the run duration.
func (u ui) scheduleModal(cl config.Cluster, namespaces []string, namespace string, listing frontend.Listing, meta string) slack.ModalViewRequest {
	blocks := []slack.Block{u.namespacePicker(actionScheduleNamespace, namespaces, namespace)}
	if len(listing.Entries) == 0 {
		blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", u.t("ℹ️ You are not authorized to start any workload in this namespace."), false, false), nil, nil))
		return slack.ModalViewRequest{
			Type: slack.VTModal, CallbackID: callbackScheduleModal, PrivateMetadata: meta,
			Title:  slack.NewTextBlockObject("plain_text", truncateText("📅 "+cl.Env, slackModalTitleMaxLen), false, false),
			Close:  slack.NewTextBlockObject("plain_text", u.t("Close"), false, false),
			Blocks: slack.Blocks{BlockSet: blocks},
		}
	}
	checks := u.multiNamespaceBlocks([]nsListing{{Namespace: namespace, Listing: listing}})
	for _, blk := range checks {
		if in, ok := blk.(*slack.InputBlock); ok {
			if cb, ok := in.Element.(*slack.CheckboxGroupsBlockElement); ok {
				cb.InitialOptions = nil
			}
			in.Hint = nil
		}
	}
	readyIn := slack.NewInputBlock(blockReserveIn, slack.NewTextBlockObject("plain_text", u.t("Ready in (hours)"), false, false),
		slack.NewTextBlockObject("plain_text", u.t("Hours from now until the workloads are ready; they start a few minutes earlier. Minimum 0.25."), false, false),
		slack.NewNumberInputBlockElement(slack.NewTextBlockObject("plain_text", "1", false, false), actionReserveIn, true).WithMinValue("0.25"))
	hours := slack.NewInputBlock(blockReserveHours, slack.NewTextBlockObject("plain_text", u.t("Run duration (hours)"), false, false),
		slack.NewTextBlockObject("plain_text", u.t("Hours they run once ready, then they stop. Empty = workload default. Minimum 0.25."), false, false),
		slack.NewNumberInputBlockElement(slack.NewTextBlockObject("plain_text", "3", false, false), actionReserveHours, true).WithMinValue("0.25"))
	hours.Optional = true
	blocks = append(blocks, checks...)
	blocks = append(blocks, readyIn, hours)
	if listing.Truncated > 0 {
		blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn",
			u.t(":warning: %d entries do not fit this form. Use POST /api/v1/reservations for those.", listing.Truncated), false, false), nil, nil))
	}
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      callbackScheduleModal,
		Title:           slack.NewTextBlockObject("plain_text", truncateText("📅 "+cl.Env, slackModalTitleMaxLen), false, false),
		Submit:          slack.NewTextBlockObject("plain_text", u.t("Reserve"), false, false),
		Close:           slack.NewTextBlockObject("plain_text", u.t("Cancel"), false, false),
		PrivateMetadata: meta,
		Blocks:          slack.Blocks{BlockSet: blocks},
	}
}

// scheduleInput is the parsed reservation form.
type scheduleInput struct {
	at    time.Time
	hours time.Duration
	names map[string][]string // namespace -> checked workload names
}

// parseSchedule reads the form; the ready time is now plus the entered hours. errs maps an
// input block to its message (response_action errors).
func (u ui) parseSchedule(meta scheduleMetadata, values map[string]map[string]slack.BlockAction, now time.Time) (scheduleInput, map[string]string) {
	in := scheduleInput{names: map[string][]string{}}
	errs := map[string]string{}
	switch readyIn, err := runlimit.ParseHours(values[blockReserveIn][actionReserveIn].Value); {
	case err != nil:
		errs[blockReserveIn] = err.Error()
	case readyIn == 0:
		errs[blockReserveIn] = u.t("Enter the hours until the workloads are ready.")
	default:
		in.at = now.Add(readyIn).UTC()
	}
	d, err := runlimit.ParseHours(values[blockReserveHours][actionReserveHours].Value)
	if err != nil {
		errs[blockReserveHours] = err.Error()
	}
	in.hours = d
	for ni, selected := range selectedByNamespace(values) {
		if ni < 0 || ni >= len(meta.Namespaces) {
			continue
		}
		for name := range selected {
			in.names[meta.Namespaces[ni]] = append(in.names[meta.Namespaces[ni]], name)
		}
	}
	if len(errs) == 0 && len(in.names) == 0 {
		errs[blockReserveIn] = u.t("Check at least one workload.")
	}
	return in, errs
}

// handleScheduleSubmit creates one reservation per namespace with checked entries. Input errors
// keep the modal open; otherwise it closes and the outcome is posted to the user.
func (b *Bot) handleScheduleSubmit(callback slack.InteractionCallback, req *socketmode.Request) {
	var meta scheduleMetadata
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil || b.reservations == nil {
		b.ack(*req)
		return
	}
	in, errs := b.parseSchedule(meta, callback.View.State.Values, b.svc.Now())
	if len(errs) > 0 {
		b.ack(*req, map[string]any{"response_action": "errors", "errors": errs})
		return
	}
	cl, ok := b.registry.ByEnv(meta.Cluster)
	if !ok {
		b.ack(*req)
		b.ephemeral(meta.ChannelID, meta.UserID, b.t("⚠️ The target Cluster is no longer in the Registry. Please run the command again."))
		return
	}
	b.ack(*req)
	lines := b.createReservations(b.subject(callback.User.ID), cl, meta.Namespaces, in)
	header := b.t("*%s / %s* reservation", strings.ToUpper(cl.Env), cl.Name)
	if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID, slack.MsgOptionBlocks(mrkdwnSections(header, lines)...)); err != nil {
		slog.Error("failed to post the reservation result", slog.String("error.message", err.Error()))
	}
}

// createReservations creates the reservations in namespace order and returns one line each.
// Checked names are the representative names of entries; merged entries expand to their members.
func (b *Bot) createReservations(sub authz.Subject, cl config.Cluster, order []string, in scheduleInput) []string {
	var lines []string
	for _, ns := range order {
		values := in.names[ns]
		if len(values) == 0 {
			continue
		}
		var names []string
		for _, e := range b.svc.List(sub, cl.Env, ns, listLimitsAll).Entries {
			for _, v := range values {
				if e.Value() == v {
					names = append(names, e.Names()...)
				}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), reservationCallTimeout)
		r, err := b.reservations.Create(ctx, reservation.CreateRequest{Env: cl.Env, Namespace: ns, Workloads: names, At: in.at, Duration: in.hours, Subject: sub, Source: audit.SourceSlack})
		cancel()
		if err != nil {
			lines = append(lines, fmt.Sprintf("⛔ %s: %s", escapeMrkdwn(ns), escapeMrkdwn(b.markup.ReservationError(err))))
			continue
		}
		lines = append(lines, b.markup.ReservedLine(r, b.reservations.Warmup(cl.Env, r.Namespace, r.Workloads)))
	}
	return lines
}

// reservationRef is the value of a reservation Cancel button.
type reservationRef struct {
	Env string `json:"e"`
	ID  string `json:"i"`
}

// visibleReservations returns the pending reservations sub may see in status.
func (b *Bot) visibleReservations(sub authz.Subject, env string) []reservation.Reservation {
	if b.reservations == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), reservationCallTimeout)
	defer cancel()
	return b.reservations.Visible(ctx, sub, env)
}

// reservationBlocks renders the pending reservations of status with a Cancel button each; free is
// the number of blocks left in the message.
func (u ui) reservationBlocks(env string, list []reservation.Reservation, free int) []slack.Block {
	if len(list) == 0 || free < 2 {
		return nil
	}
	lines := make([]string, 0, len(list))
	var buttons []slack.BlockElement
	for _, r := range list {
		lines = append(lines, u.markup.PendingLine(r))
		value, _ := json.Marshal(reservationRef{Env: env, ID: r.ID})
		label := truncateText(u.t("Cancel: %s %s UTC", r.Namespace, r.At.UTC().Format("01-02 15:04")), slackButtonTextMaxLen)
		buttons = append(buttons, slack.NewButtonBlockElement(actionReservationStop, string(value), slack.NewTextBlockObject("plain_text", label, false, false)).WithStyle(slack.StyleDanger))
	}
	blocks := mrkdwnSections(u.t("*Reservations*"), lines)
	for start := 0; start < len(buttons) && len(blocks) < free; start += buttonsPerActionBlock {
		blocks = append(blocks, slack.NewActionBlock("", buttons[start:min(start+buttonsPerActionBlock, len(buttons))]...))
	}
	if len(blocks) > free {
		return nil
	}
	return blocks
}

// handleReservationCancel cancels the reservation of a status button and refreshes the list.
func (b *Bot) handleReservationCancel(callback slack.InteractionCallback, value string) {
	var ref reservationRef
	if err := json.Unmarshal([]byte(value), &ref); err != nil || ref.Env == "" || ref.ID == "" || b.reservations == nil {
		slog.Warn("invalid reservation button value")
		return
	}
	cl, ok := b.registry.ByEnv(ref.Env)
	if !ok {
		b.replaceOriginal(callback, b.t("⚠️ The target Cluster is no longer in the Registry. Please run the command again."), nil)
		return
	}
	sub := b.subject(callback.User.ID)
	ctx, cancel := context.WithTimeout(context.Background(), reservationCallTimeout)
	r, err := b.reservations.Cancel(ctx, ref.Env, ref.ID, sub, audit.SourceSlack)
	cancel()
	note := b.markup.CancelledLine(r)
	if err != nil {
		note = "⛔ " + escapeMrkdwn(b.markup.ReservationError(err))
	}
	b.refreshStatus(callback, cl, sub, []string{note})
}

func (b *Bot) ephemeral(channelID, userID, text string) {
	if _, err := b.client.PostEphemeral(channelID, userID, slack.MsgOptionText(text, false)); err != nil {
		slog.Error("failed to post ephemeral message", slog.String("error.message", err.Error()))
	}
}
