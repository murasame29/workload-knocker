package slackbot

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
)

const (
	actionSelectNamespace = "select_namespace"
	blockSelectNamespace  = "block_namespace"
	actionSelectWorkloads = "select_workloads"
	blockSelectWorkloads  = "block_workloads"
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
	// (namespace select + truncation warning section).
	reservedModalBlocks = 2

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

// truncateOptionText fits Slack option text within the character limit.
func truncateOptionText(s string) string {
	runes := []rune(s)
	if len(runes) <= slackOptionTextMaxLen {
		return s
	}
	return string(runes[:slackOptionTextMaxLen])
}

// namespaceInputBlock builds the namespace static_select block.
// Options are sorted a-z (the store returns map order).
// A non-empty selected that is in the list becomes the initial selection.
func namespaceInputBlock(namespaces []string, selected string) *slack.InputBlock {
	sorted := slices.Clone(namespaces)
	slices.Sort(sorted)

	options := make([]*slack.OptionBlockObject, 0, len(sorted))
	var initial *slack.OptionBlockObject
	for _, ns := range sorted {
		opt := slack.NewOptionBlockObject(
			ns,
			slack.NewTextBlockObject("plain_text", ns, false, false),
			nil,
		)
		options = append(options, opt)
		if ns == selected {
			initial = opt
		}
	}

	nsSelect := slack.NewOptionsSelectBlockElement(
		"static_select",
		slack.NewTextBlockObject("plain_text", "Select a namespace", false, false),
		actionSelectNamespace,
		options...,
	)
	if initial != nil {
		nsSelect.InitialOption = initial
	}

	block := slack.NewInputBlock(
		blockSelectNamespace,
		slack.NewTextBlockObject("plain_text", "Namespace", false, false),
		nil,
		nsSelect,
	)
	block.DispatchAction = true
	return block
}

// workloadCheckboxBlocks renders one input block per page of entries.
// block_id / action_id are blockSelectWorkloads / actionSelectWorkloads plus a sequence number
// (e.g. "block_workloads_0"); submit collects them by prefix. The option value is only the
// representative workload name (option values are capped at 75 chars, so no full tuple).
func workloadCheckboxBlocks(namespace string, listing frontend.Listing) []slack.Block {
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
		label := fmt.Sprintf("Workloads (%s)", namespace)
		if total > checkboxMaxOptions {
			label = fmt.Sprintf("Workloads (%s) %d-%d", namespace, start+1, end)
		}

		// Attach the hint only to the first block to reduce vertical redundancy.
		var hint *slack.TextBlockObject
		if idx == 0 {
			hint = slack.NewTextBlockObject("plain_text", "✅ = running  ☐ = stopped", false, false)
		}

		block := slack.NewInputBlock(
			fmt.Sprintf("%s_%d", blockSelectWorkloads, idx),
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
}

// planLines renders the allowed plan changes for the preview modal.
func planLines(plan frontend.Plan) []string {
	lines := make([]string, 0, len(plan.Changes))
	for _, c := range plan.Changes {
		if c.Action == frontend.ActionStart {
			lines = append(lines, fmt.Sprintf("🟢 %s: stopped → *start*", c.Label))
		} else {
			lines = append(lines, fmt.Sprintf("🔴 %s: running → *stop*", c.Label))
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

// applyLines renders the outcome of an apply, including entries refused at apply time.
func applyLines(res frontend.ApplyResult) []string {
	var lines []string
	for _, a := range res.Applied {
		if a.Action == frontend.ActionStart {
			lines = append(lines, fmt.Sprintf("🟢 %s → start", a.Name))
		} else {
			lines = append(lines, fmt.Sprintf("🔴 %s → stop", a.Name))
		}
	}
	return append(lines, rejectionLines(res.Rejected)...)
}

// modalNote explains an empty list or a disabled chat stop; "" when there is nothing to say.
func modalNote(listing frontend.Listing, chatStopAllowed bool) string {
	var notes []string
	if listing.Total() == 0 {
		notes = append(notes, "ℹ️ You are not authorized to operate any workload in this namespace.")
	}
	if !chatStopAllowed {
		notes = append(notes, "ℹ️ Stopping from chat is disabled: running workloads are stopped by the nightly job.")
	}
	return strings.Join(notes, "\n")
}

// handleSlashCommand handles /workload-knocker-*.
// The cluster is resolved via b.registry.BySlashCommand(cmd.Command).
func (b *Bot) handleSlashCommand(cmd slack.SlashCommand) {
	cl, ok := b.registry.BySlashCommand(cmd.Command)
	if !ok {
		slog.Warn("unknown slash command", slog.String("command", cmd.Command))
		if _, err := b.client.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText("⚠️ This slash command is not registered in the Cluster Registry", false),
		); err != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()))
		}
		return
	}

	slog.Info("slash command received", slog.String("user.name", cmd.UserName), slog.String("command", cmd.Command), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env))

	// Resolve the subject while reconciling so both fit the 3s trigger_id window.
	subCh := make(chan authz.Subject, 1)
	go func() { subCh <- b.subject(cmd.UserID) }()

	// Reflect the latest ScaledObject state on demand.
	b.svc.Reconcile(cl.Env)

	namespaces, total := b.svc.Namespaces(<-subCh, cl.Env)

	// If no namespace is visible, return an error without opening the Modal
	if len(namespaces) == 0 {
		text := "⚠️ No managed workloads are registered yet. Please wait for ScaledObject discovery."
		if total > 0 {
			text = "⚠️ You are not authorized to operate any workload in this cluster."
		}
		if _, err := b.client.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(text, false),
		); err != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()))
		}
		return
	}

	nsBlock := namespaceInputBlock(namespaces, "")

	meta, _ := json.Marshal(modalMetadata{
		ChannelID: cmd.ChannelID,
		UserID:    cmd.UserID,
		Cluster:   cl.Env,
	})

	modal := slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      callbackWorkloadModal,
		Title:           slack.NewTextBlockObject("plain_text", modalTitle(cl), false, false),
		Submit:          slack.NewTextBlockObject("plain_text", "Plan", false, false),
		Close:           slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		PrivateMetadata: string(meta),
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{nsBlock},
		},
	}

	if _, err := b.client.OpenView(cmd.TriggerID, modal); err != nil {
		slog.Error("failed to open Modal", slog.String("error.message", err.Error()))
	}
}

// handleInteraction handles Slack interactions (other than view_submission).
func (b *Bot) handleInteraction(callback slack.InteractionCallback) {
	switch callback.Type {
	case slack.InteractionTypeBlockActions:
		slog.Debug("block actions received", slog.Int("count", len(callback.ActionCallback.BlockActions)))
		for _, action := range callback.ActionCallback.BlockActions {
			slog.Debug("block action", slog.String("action.id", action.ActionID), slog.String("block.id", action.BlockID), slog.String("selected.value", action.SelectedOption.Value))
			if action.ActionID == actionSelectNamespace {
				b.handleNamespaceSelected(callback, action.SelectedOption.Value)
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
	nsBlock := namespaceInputBlock(visibleNamespaces, namespace)

	// workload checkboxes (running ones are initially selected; sorted a-z)
	// Slack checkboxes allow 10 options max, so split across multiple blocks.
	blocks := make([]slack.Block, 0, 2+maxWorkloadCheckboxBlocks)
	blocks = append(blocks, nsBlock)
	blocks = append(blocks, workloadCheckboxBlocks(namespace, listing)...)

	if note := modalNote(listing, b.svc.ChatStopAllowed()); note != "" {
		blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", note, false, false), nil, nil))
	}

	if truncated > 0 {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf(
				":warning: There are %d targets, exceeding the Slack Modal display limit (%d). "+
					"The remaining %d cannot be changed on this screen.",
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
		Submit:          slack.NewTextBlockObject("plain_text", "Plan", false, false),
		Close:           slack.NewTextBlockObject("plain_text", "Cancel", false, false),
		PrivateMetadata: string(metaBytes),
		Blocks: slack.Blocks{
			BlockSet: blocks,
		},
	}

	if _, err := b.client.UpdateView(modal, callback.View.ExternalID, callback.View.Hash, callback.View.ID); err != nil {
		slog.Error("failed to update Modal", slog.String("error.message", err.Error()))
	}
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
			slack.MsgOptionText("⚠️ The target Cluster is no longer in the Registry. Please run the command again.", false),
		); err != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.message", err.Error()))
		}
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

	previewLines := planLines(plan)
	rejectedLines := rejectionLines(plan.Rejected)

	envUpper := strings.ToUpper(cl.Env)

	if len(toStart) == 0 && len(toStop) == 0 {
		// Nothing allowed to change: close the modal with an empty Ack and send an ephemeral notice
		b.ack(*req)
		header := fmt.Sprintf("ℹ️ *%s / %s / %s* — no changes", envUpper, cl.Name, meta.Namespace)
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
	})
	// private_metadata is capped at 3000 chars. If the diff is too large the preview cannot
	// open, so abort and ask the user to split the run.
	if err != nil || len(previewMeta) > slackPrivateMetadataMaxLen {
		slog.Warn("preview metadata exceeds the private_metadata limit",
			slog.Int("size", len(previewMeta)), slog.Int("limit", slackPrivateMetadataMaxLen),
			slog.Int("to_start", len(toStart)), slog.Int("to_stop", len(toStop)))
		b.ack(*req)
		if _, perr := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
			slack.MsgOptionText(fmt.Sprintf(
				"⚠️ Too many changes at once (start %d / stop %d). Please split them into several runs.",
				len(toStart), len(toStop)), false),
		); perr != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.message", perr.Error()))
		}
		return
	}

	// Show the preview Modal with response_action: update
	previewHeader := fmt.Sprintf("*%s / %s / %s* change preview:\n",
		envUpper, cl.Name, meta.Namespace,
	)

	previewModal := slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      callbackPreviewModal,
		Title:           slack.NewTextBlockObject("plain_text", "Change preview", false, false),
		Submit:          slack.NewTextBlockObject("plain_text", "✅ Apply", false, false),
		Close:           slack.NewTextBlockObject("plain_text", "Back", false, false),
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
			slack.MsgOptionText("⚠️ The target Cluster is no longer in the Registry. Please run the command again.", false),
		); err != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.message", err.Error()))
		}
		return
	}

	envUpper := strings.ToUpper(cl.Env)
	user := callback.User.Name

	res := b.svc.Apply(frontend.ApplyRequest{
		Cluster:   cl,
		Namespace: meta.Namespace,
		User:      user,
		Subject:   b.subject(callback.User.ID),
		RequestID: meta.RequestID,
		ToStart:   meta.ToStart,
		ToStop:    meta.ToStop,
	})
	resultLines := applyLines(res)

	resultHeader := fmt.Sprintf("🚀 <@%s> changed workloads (%s / %s / %s)",
		callback.User.ID, envUpper, cl.Name, meta.Namespace,
	)
	if res.AutoStop {
		resultLines = append(resultLines, "⏰ Auto-stop: today 22:00 JST")
	}

	if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
		slack.MsgOptionBlocks(mrkdwnSections(resultHeader, resultLines)...),
	); err != nil {
		slog.Error("failed to send result message", slog.String("error.message", err.Error()))
	}
}
