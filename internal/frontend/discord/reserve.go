package discordbot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/observability/audit"
	"github.com/murasame29/workload-knocker/internal/policy/authz"
	"github.com/murasame29/workload-knocker/internal/policy/runlimit"
	"github.com/murasame29/workload-knocker/internal/schedule/reservation"
)

const subcommandSchedule = "schedule"

// Custom IDs of the schedule modal inputs; checkbox groups use their part index.
const (
	inputAt    = "at"
	inputHours = "hours"
	// scheduleMaxNamespaces leaves two of the modal's five labels for the time and the hours.
	scheduleMaxNamespaces = modalMaxLabels - 2
	// reservationCallTimeout bounds a create or cancel from chat (persists through the cluster API).
	reservationCallTimeout = 10 * time.Second
)

// reservations is the part of reservation.Service the bot needs.
type reservations interface {
	Create(ctx context.Context, req reservation.CreateRequest) (reservation.Reservation, error)
	Cancel(ctx context.Context, env, id string, sub authz.Subject, source audit.Source) (reservation.Reservation, error)
	Visible(ctx context.Context, sub authz.Subject, env string) []reservation.Reservation
	Warmup(env, namespace string, names []string) time.Duration
}

// SetReservations enables the schedule subcommand and the reservation list in status.
func (b *Bot) SetReservations(r reservations) { b.reservations = r }

// handleSchedule opens the reservation modal: one checkbox group per namespace (at most 3, of
// at most 10 entries), the time and the run duration in hours.
func (b *Bot) handleSchedule(i *discordgo.Interaction, cl config.Cluster, userID string) {
	if b.reservations == nil {
		b.replyEphemeral(i, "⚠ Reservations are not available.")
		return
	}
	sub := subject(i)
	namespaces, total := b.svc.Namespaces(sub, cl.Env)
	switch {
	case len(namespaces) == 0 && total > 0:
		b.replyEphemeral(i, msgNotAuthorized)
		return
	case len(namespaces) == 0:
		b.replyEphemeral(i, "⚠ No managed workloads are registered yet.")
		return
	case len(namespaces) > scheduleMaxNamespaces:
		b.replyEphemeral(i, fmt.Sprintf("⚠ The schedule form fits %d namespaces; you may operate %d. Use POST /api/v1/reservations instead.", scheduleMaxNamespaces, len(namespaces)))
		return
	}
	parts := make([]*nsPart, 0, len(namespaces))
	for _, ns := range namespaces {
		entries := b.svc.List(sub, cl.Env, ns, frontend.Limits{}).Entries
		if len(entries) > checkboxMaxOptions {
			b.replyEphemeral(i, fmt.Sprintf("⚠ %s has more than %d entries for the schedule form. Use POST /api/v1/reservations instead.", ns, checkboxMaxOptions))
			return
		}
		parts = append(parts, &nsPart{namespace: ns, entries: entries, selected: map[string]bool{}})
	}
	token, err := b.flows.create(&flow{userID: userID, env: cl.Env, parts: parts, cur: -1})
	if err != nil {
		slog.Error("failed to create discord flow", slog.String("error.message", err.Error()))
		b.replyEphemeral(i, "⚠ Internal error. Please try again.")
		return
	}
	b.respond(i, scheduleModal(token, cl, parts))
}

// scheduleModal renders the reservation form.
func scheduleModal(token string, cl config.Cluster, parts []*nsPart) *discordgo.InteractionResponse {
	labels := make([]discordgo.MessageComponent, 0, len(parts)+2)
	for idx, p := range parts {
		options := make([]checkboxOption, 0, len(p.entries))
		for ei, e := range p.entries {
			options = append(options, checkboxOption{Value: strconv.Itoa(ei), Label: truncateRunes(e.Label(), optionLabelMaxLen)})
		}
		labels = append(labels, labelComponent{
			Label:       truncateRunes(p.namespace, labelMaxLen),
			Description: "Workloads to start",
			Component:   checkboxGroup{CustomID: strconv.Itoa(idx), Options: options},
		})
	}
	labels = append(labels,
		labelComponent{
			Label:       "Ready at",
			Description: truncateRunes("21:00 (today, or tomorrow once passed) or 2026-11-14 21:00, in "+zoneName(cl), labelDescriptionLen),
			Component:   textInput{CustomID: inputAt, Placeholder: "21:00", Required: true, MaxLength: 16},
		},
		labelComponent{
			Label:       "Run duration (hours)",
			Description: "Empty uses the workload default",
			Component:   textInput{CustomID: inputHours, Placeholder: "3", MaxLength: 8},
		},
	)
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID:   customID{kindScheduleModal, token, 0}.String(),
			Title:      truncateRunes("Reserve in "+strings.ToUpper(cl.Env), modalTitleMaxLen),
			Components: labels,
		},
	}
}

func zoneName(cl config.Cluster) string { return cl.Frontends.Discord.Location().String() }

var (
	clockRe    = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
	dateTimeRe = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})[ T](\d{1,2}):(\d{2})$`)
)

// parseAt reads "HH:MM" (today, or tomorrow when already passed) or "YYYY-MM-DD HH:MM" in loc.
func parseAt(s string, now time.Time, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	now = now.In(loc)
	if m := clockRe.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		if h > 23 || mi > 59 {
			return time.Time{}, fmt.Errorf("%q is not a time of day", s)
		}
		at := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, loc)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at, nil
	}
	if m := dateTimeRe.FindStringSubmatch(s); m != nil {
		n := make([]int, 5)
		for k := range n {
			n[k], _ = strconv.Atoi(m[k+1])
		}
		at := time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], 0, 0, loc)
		// time.Date normalizes out-of-range values; reject them instead of moving the date.
		if at.Year() != n[0] || int(at.Month()) != n[1] || at.Day() != n[2] || at.Hour() != n[3] || at.Minute() != n[4] {
			return time.Time{}, fmt.Errorf("%q is not a date and time", s)
		}
		return at, nil
	}
	return time.Time{}, fmt.Errorf("%q: use 21:00 or 2026-11-14 21:00", s)
}

// handleScheduleModal creates one reservation per namespace with checked workloads and answers
// with the result. The policy is checked by the reservation service for each workload.
func (b *Bot) handleScheduleModal(m rawSubmit) {
	userID, _ := interactionUser(m.i)
	if userID == "" || b.reservations == nil {
		return
	}
	sub := subject(m.i)
	b.flowResponse(m.i, m.id.Token, userID, false, func(f *flow, cl config.Cluster) *discordgo.InteractionResponse {
		f.expires = time.Time{}
		reply := func(lines ...string) *discordgo.InteractionResponse {
			return &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Content: fitLines(header(cl)+" reservation", lines, contentMaxLen), Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMentions()},
			}
		}
		at, err := parseAt(m.texts[inputAt], b.svc.Now(), cl.Frontends.Discord.Location())
		if err != nil {
			return reply("⛔ " + err.Error())
		}
		hours, err := runlimit.ParseHours(m.texts[inputHours])
		if err != nil {
			return reply("⛔ " + err.Error())
		}
		var lines []string
		for idx, p := range f.parts {
			names := checkedNames(p, m.checked[strconv.Itoa(idx)])
			if len(names) == 0 {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), reservationCallTimeout)
			r, err := b.reservations.Create(ctx, reservation.CreateRequest{Env: cl.Env, Namespace: p.namespace, Workloads: names, At: at, Duration: hours, Subject: sub, Source: audit.SourceDiscord})
			cancel()
			if err != nil {
				lines = append(lines, fmt.Sprintf("⛔ %s: %s", p.namespace, frontend.ReservationError(err)))
				continue
			}
			lines = append(lines, markup.ReservedLine(r, b.reservations.Warmup(cl.Env, r.Namespace, r.Workloads)))
		}
		if len(lines) == 0 {
			return reply("ℹ No workloads were selected.")
		}
		return reply(lines...)
	})
}

// checkedNames maps checked option values to the workload names of their entries.
func checkedNames(p *nsPart, values []string) []string {
	var names []string
	for _, v := range values {
		ei, err := strconv.Atoi(v)
		if err != nil || ei < 0 || ei >= len(p.entries) {
			continue
		}
		names = append(names, p.entries[ei].Names()...)
	}
	return names
}

// reservationRef is the payload of a reservation Cancel button.
type reservationRef struct {
	Env string `json:"e"`
	ID  string `json:"i"`
}

// reservationLines renders the pending reservations of the status list with a Cancel button
// each, as long as buttons remain.
func (b *Bot) reservationLines(sub authz.Subject, env string, free int) ([]string, []discordgo.MessageComponent) {
	if b.reservations == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), reservationCallTimeout)
	defer cancel()
	list := b.reservations.Visible(ctx, sub, env)
	if len(list) == 0 {
		return nil, nil
	}
	lines := []string{"**Reservations**"}
	var buttons []discordgo.MessageComponent
	for _, r := range list {
		lines = append(lines, markup.PendingLine(r))
		if len(buttons) >= free {
			continue
		}
		payload, _ := json.Marshal(reservationRef{Env: env, ID: r.ID})
		token, err := b.poster.buttons.put(string(payload))
		if err != nil {
			slog.Error("failed to create reservation button", slog.String("error.message", err.Error()))
			continue
		}
		label := truncateRunes("Cancel: "+r.Namespace+" "+r.At.Format("01-02 15:04"), buttonLabelMaxLen)
		buttons = append(buttons, button(label, discordgo.DangerButton, customID{kindReservationStop, token, 0}, false))
	}
	return lines, buttons
}

// handleReservationCancel cancels the reservation of a status button and refreshes the list.
func (b *Bot) handleReservationCancel(i *discordgo.Interaction, id customID) {
	payload, ok := b.poster.buttons.get(id.Token)
	var ref reservationRef
	if !ok || json.Unmarshal([]byte(payload), &ref) != nil || ref.Env == "" || ref.ID == "" || b.reservations == nil {
		b.replyEphemeral(i, msgExpired)
		return
	}
	cl, found := b.registry.ByEnv(ref.Env)
	if !found {
		b.update(i, text(msgClusterGone))
		return
	}
	// Ack first: cancelling persists through the cluster API.
	if !b.deferUpdate(i) {
		return
	}
	sub := subject(i)
	ctx, cancel := context.WithTimeout(context.Background(), reservationCallTimeout)
	r, err := b.reservations.Cancel(ctx, ref.Env, ref.ID, sub, audit.SourceDiscord)
	cancel()
	note := markup.CancelledLine(r)
	if err != nil {
		note = "⛔ " + frontend.ReservationError(err)
	}
	b.edit(i, b.statusView(cl, sub, b.svc.Status(sub, cl.Env), []string{note}))
}
