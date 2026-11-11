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

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/reservation"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

const (
	slashSchedule = "schedule"

	callbackScheduleModal = "schedule_modal"
	blockReserveAt        = "block_reserve_at"
	actionReserveAt       = "action_reserve_at"
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

// scheduleMetadata is the private_metadata of the schedule modal.
type scheduleMetadata struct {
	ChannelID  string   `json:"c"`
	UserID     string   `json:"u"`
	Cluster    string   `json:"e"`
	Namespaces []string `json:"n"`
}

// handleSchedule opens the reservation modal: checkboxes per namespace, a date and time picker
// (shown and read in the user's own time zone) and the run duration in hours.
func (b *Bot) handleSchedule(cmd slack.SlashCommand, cl config.Cluster) {
	if b.reservations == nil {
		b.ephemeral(cmd.ChannelID, cmd.UserID, "⚠️ Reservations are not available.")
		return
	}
	sub := b.subject(cmd.UserID)
	namespaces, total := b.svc.Namespaces(sub, cl.Env)
	listings := b.listNamespaces(sub, cl.Env, namespaces)
	switch {
	case len(listings) == 0 && total > 0:
		b.ephemeral(cmd.ChannelID, cmd.UserID, "⚠️ You are not authorized to operate any workload in this cluster.")
		return
	case len(listings) == 0:
		b.ephemeral(cmd.ChannelID, cmd.UserID, "⚠️ No managed workloads are registered yet.")
		return
	case !multiNamespaceFits(listings):
		b.ephemeral(cmd.ChannelID, cmd.UserID, "⚠️ The workloads do not fit the schedule form. Use POST /api/v1/reservations instead.")
		return
	}
	meta := scheduleMetadata{ChannelID: cmd.ChannelID, UserID: cmd.UserID, Cluster: cl.Env}
	for _, l := range listings {
		meta.Namespaces = append(meta.Namespaces, l.Namespace)
	}
	raw, err := json.Marshal(meta)
	if err != nil || len(raw) > slackPrivateMetadataMaxLen {
		b.ephemeral(cmd.ChannelID, cmd.UserID, "⚠️ The workloads do not fit the schedule form. Use POST /api/v1/reservations instead.")
		return
	}
	if _, err := b.client.OpenView(cmd.TriggerID, scheduleModal(cl, listings, string(raw), b.svc.Now())); err != nil {
		slog.Error("failed to open the schedule modal", slog.String("error.message", err.Error()))
	}
}

// scheduleModal renders the reservation form; no workload is checked by default.
func scheduleModal(cl config.Cluster, listings []nsListing, meta string, now time.Time) slack.ModalViewRequest {
	blocks := multiNamespaceBlocks(listings)
	for _, blk := range blocks {
		if in, ok := blk.(*slack.InputBlock); ok {
			if cb, ok := in.Element.(*slack.CheckboxGroupsBlockElement); ok {
				cb.InitialOptions = nil
			}
			in.Hint = nil
		}
	}
	picker := slack.NewDateTimePickerBlockElement(actionReserveAt)
	picker.InitialDateTime = now.Add(time.Hour).Truncate(time.Hour).Unix()
	at := slack.NewInputBlock(blockReserveAt, slack.NewTextBlockObject("plain_text", "Ready at", false, false),
		slack.NewTextBlockObject("plain_text", "Shown in your time zone; the workloads start a few minutes earlier.", false, false), picker)
	hours := slack.NewInputBlock(blockReserveHours, slack.NewTextBlockObject("plain_text", "Run duration (hours)", false, false),
		slack.NewTextBlockObject("plain_text", "Empty = workload default. Minimum 0.25.", false, false),
		slack.NewNumberInputBlockElement(slack.NewTextBlockObject("plain_text", "3", false, false), actionReserveHours, true).WithMinValue("0.25"))
	hours.Optional = true
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      callbackScheduleModal,
		Title:           slack.NewTextBlockObject("plain_text", truncateText("📅 "+cl.Env, slackModalTitleMaxLen), false, false),
		Submit:          slack.NewTextBlockObject("plain_text", "Reserve", false, false),
		Close:           slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		PrivateMetadata: meta,
		Blocks:          slack.Blocks{BlockSet: append(blocks, at, hours)},
	}
}

// scheduleInput is the parsed reservation form.
type scheduleInput struct {
	at    time.Time
	hours time.Duration
	names map[string][]string // namespace -> checked workload names
}

// parseSchedule reads the form; errs maps an input block to its message (response_action errors).
func parseSchedule(meta scheduleMetadata, values map[string]map[string]slack.BlockAction) (scheduleInput, map[string]string) {
	in := scheduleInput{names: map[string][]string{}}
	errs := map[string]string{}
	if ts := values[blockReserveAt][actionReserveAt].SelectedDateTime; ts > 0 {
		in.at = time.Unix(ts, 0).UTC()
	} else {
		errs[blockReserveAt] = "Pick a date and time."
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
		errs[blockReserveAt] = "Check at least one workload."
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
	in, errs := parseSchedule(meta, callback.View.State.Values)
	if len(errs) > 0 {
		b.ack(*req, map[string]any{"response_action": "errors", "errors": errs})
		return
	}
	cl, ok := b.registry.ByEnv(meta.Cluster)
	if !ok {
		b.ack(*req)
		b.ephemeral(meta.ChannelID, meta.UserID, "⚠️ The target Cluster is no longer in the Registry. Please run the command again.")
		return
	}
	b.ack(*req)
	lines := b.createReservations(b.subject(callback.User.ID), cl, meta.Namespaces, in)
	header := fmt.Sprintf("*%s / %s* reservation", strings.ToUpper(cl.Env), cl.Name)
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
			lines = append(lines, fmt.Sprintf("⛔ %s: %s", escapeMrkdwn(ns), escapeMrkdwn(frontend.ReservationError(err))))
			continue
		}
		lines = append(lines, markup.ReservedLine(r, b.reservations.Warmup(cl.Env, r.Namespace, r.Workloads)))
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
func reservationBlocks(env string, list []reservation.Reservation, free int) []slack.Block {
	if len(list) == 0 || free < 2 {
		return nil
	}
	lines := make([]string, 0, len(list))
	var buttons []slack.BlockElement
	for _, r := range list {
		lines = append(lines, markup.PendingLine(r))
		value, _ := json.Marshal(reservationRef{Env: env, ID: r.ID})
		label := truncateText("Cancel: "+r.Namespace+" "+r.At.UTC().Format("01-02 15:04")+" UTC", slackButtonTextMaxLen)
		buttons = append(buttons, slack.NewButtonBlockElement(actionReservationStop, string(value), slack.NewTextBlockObject("plain_text", label, false, false)).WithStyle(slack.StyleDanger))
	}
	blocks := mrkdwnSections("*Reservations*", lines)
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
		b.replaceOriginal(callback, "⚠️ The target Cluster is no longer in the Registry. Please run the command again.", nil)
		return
	}
	sub := b.subject(callback.User.ID)
	ctx, cancel := context.WithTimeout(context.Background(), reservationCallTimeout)
	r, err := b.reservations.Cancel(ctx, ref.Env, ref.ID, sub, audit.SourceSlack)
	cancel()
	note := markup.CancelledLine(r)
	if err != nil {
		note = "⛔ " + escapeMrkdwn(frontend.ReservationError(err))
	}
	text, blocks := statusMessage(cl, b.svc.Status(sub, cl.Env), b.visibleReservations(sub, cl.Env), []string{note})
	b.replaceOriginal(callback, text, blocks)
}

func (b *Bot) ephemeral(channelID, userID, text string) {
	if _, err := b.client.PostEphemeral(channelID, userID, slack.MsgOptionText(text, false)); err != nil {
		slog.Error("failed to post ephemeral message", slog.String("error.message", err.Error()))
	}
}
