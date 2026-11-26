package slackbot

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/observability/audit"
	"github.com/murasame29/workload-knocker/internal/policy/runlimit"
	"github.com/murasame29/workload-knocker/internal/workload/rollout"
)

const (
	actionSelectNamespace = "select_namespace"
	blockSelectNamespace  = "block_namespace"
	actionSelectWorkloads = "select_workloads"
	blockSelectWorkloads  = "block_workloads"
	blockDuration         = "block_duration"
	actionDuration        = "action_duration"
	callbackWorkloadModal = "workload_modal"
	callbackPreviewModal  = "preview_modal"

	// slackModalTitleMaxLen is the max length of a Slack Modal plain_text title.
	// Exceeding it makes OpenView fail with invalid_arguments.
	slackModalTitleMaxLen = 24

	// checkboxMaxOptions is the max options in a Slack checkboxes element.
	// Exceeding it makes OpenView / UpdateView fail with invalid_arguments, so entries are
	// split across multiple input blocks of this size.
	checkboxMaxOptions = 10

	// slackOptionTextMaxLen is the max length of an option object text.
	slackOptionTextMaxLen = 75

	// slackModalMaxBlocks is the max number of blocks in one Modal.
	slackModalMaxBlocks = 100

	// reservedModalBlocks is the number of blocks reserved besides workload checkboxes
	// (namespace select + duration input + truncation warning section).
	reservedModalBlocks = 3

	// maxWorkloadCheckboxBlocks / maxWorkloadOptions cap checkbox blocks and display entries per Modal.
	// Entries are post-merge units, so the real workload count may be higher.
	maxWorkloadCheckboxBlocks = (slackModalMaxBlocks - reservedModalBlocks) / checkboxMaxOptions
	maxWorkloadOptions        = maxWorkloadCheckboxBlocks * checkboxMaxOptions

	// slackPrivateMetadataMaxLen is the max length of view.private_metadata.
	slackPrivateMetadataMaxLen = 3000

	// slackSectionTextMaxLen is the max length of a section block text.
	slackSectionTextMaxLen = 3000
)

// modalTitle builds the Slack Modal title.
// It uses "🚀 <key>" to fit 24 characters and truncates if it still overflows.
func modalTitle(cl config.Cluster) string {
	title := "🚀 " + cl.Env
	runes := []rune(title)
	if len(runes) <= slackModalTitleMaxLen {
		return title
	}
	return string(runes[:slackModalTitleMaxLen])
}

// listLimits are the Slack Modal limits handed to the frontend service.
var listLimits = frontend.Limits{PageSize: checkboxMaxOptions, MaxEntries: maxWorkloadOptions}

// listLimitsAll lists every entry (resolving submitted values back to workloads).
var listLimitsAll = frontend.Limits{}

// truncateOptionText fits Slack option text within the character limit.
func truncateOptionText(s string) string {
	runes := []rune(s)
	if len(runes) <= slackOptionTextMaxLen {
		return s
	}
	return string(runes[:slackOptionTextMaxLen])
}

// workloadCheckboxBlocks renders one input block per page of entries.
// block_id is blockSelectWorkloads, the render ID and a sequence number (e.g.
// "block_workloads_3f2a9c1e_0"); submit collects them by prefix. Slack keeps the state of an input
// whose block_id survives views.update and ignores its initial_options, so a namespace chosen
// again would show the checks of the previous one: a new render ID per update avoids that.
// The option value is only the representative workload name (option values are capped at 75 chars).
func (u ui) workloadCheckboxBlocks(namespace, render string, listing frontend.Listing) []slack.Block {
	pages := listing.Pages()
	blocks := make([]slack.Block, 0, len(pages))
	total := len(listing.Entries)

	start := 0
	for idx, chunk := range pages {
		end := start + len(chunk)

		options := make([]*slack.OptionBlockObject, 0, len(chunk))
		var initialOptions []*slack.OptionBlockObject
		for _, g := range chunk {
			opt := slack.NewOptionBlockObject(
				g.Value(),
				slack.NewTextBlockObject("plain_text", truncateOptionText(g.Label()), false, false),
				nil,
			)
			options = append(options, opt)
			if g.AnyRunning() {
				initialOptions = append(initialOptions, opt)
			}
		}

		checkboxes := slack.NewCheckboxGroupsBlockElement(
			fmt.Sprintf("%s_%d", actionSelectWorkloads, idx),
			options...,
		)
		if len(initialOptions) > 0 {
			checkboxes.InitialOptions = initialOptions
		}

		// When split, number the label so the block range is clear.
		label := u.t("Workloads (%s)", namespace)
		if total > checkboxMaxOptions {
			label = u.t("Workloads (%s) %d-%d", namespace, start+1, end)
		}

		// Attach the hint only to the first block to reduce vertical redundancy.
		var hint *slack.TextBlockObject
		if idx == 0 {
			hint = slack.NewTextBlockObject("plain_text", u.t("✅ = running  ☐ = stopped"), false, false)
		}

		block := slack.NewInputBlock(
			fmt.Sprintf("%s_%s_%d", blockSelectWorkloads, render, idx),
			slack.NewTextBlockObject("plain_text", label, false, false),
			hint,
			checkboxes,
		)
		block.Optional = true
		blocks = append(blocks, block)
		start = end
	}

	return blocks
}

// durationInputBlock builds the optional run duration input (hours, decimals allowed).
// Empty means the workload default.
func (u ui) durationInputBlock() *slack.InputBlock {
	el := slack.NewNumberInputBlockElement(
		slack.NewTextBlockObject("plain_text", u.t("Empty = workload default"), false, false),
		actionDuration, true,
	).WithMinValue("0.25")
	block := slack.NewInputBlock(
		blockDuration,
		slack.NewTextBlockObject("plain_text", u.t("Run duration (hours)"), false, false),
		slack.NewTextBlockObject("plain_text", u.t("Applies to every workload started now; they stop automatically afterwards. Minimum 0.25 (15 minutes)."), false, false),
		el,
	)
	block.Optional = true
	return block
}

// submittedDuration reads the run duration input; 0 means empty (workload default).
func submittedDuration(values map[string]map[string]slack.BlockAction) (time.Duration, error) {
	return runlimit.ParseHours(values[blockDuration][actionDuration].Value)
}

// slackTime renders t in each viewer's local time zone.
func slackTime(t time.Time) string {
	return fmt.Sprintf("<!date^%d^{date_short_pretty} {time}|%s>", t.Unix(), t.UTC().Format("2006-01-02 15:04 UTC"))
}

// durationErrors maps the duration input block to the first start whose duration is out of range.
func durationErrors(starts []frontend.Schedule) map[string]string {
	for _, s := range starts {
		if s.Err != nil {
			return map[string]string{blockDuration: fmt.Sprintf("%s: %v", s.Name, s.Err)}
		}
	}
	return nil
}

// newRenderID returns a short random ID that makes the block_ids of one render unique.
func newRenderID() string { return strconv.FormatUint(rand.Uint64()&0xffffffff, 16) }

// selectedWorkloadNames folds the selection of all split checkbox blocks into one set.
// It scans by prefix, so submissions from the old single block ("block_workloads") also work.
func selectedWorkloadNames(values map[string]map[string]slack.BlockAction) map[string]bool {
	selected := make(map[string]bool)
	for blockID, actions := range values {
		if !strings.HasPrefix(blockID, blockSelectWorkloads) {
			continue
		}
		for actionID, action := range actions {
			if !strings.HasPrefix(actionID, actionSelectWorkloads) {
				continue
			}
			for _, opt := range action.SelectedOptions {
				selected[opt.Value] = true
			}
		}
	}
	return selected
}

// mrkdwnSections splits header + lines into sections within the section text limit (3000 chars).
// A single line cannot exceed the limit because workload names are capped (K8s limit 253 chars).
func mrkdwnSections(header string, lines []string) []slack.Block {
	var blocks []slack.Block
	current := header
	for _, line := range lines {
		candidate := current + "\n" + line
		if len(candidate) > slackSectionTextMaxLen {
			blocks = append(blocks, slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", current, false, false), nil, nil,
			))
			current = line
			continue
		}
		current = candidate
	}
	if current != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", current, false, false), nil, nil,
		))
	}
	return blocks
}

// modalMetadata is the information embedded in the Modal private_metadata.
// Cluster holds the cluster.Key resolved from the Slack slash command.
type modalMetadata struct {
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace,omitempty"`
	// Rendered is the number of workloads actually rendered as checkboxes.
	// It restores the diff scope so workloads truncated by the display limit are not
	// mistaken for "unchecked = stopped" on submit.
	Rendered int `json:"rendered,omitempty"`
	// Namespaces is set by the multi-namespace modal (one checkbox group per namespace).
	Namespaces []nsMeta `json:"namespaces,omitempty"`
}

// previewMetadata is the private_metadata of the preview Modal.
// ToStart / ToStop hold only workload names (cluster / namespace are fixed by the same struct).
// private_metadata is capped at 3000 chars, which full "cluster/namespace/name" tuples would break.
type previewMetadata struct {
	ChannelID string   `json:"channel_id"`
	UserID    string   `json:"user_id"`
	Cluster   string   `json:"cluster"`
	Namespace string   `json:"namespace"`
	ToStart   []string `json:"to_start"` // workload name
	ToStop    []string `json:"to_stop"`  // workload name
	// RequestID ties the preview and the apply of one operation in the audit log.
	RequestID string `json:"request_id"`
	// DurationSec is the requested run duration in seconds; 0 selects each workload's default.
	DurationSec int64 `json:"duration_sec,omitempty"`
	// Parts are the plans per namespace of the multi-namespace modal; empty for a single namespace.
	Parts []previewPart `json:"parts,omitempty"`
}

// applyLines renders the outcome of an apply, including entries refused at apply time.
func (u ui) applyLines(res frontend.ApplyResult) []string { return u.progressLines(res, nil, 0) }

// progressLines is applyLines with the rollout progress of the applied workloads.
func (u ui) progressLines(res frontend.ApplyResult, snap rollout.Snapshot, timeout time.Duration) []string {
	progress := make(map[string]rollout.Item, len(snap))
	for _, it := range snap {
		progress[it.Target.Name] = it
	}
	var lines []string
	for _, a := range res.Applied {
		if it, ok := progress[a.Name]; ok && a.Found {
			lines = append(lines, u.markup.RolloutLine(it, timeout))
			continue
		}
		if a.Action == frontend.ActionStart {
			lines = append(lines, u.t("🟢 %s → start", a.Name))
		} else {
			lines = append(lines, u.t("🔴 %s → stop", a.Name))
		}
	}
	lines = append(lines, u.markup.AppliedStopLines(res.Applied)...)
	return append(lines, u.markup.RejectionLines(res.Rejected)...)
}

// modalNote explains an empty list or a disabled chat stop; "" when there is nothing to say.
func (u ui) modalNote(listing frontend.Listing, chatStopAllowed bool) string {
	var notes []string
	if listing.Total() == 0 {
		notes = append(notes, u.t("ℹ️ You are not authorized to operate any workload in this namespace."))
	}
	if !chatStopAllowed {
		notes = append(notes, u.t("ℹ️ Stopping from chat is disabled: running workloads are stopped by the nightly job."))
	}
	return strings.Join(notes, "\n")
}

// handleSlashCommand handles /workload-knocker-*: no argument opens the menu modal, status and
// schedule open their flow directly. The cluster is resolved via b.registry.BySlashCommand(cmd.Command).
func (b *Bot) handleSlashCommand(cmd slack.SlashCommand) {
	cl, ok := b.registry.BySlashCommand(cmd.Command)
	if !ok {
		slog.Warn("unknown slash command", slog.String("command", cmd.Command))
		if _, err := b.client.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(b.t("⚠️ This slash command is not registered in the Cluster Registry"), false),
		); err != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()))
		}
		return
	}

	arg := parseSlashArg(cmd.Text)
	slog.Info("slash command received", slog.String("user.name", cmd.UserName), slog.String("command", cmd.Command), slog.Bool("status", arg == argStatus), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env))

	switch arg {
	case argStatus:
		b.handleStatus(cmd, cl)
		return
	case argSchedule:
		b.handleSchedule(cmd, cl)
		return
	case argUnknown:
		b.postUsage(cmd)
		return
	}

	meta, _ := json.Marshal(modalMetadata{ChannelID: cmd.ChannelID, UserID: cmd.UserID, Cluster: cl.Env})
	b.openView(cmd.TriggerID, b.menuModal(cl, string(meta)))
}

// openView opens a modal from a slash command (trigger_id is valid for 3 seconds).
func (b *Bot) openView(triggerID string, view slack.ModalViewRequest) {
	if _, err := b.views.OpenView(triggerID, view); err != nil {
		slog.Error("failed to open Modal", slog.String("view.callback_id", view.CallbackID), slog.String("error.message", err.Error()))
	}
}

// handleInteraction handles Slack interactions (other than view_submission).
func (b *Bot) handleInteraction(callback slack.InteractionCallback) {
	switch callback.Type {
	case slack.InteractionTypeBlockActions:
		slog.Debug("block actions received", slog.Int("count", len(callback.ActionCallback.BlockActions)))
		for _, action := range callback.ActionCallback.BlockActions {
			slog.Debug("block action", slog.String("action.id", action.ActionID), slog.String("block.id", action.BlockID), slog.String("selected.value", action.SelectedOption.Value))
			switch action.ActionID {
			case actionMenuStartStop, actionMenuSchedule, actionMenuStatus:
				b.handleMenu(callback, action.ActionID)
			case actionScheduleNamespace:
				b.handleScheduleNamespace(callback, action.SelectedOption.Value)
			case actionStatusNamespace:
				b.handleStatusNamespace(callback, action.SelectedOption.Value)
			case actionSelectNamespace:
				b.handleNamespaceSelected(callback, action.SelectedOption.Value)
			case actionExtend:
				b.handleExtend(callback, action.Value)
			case actionStatusExtend, actionStatusStop:
				b.handleStatusAction(callback, action.ActionID, action.Value)
			case actionReservationStop:
				b.handleReservationCancel(callback, action.Value)
			}
		}
	default:
		slog.Debug("unhandled interaction type", slog.String("callback.type", string(callback.Type)))
	}
}

// handleViewSubmission handles view_submission events.
// In Socket Mode the Ack payload carries response_action to control the modal.
func (b *Bot) handleViewSubmission(callback slack.InteractionCallback, req *socketmode.Request) {
	slog.Debug("view submission dispatched", slog.String("view.callback_id", callback.View.CallbackID), slog.String("view.id", callback.View.ID))
	switch callback.View.CallbackID {
	case callbackWorkloadModal:
		b.handleModalSubmit(callback, req)
	case callbackPreviewModal:
		// Apply: close the modal with an empty Ack, then post the result to the channel
		b.ack(*req)
		b.handlePreviewConfirm(callback)
	case callbackScheduleModal:
		b.handleScheduleSubmit(callback, req)
	default:
		slog.Warn("unknown view submission callback id", slog.String("view.callback_id", callback.View.CallbackID))
		b.ack(*req)
	}
}

// handleNamespaceSelected shows the workload checkboxes after a namespace is selected.
// Running workloads become the initial_options selection.
func (b *Bot) handleNamespaceSelected(callback slack.InteractionCallback, namespace string) {
	// Extract the cluster from private_metadata and resolve it via the registry.
	var meta modalMetadata
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil {
		slog.Error("failed to parse private_metadata", slog.String("error.message", err.Error()))
		return
	}

	cl, ok := b.registry.ByEnv(meta.Cluster)
	if !ok {
		// The cluster may have been removed by a registry hot reload while the modal was open.
		slog.Warn("cluster not found at namespace selection")
		return
	}

	slog.Info("namespace selected", slog.String("user.name", callback.User.Name), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env), slog.String("namespace", namespace))

	sub := b.subject(callback.User.ID)

	// Entries beyond the Modal block limit cannot be shown, so the service cuts them off; warn.
	listing := b.svc.List(sub, cl.Env, namespace, listLimits)
	truncated := listing.Truncated
	if truncated > 0 {
		slog.Warn("display entries exceed the Modal limit; some are hidden",
			slog.String("cluster.env", cl.Env), slog.String("namespace", namespace),
			slog.Int("limit", maxWorkloadOptions), slog.Int("truncated", truncated))
	}

	// namespace select (keep the selected state; sorted a-z)
	visibleNamespaces, _ := b.svc.Namespaces(sub, cl.Env)
	nsBlock := b.namespacePicker(actionSelectNamespace, visibleNamespaces, namespace)

	// workload checkboxes (running ones are initially selected; sorted a-z)
	// Slack checkboxes allow 10 options max, so split across multiple blocks.
	blocks := make([]slack.Block, 0, 2+maxWorkloadCheckboxBlocks)
	blocks = append(blocks, nsBlock)
	blocks = append(blocks, b.workloadCheckboxBlocks(namespace, newRenderID(), listing)...)
	if len(listing.Entries) > 0 {
		blocks = append(blocks, b.durationInputBlock())
	}

	if note := b.modalNote(listing, b.svc.ChatStopAllowed()); note != "" {
		blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", note, false, false), nil, nil))
	}

	if truncated > 0 {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", b.t(
				":warning: There are %d targets, exceeding the Slack Modal display limit (%d). The remaining %d cannot be changed on this screen.",
				listing.Total(), maxWorkloadOptions, truncated,
			), false, false),
			nil, nil,
		))
	}

	// Add namespace and rendered count (post-merge entries) to private_metadata (keep cluster)
	meta.Namespace = namespace
	meta.Rendered = len(listing.Entries)
	metaBytes, _ := json.Marshal(meta)

	modal := slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      callbackWorkloadModal,
		Title:           slack.NewTextBlockObject("plain_text", modalTitle(cl), false, false),
		Close:           slack.NewTextBlockObject("plain_text", b.t("Cancel"), false, false),
		PrivateMetadata: string(metaBytes),
		Blocks: slack.Blocks{
			BlockSet: blocks,
		},
	}
	// Slack requires a submit button with input blocks, and allows none without them.
	if len(listing.Entries) > 0 {
		modal.Submit = slack.NewTextBlockObject("plain_text", b.t("Plan"), false, false)
	}

	b.updateView(callback, modal)
}

// handleModalSubmit computes the diff on Plan and shows the preview Modal.
// In Socket Mode view_submission, the Ack payload with response_action: update
// switches the same modal to the preview.
func (b *Bot) handleModalSubmit(callback slack.InteractionCallback, req *socketmode.Request) {
	var meta modalMetadata
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil {
		slog.Error("failed to parse private_metadata", slog.String("error.message", err.Error()))
		b.ack(*req)
		return
	}

	cl, ok := b.registry.ByEnv(meta.Cluster)
	if !ok {
		slog.Warn("cluster not found at plan submit")
		b.ack(*req)
		if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
			slack.MsgOptionText(b.t("⚠️ The target Cluster is no longer in the Registry. Please run the command again."), false),
		); err != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.message", err.Error()))
		}
		return
	}

	requested, err := submittedDuration(callback.View.State.Values)
	if err != nil {
		b.ack(*req, map[string]any{"response_action": "errors", "errors": map[string]string{blockDuration: err.Error()}})
		return
	}
	if len(meta.Namespaces) > 0 {
		b.handleMultiSubmit(callback, req, cl, meta, requested)
		return
	}

	// Selected workloads (checked = wants running), folded across all split checkbox blocks.
	selectedSet := selectedWorkloadNames(callback.View.State.Values)

	// Compare with the current state; rebuild with the same order and merge unit as render.
	// Entries not rendered in the Modal are excluded from the diff; otherwise
	// "not shown = unchecked = stop" would stop running workloads that are off screen.
	sub := b.subject(callback.User.ID)
	listing := b.svc.List(sub, cl.Env, meta.Namespace, frontend.Limits{PageSize: checkboxMaxOptions})
	requestID := audit.NewRequestID()
	plan := b.svc.Plan(sub, cl, meta.Namespace, frontend.Rendered(listing.Entries, meta.Rendered), selectedSet, requestID)
	toStart, toStop := plan.ToStart, plan.ToStop

	starts := b.svc.Schedules(cl.Env, meta.Namespace, toStart, frontend.Uniform(toStart, requested))
	if errs := durationErrors(starts); errs != nil {
		b.ack(*req, map[string]any{"response_action": "errors", "errors": errs})
		return
	}

	previewLines := append(b.foreignStopLines(plan.Changes, frontend.Rendered(listing.Entries, meta.Rendered), viewerOf(callback.User)), b.markup.PlanLines(plan.Changes)...)
	previewLines = append(previewLines, b.markup.ScheduleLines(starts)...)
	rejectedLines := b.markup.RejectionLines(plan.Rejected)

	envUpper := strings.ToUpper(cl.Env)

	if len(toStart) == 0 && len(toStop) == 0 {
		// Nothing allowed to change: close the modal with an empty Ack and send an ephemeral notice
		b.ack(*req)
		header := b.t("ℹ️ *%s / %s / %s* — no changes", envUpper, cl.Name, meta.Namespace)
		if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
			slack.MsgOptionBlocks(mrkdwnSections(header, rejectedLines)...),
		); err != nil {
			slog.Error("failed to send no-change message", slog.String("error.message", err.Error()))
		}
		return
	}

	previewMeta, err := json.Marshal(previewMetadata{
		ChannelID: meta.ChannelID,
		UserID:    meta.UserID,
		Cluster:   cl.Env,
		Namespace: meta.Namespace,
		ToStart:   toStart,
		ToStop:    toStop,
		RequestID: requestID,

		DurationSec: int64(requested / time.Second),
	})
	// private_metadata is capped at 3000 chars. If the diff is too large the preview cannot
	// open, so abort and ask the user to split the run.
	if err != nil || len(previewMeta) > slackPrivateMetadataMaxLen {
		slog.Warn("preview metadata exceeds the private_metadata limit",
			slog.Int("size", len(previewMeta)), slog.Int("limit", slackPrivateMetadataMaxLen),
			slog.Int("to_start", len(toStart)), slog.Int("to_stop", len(toStop)))
		b.ack(*req)
		if _, perr := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
			slack.MsgOptionText(b.t(
				"⚠️ Too many changes at once (start %d / stop %d). Please split them into several runs.",
				len(toStart), len(toStop)), false),
		); perr != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.message", perr.Error()))
		}
		return
	}

	// Show the preview Modal with response_action: update
	previewHeader := b.t("*%s / %s / %s* change preview:", envUpper, cl.Name, meta.Namespace) + "\n"

	previewModal := slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      callbackPreviewModal,
		Title:           slack.NewTextBlockObject("plain_text", b.t("Change preview"), false, false),
		Submit:          slack.NewTextBlockObject("plain_text", b.t("✅ Apply"), false, false),
		Close:           slack.NewTextBlockObject("plain_text", b.t("Back"), false, false),
		PrivateMetadata: string(previewMeta),
		Blocks: slack.Blocks{
			BlockSet: mrkdwnSections(previewHeader, append(previewLines, rejectedLines...)),
		},
	}

	// Switch the modal by including response_action: update in the Ack payload
	b.ack(*req, map[string]any{
		"response_action": "update",
		"view":            previewModal,
	})
}

// handlePreviewConfirm runs the actual start/stop on the preview Apply button.
func (b *Bot) handlePreviewConfirm(callback slack.InteractionCallback) {
	var meta previewMetadata
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil {
		slog.Error("failed to parse preview metadata", slog.String("error.message", err.Error()))
		return
	}

	// Do nothing if the cluster was removed from the registry by a hot reload.
	cl, ok := b.registry.ByEnv(meta.Cluster)
	if !ok {
		slog.Warn("cluster not found in registry at apply time")
		if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
			slack.MsgOptionText(b.t("⚠️ The target Cluster is no longer in the Registry. Please run the command again."), false),
		); err != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.message", err.Error()))
		}
		return
	}

	envUpper := strings.ToUpper(cl.Env)
	sub := b.subject(callback.User.ID)
	duration := time.Duration(meta.DurationSec) * time.Second

	// One request per namespace keeps the audit, notify and REST semantics per namespace.
	var applied []appliedPart
	var targets []rollout.Target
	for _, p := range meta.parts() {
		req := frontend.ApplyRequest{
			Cluster: cl, Namespace: p.Namespace, User: callback.User.Name, Subject: sub, RequestID: meta.RequestID,
			ToStart: p.ToStart, ToStop: p.ToStop, Durations: frontend.Uniform(p.ToStart, duration),
		}
		res := b.svc.Apply(req)
		applied = append(applied, appliedPart{req: req, res: res})
		targets = append(targets, b.svc.RolloutTargets(req, res)...)
	}
	tracked := b.svc.RolloutEnabled() && len(targets) > 0
	timeout := b.svc.RolloutTimeout()
	var initial rollout.Snapshot
	if tracked {
		initial = rollout.InitialSnapshot(targets)
	}

	resultHeader := b.t("🚀 <@%s> changed workloads (%s / %s / %s)",
		callback.User.ID, envUpper, cl.Name, appliedNamespaces(applied),
	)
	if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
		slack.MsgOptionBlocks(mrkdwnSections(resultHeader, b.resultLines(applied, initial, timeout))...),
	); err != nil {
		slog.Error("failed to send result message", slog.String("error.message", err.Error()))
	}
	if tracked {
		b.watchRollout(meta, applied, targets, timeout)
	}
}

// watchRollout follows the rollout in the background. Slack cannot edit an ephemeral message
// posted with chat.postEphemeral, so only the final state is posted as a second message.
func (b *Bot) watchRollout(meta previewMetadata, applied []appliedPart, targets []rollout.Target, timeout time.Duration) {
	b.watchers.Add(1)
	go func() {
		defer b.watchers.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in slack rollout watch", slog.Any("recover", r), slog.String("stack", string(debug.Stack())))
			}
		}()
		renderers := make(map[string]*frontend.ReadyRenderer, len(applied))
		for _, a := range applied {
			renderers[a.req.Namespace] = b.svc.NewReadyRenderer(a.req)
		}
		b.svc.Track(b.watchCtx, targets, func(snap rollout.Snapshot) {
			if !snap.Done() {
				return
			}
			for idx := range snap {
				if r := renderers[snap[idx].Target.Namespace]; r != nil {
					r.Decorate(snap[idx : idx+1])
				}
			}
			header := b.t("📡 Rollout result (%s / %s)", strings.ToUpper(targets[0].Env), appliedNamespaces(applied))
			if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
				slack.MsgOptionBlocks(mrkdwnSections(header, b.resultLines(applied, snap, timeout))...),
			); err != nil {
				slog.Error("failed to send rollout result message", slog.String("error.message", err.Error()))
			}
		})
	}()
}
