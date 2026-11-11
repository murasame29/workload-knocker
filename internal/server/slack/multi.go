package slackbot

import (
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
	"github.com/murasame29/workload-knocker/internal/rollout"
)

// Block and action ID prefixes of the multi-namespace modal: <prefix><namespace index>_<page>.
// They differ from blockSelectWorkloads so the single-namespace fallback keeps its own IDs.
const (
	blockNamespaceWorkloads  = "block_ns_"
	actionNamespaceWorkloads = "select_ns_"
)

// nsMeta is one namespace of the multi-namespace modal and its rendered entry count.
type nsMeta struct {
	Namespace string `json:"n"`
	Rendered  int    `json:"r"`
}

// nsListing is the entries of one namespace shown in the multi-namespace modal.
type nsListing struct {
	Namespace string
	Listing   frontend.Listing
}

// listNamespaces lists the operable entries of every namespace; namespaces without any are skipped.
func (b *Bot) listNamespaces(sub authz.Subject, env string, namespaces []string) []nsListing {
	var out []nsListing
	for _, ns := range namespaces {
		l := b.svc.List(sub, env, ns, frontend.Limits{PageSize: checkboxMaxOptions})
		if len(l.Entries) > 0 {
			out = append(out, nsListing{Namespace: ns, Listing: l})
		}
	}
	return out
}

// multiNamespaceFits reports whether every namespace fits one modal with the reserved blocks.
func multiNamespaceFits(listings []nsListing) bool {
	n := 0
	for _, l := range listings {
		n += len(l.Listing.Pages())
	}
	return len(listings) > 0 && n <= slackModalMaxBlocks-reservedModalBlocks
}

// multiNamespaceBlocks renders one checkbox input per page of each namespace, labelled with the
// namespace; running entries are checked. Option values are the representative workload names.
func multiNamespaceBlocks(listings []nsListing) []slack.Block {
	var blocks []slack.Block
	for ni, l := range listings {
		pages := l.Listing.Pages()
		total := len(l.Listing.Entries)
		start := 0
		for pi, chunk := range pages {
			end := start + len(chunk)
			options := make([]*slack.OptionBlockObject, 0, len(chunk))
			var initial []*slack.OptionBlockObject
			for _, e := range chunk {
				opt := slack.NewOptionBlockObject(e.Value(), slack.NewTextBlockObject("plain_text", truncateOptionText(e.Label()), false, false), nil)
				options = append(options, opt)
				if e.AnyRunning() {
					initial = append(initial, opt)
				}
			}
			cb := slack.NewCheckboxGroupsBlockElement(fmt.Sprintf("%s%d_%d", actionNamespaceWorkloads, ni, pi), options...)
			if len(initial) > 0 {
				cb.InitialOptions = initial
			}
			label := l.Namespace
			if total > checkboxMaxOptions {
				label = fmt.Sprintf("%s %d-%d", l.Namespace, start+1, end)
			}
			var hint *slack.TextBlockObject
			if ni == 0 && pi == 0 {
				hint = slack.NewTextBlockObject("plain_text", "✅ = running  ☐ = stopped", false, false)
			}
			block := slack.NewInputBlock(fmt.Sprintf("%s%d_%d", blockNamespaceWorkloads, ni, pi), slack.NewTextBlockObject("plain_text", label, false, false), hint, cb)
			block.Optional = true
			blocks = append(blocks, block)
			start = end
		}
	}
	return blocks
}

// multiModal returns the multi-namespace workload modal, or false when it does not fit the
// Slack limits (the caller then opens the namespace select).
func (b *Bot) multiModal(sub authz.Subject, cl config.Cluster, namespaces []string, channelID, userID string) (slack.ModalViewRequest, bool) {
	listings := b.listNamespaces(sub, cl.Env, namespaces)
	if !multiNamespaceFits(listings) {
		return slack.ModalViewRequest{}, false
	}
	meta := modalMetadata{ChannelID: channelID, UserID: userID, Cluster: cl.Env}
	for _, l := range listings {
		meta.Namespaces = append(meta.Namespaces, nsMeta{Namespace: l.Namespace, Rendered: len(l.Listing.Entries)})
	}
	raw, err := json.Marshal(meta)
	if err != nil || len(raw) > slackPrivateMetadataMaxLen {
		return slack.ModalViewRequest{}, false
	}
	blocks := append(multiNamespaceBlocks(listings), durationInputBlock())
	if !b.svc.ChatStopAllowed() {
		blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", "ℹ️ Stopping from chat is disabled: running workloads are stopped by the nightly job.", false, false), nil, nil))
	}
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      callbackWorkloadModal,
		Title:           slack.NewTextBlockObject("plain_text", modalTitle(cl), false, false),
		Submit:          slack.NewTextBlockObject("plain_text", "Plan", false, false),
		Close:           slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		PrivateMetadata: string(raw),
		Blocks:          slack.Blocks{BlockSet: blocks},
	}, true
}

// selectedByNamespace folds the checked values of the multi-namespace modal per namespace index.
func selectedByNamespace(values map[string]map[string]slack.BlockAction) map[int]map[string]bool {
	out := map[int]map[string]bool{}
	for blockID, actions := range values {
		var ni, pi int
		if _, err := fmt.Sscanf(blockID, blockNamespaceWorkloads+"%d_%d", &ni, &pi); err != nil {
			continue
		}
		for actionID, action := range actions {
			if !strings.HasPrefix(actionID, actionNamespaceWorkloads) {
				continue
			}
			if out[ni] == nil {
				out[ni] = map[string]bool{}
			}
			for _, opt := range action.SelectedOptions {
				out[ni][opt.Value] = true
			}
		}
	}
	return out
}

// previewPart is the plan of one namespace carried from the preview to Apply.
type previewPart struct {
	Namespace string   `json:"n"`
	ToStart   []string `json:"s,omitempty"`
	ToStop    []string `json:"t,omitempty"`
}

// nsPlan is the plan and schedules of one namespace of a multi-namespace submit.
type nsPlan struct {
	plan   frontend.Plan
	starts []frontend.Schedule
}

// planNamespaces plans every namespace of meta from the submitted selection. Only rendered
// entries take part, so entries that appeared after the modal opened are never stopped.
func (b *Bot) planNamespaces(sub authz.Subject, cl config.Cluster, meta modalMetadata, values map[string]map[string]slack.BlockAction, requested time.Duration, requestID string) []nsPlan {
	selected := selectedByNamespace(values)
	plans := make([]nsPlan, 0, len(meta.Namespaces))
	for ni, nm := range meta.Namespaces {
		listing := b.svc.List(sub, cl.Env, nm.Namespace, frontend.Limits{PageSize: checkboxMaxOptions})
		plan := b.svc.Plan(sub, cl, nm.Namespace, frontend.Rendered(listing.Entries, nm.Rendered), selected[ni], requestID)
		plans = append(plans, nsPlan{plan: plan, starts: b.svc.Schedules(cl.Env, nm.Namespace, plan.ToStart, frontend.Uniform(plan.ToStart, requested))})
	}
	return plans
}

// multiPreviewLines renders the plans; several namespaces get a bold namespace line each.
// Namespaces with neither a change nor a refusal are left out.
func multiPreviewLines(plans []nsPlan) []string {
	shown := 0
	for _, p := range plans {
		if len(p.plan.Changes)+len(p.plan.Rejected) > 0 {
			shown++
		}
	}
	var lines []string
	for _, p := range plans {
		if len(p.plan.Changes)+len(p.plan.Rejected) == 0 {
			continue
		}
		if shown > 1 {
			lines = append(lines, "*"+p.plan.Namespace+"*")
		}
		lines = append(lines, markup.PlanLines(p.plan.Changes)...)
		lines = append(lines, markup.ScheduleLines(p.starts)...)
		lines = append(lines, frontend.RejectionLines(p.plan.Rejected)...)
	}
	return lines
}

// namespaceList joins the namespaces of plans that change or refuse something.
func namespaceList(plans []nsPlan) string {
	var names []string
	for _, p := range plans {
		if len(p.plan.Changes)+len(p.plan.Rejected) > 0 {
			names = append(names, p.plan.Namespace)
		}
	}
	return strings.Join(names, ", ")
}

// handleMultiSubmit is handleModalSubmit for the multi-namespace modal.
func (b *Bot) handleMultiSubmit(callback slack.InteractionCallback, req *socketmode.Request, cl config.Cluster, meta modalMetadata, requested time.Duration) {
	sub := b.subject(callback.User.ID)
	requestID := audit.NewRequestID()
	plans := b.planNamespaces(sub, cl, meta, callback.View.State.Values, requested, requestID)

	var starts []frontend.Schedule
	var parts []previewPart
	for _, p := range plans {
		starts = append(starts, p.starts...)
		if len(p.plan.ToStart)+len(p.plan.ToStop) > 0 {
			parts = append(parts, previewPart{Namespace: p.plan.Namespace, ToStart: p.plan.ToStart, ToStop: p.plan.ToStop})
		}
	}
	if errs := durationErrors(starts); errs != nil {
		b.ack(*req, map[string]any{"response_action": "errors", "errors": errs})
		return
	}
	envUpper := strings.ToUpper(cl.Env)
	if len(parts) == 0 {
		b.ack(*req)
		header := fmt.Sprintf("ℹ️ *%s / %s* — no changes", envUpper, cl.Name)
		if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID, slack.MsgOptionBlocks(mrkdwnSections(header, multiPreviewLines(plans))...)); err != nil {
			slog.Error("failed to send no-change message", slog.String("error.message", err.Error()))
		}
		return
	}

	previewMeta, err := json.Marshal(previewMetadata{
		ChannelID: meta.ChannelID, UserID: meta.UserID, Cluster: cl.Env, RequestID: requestID,
		Parts: parts, DurationSec: int64(requested / time.Second),
	})
	if err != nil || len(previewMeta) > slackPrivateMetadataMaxLen {
		b.ack(*req)
		if _, perr := b.client.PostEphemeral(meta.ChannelID, meta.UserID, slack.MsgOptionText("⚠️ Too many changes at once. Please split them into several runs.", false)); perr != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.message", perr.Error()))
		}
		return
	}
	header := fmt.Sprintf("*%s / %s / %s* change preview:\n", envUpper, cl.Name, namespaceList(plans))
	b.ack(*req, map[string]any{
		"response_action": "update",
		"view": slack.ModalViewRequest{
			Type:            slack.VTModal,
			CallbackID:      callbackPreviewModal,
			Title:           slack.NewTextBlockObject("plain_text", "Change preview", false, false),
			Submit:          slack.NewTextBlockObject("plain_text", "✅ Apply", false, false),
			Close:           slack.NewTextBlockObject("plain_text", "Back", false, false),
			PrivateMetadata: string(previewMeta),
			Blocks:          slack.Blocks{BlockSet: mrkdwnSections(header, multiPreviewLines(plans))},
		},
	})
}

// parts returns the per-namespace plans of a preview; older single-namespace previews have one.
func (m previewMetadata) parts() []previewPart {
	if len(m.Parts) > 0 {
		return m.Parts
	}
	return []previewPart{{Namespace: m.Namespace, ToStart: m.ToStart, ToStop: m.ToStop}}
}

// appliedPart is the outcome of the plan of one namespace.
type appliedPart struct {
	req frontend.ApplyRequest
	res frontend.ApplyResult
}

// resultLines renders the outcome of every namespace with the rollout progress in snap.
func resultLines(applied []appliedPart, snap rollout.Snapshot, timeout time.Duration) []string {
	var lines []string
	for _, a := range applied {
		var own rollout.Snapshot
		for _, it := range snap {
			if it.Target.Namespace == a.req.Namespace {
				own = append(own, it)
			}
		}
		if len(applied) > 1 {
			lines = append(lines, "*"+a.req.Namespace+"*")
		}
		lines = append(lines, progressLines(a.res, own, timeout)...)
	}
	return lines
}

func appliedNamespaces(applied []appliedPart) string {
	names := make([]string, 0, len(applied))
	for _, a := range applied {
		names = append(names, a.req.Namespace)
	}
	return strings.Join(names, ", ")
}
