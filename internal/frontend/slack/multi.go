package slackbot

import (
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
	"github.com/murasame29/workload-knocker/internal/workload/rollout"
)

// Block and action ID prefixes of the namespace checkbox groups of the schedule form, and of the
// multi-namespace start/stop modal of older versions: <prefix><namespace index>_<page>.
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

// multiNamespaceBlocks renders one checkbox input per page of each namespace, labelled with the
// namespace; running entries are checked. Option values are the representative workload names.
func (u ui) multiNamespaceBlocks(listings []nsListing) []slack.Block {
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
				hint = slack.NewTextBlockObject("plain_text", u.t("✅ = running  ☐ = stopped"), false, false)
			}
			block := slack.NewInputBlock(fmt.Sprintf("%s%d_%d", blockNamespaceWorkloads, ni, pi), slack.NewTextBlockObject("plain_text", label, false, false), hint, cb)
			block.Optional = true
			blocks = append(blocks, block)
			start = end
		}
	}
	return blocks
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
func (u ui) multiPreviewLines(plans []nsPlan) []string {
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
		lines = append(lines, u.markup.PlanLines(p.plan.Changes)...)
		lines = append(lines, u.markup.ScheduleLines(p.starts)...)
		lines = append(lines, u.markup.RejectionLines(p.plan.Rejected)...)
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

// handleMultiSubmit is handleModalSubmit for the multi-namespace modal that older versions opened;
// it keeps a modal that was open during an upgrade working.
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
		header := b.t("ℹ️ *%s / %s* — no changes", envUpper, cl.Name)
		if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID, slack.MsgOptionBlocks(mrkdwnSections(header, b.multiPreviewLines(plans))...)); err != nil {
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
		if _, perr := b.client.PostEphemeral(meta.ChannelID, meta.UserID, slack.MsgOptionText(b.t("⚠️ Too many changes at once. Please split them into several runs."), false)); perr != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.message", perr.Error()))
		}
		return
	}
	header := b.t("*%s / %s / %s* change preview:", envUpper, cl.Name, namespaceList(plans)) + "\n"
	b.ack(*req, map[string]any{
		"response_action": "update",
		"view": slack.ModalViewRequest{
			Type:            slack.VTModal,
			CallbackID:      callbackPreviewModal,
			Title:           slack.NewTextBlockObject("plain_text", b.t("Change preview"), false, false),
			Submit:          slack.NewTextBlockObject("plain_text", b.t("✅ Apply"), false, false),
			Close:           slack.NewTextBlockObject("plain_text", b.t("Back"), false, false),
			PrivateMetadata: string(previewMeta),
			Blocks:          slack.Blocks{BlockSet: mrkdwnSections(header, b.multiPreviewLines(plans))},
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
func (u ui) resultLines(applied []appliedPart, snap rollout.Snapshot, timeout time.Duration) []string {
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
		lines = append(lines, u.progressLines(a.res, own, timeout)...)
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
