package discordbot

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/policy/runlimit"
	"github.com/murasame29/workload-knocker/internal/workload/rollout"
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
// opened counts the namespaces already opened in this flow; with any, Apply previews them.
func namespaceView(token string, cl config.Cluster, namespaces []string, page, opened int) view {
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
	if opened > 0 {
		content += fmt.Sprintf("\n%d namespace(s) selected; Apply previews all of them.", opened)
	}
	buttons := pageButtons(kindNamespacePage, token, page, pages)
	if opened > 0 {
		buttons = append(buttons, button("Apply", discordgo.PrimaryButton, customID{kindPlan, token, 0}, false))
	}
	buttons = append(buttons, button("Cancel", discordgo.DangerButton, customID{kindCancel, token, 0}, false))
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
	other := button("Other namespace", discordgo.SecondaryButton, customID{kindNamespaces, token, 0}, false)
	if len(entries) == 0 {
		return view{
			Content: content + "\nNo workloads you may operate in this namespace.",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{other, cancel}},
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
		other,
		cancel)
	return view{Content: content, Components: []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{menu}},
		discordgo.ActionsRow{Components: buttons},
	}}
}

// maxDurationInputs is Discord's cap of inputs per modal.
const maxDurationInputs = 5

// section is the previewed plan of one namespace.
type section struct {
	namespace string
	changes   []frontend.Change
	rejected  []frontend.Rejection
	starts    []frontend.Schedule
}

// namespaceHead names the namespaces of sections in a header line.
func namespaceHead(cl config.Cluster, namespaces []string) string {
	return fmt.Sprintf("%s / %s", header(cl), strings.Join(namespaces, ", "))
}

// previewView shows the plan of every section with the auto-stop of each start, Confirm,
// Change duration and Cancel. Several sections get a bold namespace line each.
func previewView(token string, cl config.Cluster, sections []section) view {
	names := make([]string, 0, len(sections))
	var lines []string
	starts := 0
	for _, s := range sections {
		names = append(names, s.namespace)
		if len(sections) > 1 {
			lines = append(lines, "**"+s.namespace+"**")
		}
		lines = append(lines, markup.PlanLines(s.changes)...)
		lines = append(lines, markup.ScheduleLines(s.starts)...)
		lines = append(lines, frontend.RejectionLines(s.rejected)...)
		starts += len(s.starts)
	}
	if starts > maxDurationInputs {
		lines = append(lines, fmt.Sprintf("ℹ Change duration covers the first %d starts; the others use their default.", maxDurationInputs))
	}
	buttons := []discordgo.MessageComponent{button("Confirm", discordgo.SuccessButton, customID{kindConfirm, token, 0}, false)}
	if starts > 0 {
		buttons = append(buttons, button("Change duration", discordgo.SecondaryButton, customID{kindDuration, token, 0}, false))
	}
	buttons = append(buttons, button("Cancel", discordgo.DangerButton, customID{kindCancel, token, 0}, false))
	return view{
		Content:    fitLines(namespaceHead(cl, names)+" change preview:", lines, contentMaxLen),
		Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: buttons}},
	}
}

// stopTime renders t in each viewer's local time zone.
func stopTime(t time.Time) string { return fmt.Sprintf("<t:%d:f>", t.Unix()) }

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

// applied is the outcome of the plan of one namespace.
type applied struct {
	namespace string
	res       frontend.ApplyResult
}

// resultText renders the outcome of an applied plan.
func resultText(userID string, cl config.Cluster, results []applied) string {
	return progressText(userID, cl, results, nil, 0)
}

// progressText is resultText with the rollout progress of the applied workloads; applied
// workloads missing from snap keep the plain "→ start" line. Several namespaces get a bold
// namespace line each.
func progressText(userID string, cl config.Cluster, results []applied, snap rollout.Snapshot, timeout time.Duration) string {
	progress := make(map[string]rollout.Item, len(snap))
	for _, it := range snap {
		progress[it.Target.Namespace+"/"+it.Target.Name] = it
	}
	names := make([]string, 0, len(results))
	var lines []string
	for _, r := range results {
		names = append(names, r.namespace)
		if len(results) > 1 {
			lines = append(lines, "**"+r.namespace+"**")
		}
		for _, a := range r.res.Applied {
			it, tracked := progress[r.namespace+"/"+a.Name]
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
		lines = append(lines, markup.AppliedStopLines(r.res.Applied)...)
		lines = append(lines, frontend.RejectionLines(r.res.Rejected)...)
	}
	head := fmt.Sprintf("🚀 <@%s> changed workloads (%s / %s / %s)", userID, strings.ToUpper(cl.Env), cl.Name, strings.Join(names, ", "))
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
