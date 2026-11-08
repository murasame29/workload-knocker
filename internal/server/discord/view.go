package discordbot

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/rollout"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

const (
	// selectMaxOptions is Discord's cap of options per select menu (and our page size).
	selectMaxOptions = 25
	// optionLabelMaxLen is Discord's cap of a select option label.
	optionLabelMaxLen = 100
	// contentMaxLen is Discord's cap of message content.
	contentMaxLen = 2000
)

// view is the content and components of one ephemeral message.
type view struct {
	Content    string
	Components []discordgo.MessageComponent
}

// text is a final message without components.
func text(s string) view { return view{Content: s, Components: noComponents()} }

// noComponents is a non-nil empty slice: JSON null would leave the old components in place.
func noComponents() []discordgo.MessageComponent { return []discordgo.MessageComponent{} }

func header(cl config.Cluster) string {
	return fmt.Sprintf("**%s / %s**", strings.ToUpper(cl.Env), cl.Name)
}

// pageBounds returns the clamped page and its [start, end) range over total items.
func pageBounds(total, size, page int) (clamped, start, end, pages int) {
	if size <= 0 || total <= 0 {
		return 0, 0, 0, 0
	}
	pages = (total + size - 1) / size
	clamped = min(max(page, 0), pages-1)
	start = clamped * size
	end = min(start+size, total)
	return clamped, start, end, pages
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func button(label string, style discordgo.ButtonStyle, id customID, disabled bool) discordgo.Button {
	return discordgo.Button{Label: label, Style: style, CustomID: id.String(), Disabled: disabled}
}

func pageButtons(kind, token string, page, pages int) []discordgo.MessageComponent {
	if pages <= 1 {
		return nil
	}
	return []discordgo.MessageComponent{
		button("◀ Previous", discordgo.SecondaryButton, customID{kind, token, page - 1}, page == 0),
		button("Next ▶", discordgo.SecondaryButton, customID{kind, token, page + 1}, page >= pages-1),
	}
}

func selectMenu(id customID, placeholder string, minValues int, options []discordgo.SelectMenuOption) discordgo.SelectMenu {
	return discordgo.SelectMenu{
		MenuType:    discordgo.StringSelectMenu,
		CustomID:    id.String(),
		Placeholder: placeholder,
		MinValues:   &minValues,
		MaxValues:   len(options),
		Options:     options,
	}
}

// namespaceView lists one page of namespaces (already sorted) as a single-choice select menu.
func namespaceView(token string, cl config.Cluster, namespaces []string, page int) view {
	page, start, end, pages := pageBounds(len(namespaces), selectMaxOptions, page)
	options := make([]discordgo.SelectMenuOption, 0, end-start)
	for _, ns := range namespaces[start:end] {
		options = append(options, discordgo.SelectMenuOption{Label: truncateRunes(ns, optionLabelMaxLen), Value: ns})
	}
	menu := selectMenu(customID{kindNamespaceSelect, token, 0}, "Select a namespace", 1, options)
	menu.MaxValues = 1

	content := header(cl) + "\nSelect a namespace."
	if pages > 1 {
		content += fmt.Sprintf(" (page %d/%d)", page+1, pages)
	}
	buttons := append(pageButtons(kindNamespacePage, token, page, pages),
		button("Cancel", discordgo.DangerButton, customID{kindCancel, token, 0}, false))
	return view{Content: content, Components: []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{menu}},
		discordgo.ActionsRow{Components: buttons},
	}}
}

// workloadView renders one page of entries as a multi-select. Option values are indexes into
// entries because workload names can exceed the 100-char option value limit.
func workloadView(token string, cl config.Cluster, namespace string, entries []frontend.Entry, selected map[string]bool, page int, chatStop bool) view {
	content := fmt.Sprintf("%s / %s\n🟢 running  🟡 partial  🔴 stopped", header(cl), namespace)
	if !chatStop {
		content += "\nℹ Stopping from chat is disabled: running workloads are stopped by the nightly job."
	}
	cancel := button("Cancel", discordgo.DangerButton, customID{kindCancel, token, 0}, false)
	if len(entries) == 0 {
		return view{
			Content: content + "\nNo workloads you may operate in this namespace.",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{cancel}},
			},
		}
	}

	page, start, end, pages := pageBounds(len(entries), selectMaxOptions, page)
	options := make([]discordgo.SelectMenuOption, 0, end-start)
	for i := start; i < end; i++ {
		options = append(options, discordgo.SelectMenuOption{
			Label:   truncateRunes(entries[i].Label(), optionLabelMaxLen),
			Value:   strconv.Itoa(i),
			Default: selected[entries[i].Value()],
		})
	}
	menu := selectMenu(customID{kindWorkloadSelect, token, 0}, "Workloads to keep running", 0, options)

	content += "\nChecked workloads run; unchecked ones stop."
	if pages > 1 {
		content += fmt.Sprintf(" (page %d/%d)", page+1, pages)
	}
	buttons := pageButtons(kindWorkloadPage, token, page, pages)
	buttons = append(buttons,
		button("Apply", discordgo.PrimaryButton, customID{kindPlan, token, 0}, false),
		cancel)
	return view{Content: content, Components: []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{menu}},
		discordgo.ActionsRow{Components: buttons},
	}}
}

// planLines renders plan changes in the same wording as the Slack preview.
func planLines(changes []frontend.Change) []string {
	lines := make([]string, 0, len(changes))
	for _, c := range changes {
		if c.Action == frontend.ActionStart {
			lines = append(lines, fmt.Sprintf("🟢 %s: stopped → **start**", c.Label))
		} else {
			lines = append(lines, fmt.Sprintf("🔴 %s: running → **stop**", c.Label))
		}
	}
	return lines
}

// rejectionLines renders refused changes with the reason.
func rejectionLines(rejected []frontend.Rejection) []string {
	lines := make([]string, 0, len(rejected))
	for _, r := range rejected {
		lines = append(lines, fmt.Sprintf("⛔ %s: %s", r.Label, r.Message()))
	}
	return lines
}

// maxDurationInputs is Discord's cap of inputs per modal.
const maxDurationInputs = 5

// previewView shows the plan with the auto-stop of each start, Confirm, Change duration and Cancel.
func previewView(token string, cl config.Cluster, namespace string, changes []frontend.Change, rejected []frontend.Rejection, starts []frontend.Schedule) view {
	head := fmt.Sprintf("%s / %s change preview:", header(cl), namespace)
	lines := append(planLines(changes), scheduleLines(starts)...)
	if len(starts) > maxDurationInputs {
		lines = append(lines, fmt.Sprintf("ℹ Change duration covers the first %d starts; the others use their default.", maxDurationInputs))
	}
	buttons := []discordgo.MessageComponent{button("Confirm", discordgo.SuccessButton, customID{kindConfirm, token, 0}, false)}
	if len(starts) > 0 {
		buttons = append(buttons, button("Change duration", discordgo.SecondaryButton, customID{kindDuration, token, 0}, false))
	}
	buttons = append(buttons, button("Cancel", discordgo.DangerButton, customID{kindCancel, token, 0}, false))
	return view{
		Content:    fitLines(head, append(lines, rejectionLines(rejected)...), contentMaxLen),
		Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: buttons}},
	}
}

// stopTime renders t in each viewer's local time zone.
func stopTime(t time.Time) string { return fmt.Sprintf("<t:%d:f>", t.Unix()) }

// scheduleLines groups the starts by run duration; nothing is shown when no start has an auto-stop.
func scheduleLines(starts []frontend.Schedule) []string {
	var scheduled bool
	for _, s := range starts {
		scheduled = scheduled || s.Duration > 0
	}
	if !scheduled {
		return nil
	}
	var lines []string
	done := map[time.Duration]bool{}
	for _, s := range starts {
		if s.Err != nil {
			lines = append(lines, fmt.Sprintf("⛔ %s: %v", s.Name, s.Err))
			continue
		}
		if done[s.Duration] {
			continue
		}
		done[s.Duration] = true
		var names []string
		for _, o := range starts {
			if o.Err == nil && o.Duration == s.Duration {
				names = append(names, o.Name)
			}
		}
		if s.Duration == 0 {
			lines = append(lines, fmt.Sprintf("⏰ %s: no auto-stop", strings.Join(names, ", ")))
		} else {
			lines = append(lines, fmt.Sprintf("⏰ %s: auto-stop in %s (%s)", strings.Join(names, ", "), runlimit.FormatHours(s.Duration), stopTime(s.StopAt)))
		}
	}
	return lines
}

// appliedStopLines groups the started workloads by their stop time.
func appliedStopLines(applied []frontend.Applied) []string {
	var lines []string
	done := map[int64]bool{}
	for _, a := range applied {
		if !a.Found || a.StopAt.IsZero() || done[a.StopAt.Unix()] {
			continue
		}
		done[a.StopAt.Unix()] = true
		var names []string
		for _, o := range applied {
			if o.Found && o.StopAt.Unix() == a.StopAt.Unix() {
				names = append(names, o.Name)
			}
		}
		lines = append(lines, fmt.Sprintf("⏰ %s: auto-stop at %s", strings.Join(names, ", "), stopTime(a.StopAt)))
	}
	return lines
}

// durationModal asks for the run duration in hours of each of the first starts. drafts hold the
// raw inputs of a rejected submission and win over the current value.
func durationModal(token string, starts []frontend.Schedule, drafts []string) *discordgo.InteractionResponse {
	rows := make([]discordgo.MessageComponent, 0, maxDurationInputs)
	for i, s := range starts[:min(len(starts), maxDurationInputs)] {
		value := ""
		switch {
		case i < len(drafts):
			value = drafts[i]
		case s.Duration > 0:
			value = runlimit.FormatHoursNumber(s.Duration)
		}
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
			CustomID:    strconv.Itoa(i),
			Label:       truncateRunes(s.Name, 45),
			Style:       discordgo.TextInputShort,
			Placeholder: durationPlaceholder(s),
			Value:       value,
			MaxLength:   8,
		}}})
	}
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID:   customID{kindDurationModal, token, 0}.String(),
			Title:      "Run duration (hours)",
			Components: rows,
		},
	}
}

func durationPlaceholder(s frontend.Schedule) string {
	def := "no auto-stop"
	if s.Default > 0 {
		def = runlimit.FormatHours(s.Default)
	}
	p := "Hours (min 0.25). Empty = default " + def
	if s.Max > 0 {
		p += ", max " + runlimit.FormatHours(s.Max)
	}
	return truncateRunes(p, 100)
}

// retryView explains rejected duration inputs and offers to reopen the modal.
func retryView(token string, lines []string) view {
	return view{
		Content: fitLines("⚠ Invalid run duration:", lines, contentMaxLen),
		Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			button("Try again", discordgo.PrimaryButton, customID{kindDurationRetry, token, 0}, false),
		}}},
	}
}

// resultText renders the outcome of an applied plan.
func resultText(userID string, cl config.Cluster, namespace string, res frontend.ApplyResult) string {
	return progressText(userID, cl, namespace, res, nil, 0)
}

// progressText is resultText with the rollout progress of the applied workloads; applied
// workloads missing from snap keep the plain "→ start" line.
func progressText(userID string, cl config.Cluster, namespace string, res frontend.ApplyResult, snap rollout.Snapshot, timeout time.Duration) string {
	progress := make(map[string]rollout.Item, len(snap))
	for _, it := range snap {
		progress[it.Target.Name] = it
	}
	lines := make([]string, 0, len(res.Applied)+1)
	for _, a := range res.Applied {
		it, tracked := progress[a.Name]
		switch {
		case !a.Found:
			lines = append(lines, fmt.Sprintf("⚠ %s → %s (not found)", a.Name, a.Action))
		case tracked:
			lines = append(lines, rollout.Line(it, timeout))
		case a.Action == frontend.ActionStart:
			lines = append(lines, fmt.Sprintf("🟢 %s → start", a.Name))
		default:
			lines = append(lines, fmt.Sprintf("🔴 %s → stop", a.Name))
		}
	}
	lines = append(lines, appliedStopLines(res.Applied)...)
	lines = append(lines, rejectionLines(res.Rejected)...)
	if res.AutoStop {
		lines = append(lines, "⏰ Auto-stop: today 22:00 JST")
	}
	head := fmt.Sprintf("🚀 <@%s> changed workloads (%s / %s / %s)", userID, strings.ToUpper(cl.Env), cl.Name, namespace)
	return fitLines(head, lines, contentMaxLen)
}

// fitLines joins head and lines, replacing the overflowing tail with a count to stay within limit bytes.
func fitLines(head string, lines []string, limit int) string {
	var b strings.Builder
	b.WriteString(head)
	for i, line := range lines {
		more := fmt.Sprintf("\n… and %d more", len(lines)-i)
		if b.Len()+1+len(line)+len(more) > limit {
			b.WriteString(more)
			return b.String()
		}
		b.WriteString("\n" + line)
	}
	return b.String()
}
