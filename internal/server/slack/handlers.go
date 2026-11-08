package slackbot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/model"
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

// sortedWorkloads returns a copy of workloads sorted by name.
// The store returns map iteration order, which would make checkbox split boundaries and the
// truncation target differ between render and submit; fixing the order keeps them identical.
//
// groupWorkloads fixes the order of whole entries by Key; this function orders members
// inside a merged entry (the suffix display order).
func sortedWorkloads(ws []*model.Workload) []*model.Workload {
	out := slices.Clone(ws)
	slices.SortFunc(out, func(a, b *model.Workload) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// workloadLabel returns the label of a standalone workload checkbox.
func workloadLabel(w *model.Workload) string {
	if w.Replicas > 0 {
		return fmt.Sprintf("🟢 %s (%d pod)", w.Name, w.Replicas)
	}
	return fmt.Sprintf("🔴 %s", w.Name)
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

// workloadCheckboxBlocks splits display entries into input blocks of checkboxMaxOptions each.
// block_id / action_id are blockSelectWorkloads / actionSelectWorkloads plus a sequence number
// (e.g. "block_workloads_0"); submit collects them by prefix. The option value is only the
// representative workload name (option values are capped at 75 chars, so no full tuple).
func workloadCheckboxBlocks(namespace string, groups []workloadGroup) []slack.Block {
	blocks := make([]slack.Block, 0, (len(groups)+checkboxMaxOptions-1)/checkboxMaxOptions)

	for start := 0; start < len(groups); start += checkboxMaxOptions {
		end := min(start+checkboxMaxOptions, len(groups))
		chunk := groups[start:end]
		idx := start / checkboxMaxOptions

		options := make([]*slack.OptionBlockObject, 0, len(chunk))
		var initialOptions []*slack.OptionBlockObject
		for _, g := range chunk {
			opt := slack.NewOptionBlockObject(
				g.Value(),
				slack.NewTextBlockObject("plain_text", g.label(), false, false),
				nil,
			)
			options = append(options, opt)
			if g.anyRunning() {
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
		if len(groups) > checkboxMaxOptions {
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
}

// handleSlashCommand handles /workload-knocker-*.
// The cluster is resolved via b.registry.BySlashCommand(cmd.Command).
func (b *Bot) handleSlashCommand(cmd slack.SlashCommand) {
	txn := b.startTransaction("SlackBot/SlashCommand")
	if txn != nil {
		defer txn.End()
		txn.AddAttribute("user.name", cmd.UserName)
		txn.AddAttribute("command", cmd.Command)
	}

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

	if txn != nil {

		txn.AddAttribute("cluster.name", cl.Name)
		txn.AddAttribute("cluster.env", cl.Env)
	}

	slog.Info("slash command received", slog.String("user.name", cmd.UserName), slog.String("command", cmd.Command), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env))

	// Reflect the latest ScaledObject state on demand.
	// On failure, continue with the cached registry.
	reconcileCtx, cancel := context.WithTimeout(context.Background(), b.cfg.ClusterReconcileTimeout)
	defer cancel()
	if err := b.store.Reconcile(reconcileCtx, cl.Env); err != nil {
		slog.Warn("ScaledObject reconcile failed, continuing with cache", slog.String("error.message", err.Error()))
	}

	namespaces := b.store.Namespaces(cl.Env)

	// If no namespace is registered, return an error without opening the Modal
	if len(namespaces) == 0 {
		if _, err := b.client.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText("⚠️ No managed workloads are registered yet. Please wait for ScaledObject discovery.", false),
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
	txn := b.startTransaction("SlackBot/NamespaceSelected")
	if txn != nil {
		defer txn.End()
		txn.AddAttribute("user.name", callback.User.Name)
		txn.AddAttribute("namespace", namespace)
	}

	// Extract the cluster from private_metadata and resolve it via the registry.
	var meta modalMetadata
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil {
		slog.Error("failed to parse private_metadata", slog.String("error.message", err.Error()))
		return
	}

	cl, ok := b.registry.ByEnv(meta.Cluster)
	if !ok {
		// The registry is immutable after startup, so a missing cluster is unexpected; guard defensively.
		slog.Warn("cluster not found at namespace selection")
		return
	}

	if txn != nil {

		txn.AddAttribute("cluster.name", cl.Name)
		txn.AddAttribute("cluster.env", cl.Env)
	}

	slog.Info("namespace selected", slog.String("user.name", callback.User.Name), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env), slog.String("namespace", namespace))

	// Merge workloads sharing a base name that differ only by suffix (api/app/worker) into one entry.
	// Splitting, display limit and diff all operate on these entries.
	groups := groupWorkloads(b.store.WorkloadsByNamespace(cl.Env, namespace))

	// Entries beyond the Modal block limit cannot be shown, so cut them off and warn.
	truncated := 0
	if len(groups) > maxWorkloadOptions {
		truncated = len(groups) - maxWorkloadOptions
		groups = groups[:maxWorkloadOptions]
		slog.Warn("display entries exceed the Modal limit; some are hidden",
			slog.String("cluster.env", cl.Env), slog.String("namespace", namespace),
			slog.Int("limit", maxWorkloadOptions), slog.Int("truncated", truncated))
	}

	// namespace select (keep the selected state; sorted a-z)
	nsBlock := namespaceInputBlock(b.store.Namespaces(cl.Env), namespace)

	// workload checkboxes (running ones are initially selected; sorted a-z)
	// Slack checkboxes allow 10 options max, so split across multiple blocks.
	blocks := make([]slack.Block, 0, 2+maxWorkloadCheckboxBlocks)
	blocks = append(blocks, nsBlock)
	blocks = append(blocks, workloadCheckboxBlocks(namespace, groups)...)

	if truncated > 0 {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf(
				":warning: There are %d targets, exceeding the Slack Modal display limit (%d). "+
					"The remaining %d cannot be changed on this screen.",
				len(groups)+truncated, maxWorkloadOptions, truncated,
			), false, false),
			nil, nil,
		))
	}

	// Add namespace and rendered count (post-merge entries) to private_metadata (keep cluster)
	meta.Namespace = namespace
	meta.Rendered = len(groups)
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
	txn := b.startTransaction("SlackBot/Plan")
	if txn != nil {
		defer txn.End()
		txn.AddAttribute("user.name", callback.User.Name)
	}

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

	if txn != nil {

		txn.AddAttribute("cluster.name", cl.Name)
		txn.AddAttribute("cluster.env", cl.Env)
		txn.AddAttribute("namespace", meta.Namespace)
	}

	// Selected workloads (checked = wants running), folded across all split checkbox blocks.
	selectedSet := selectedWorkloadNames(callback.View.State.Values)

	// Compare with the current state; rebuild with the same order and merge unit as render.
	// Entries not rendered in the Modal are excluded from the diff; otherwise
	// "not shown = unchecked = stop" would stop running workloads that are off screen.
	groups := renderedGroups(
		groupWorkloads(b.store.WorkloadsByNamespace(cl.Env, meta.Namespace)),
		meta.Rendered,
	)

	var toStart, toStop []string
	var previewLines []string

	for _, g := range groups {
		// A merged entry is represented by one member name; treat it as ON if any member name is
		// selected, so it still works when the representative disappears on reconcile.
		wantRunning := false
		for _, name := range g.Names() {
			if selectedSet[name] {
				wantRunning = true
				break
			}
		}

		var startNames, stopNames []string
		for _, w := range g.Members {
			isRunning := w.Replicas > 0
			switch {
			case wantRunning && !isRunning:
				toStart = append(toStart, w.Name)
				startNames = append(startNames, w.Name)
			case !wantRunning && isRunning:
				toStop = append(toStop, w.Name)
				stopNames = append(stopNames, w.Name)
			}
		}

		if len(startNames) > 0 {
			previewLines = append(previewLines, fmt.Sprintf("🟢 %s: stopped → *start*", g.changeLabel(startNames)))
		}
		if len(stopNames) > 0 {
			previewLines = append(previewLines, fmt.Sprintf("🔴 %s: running → *stop*", g.changeLabel(stopNames)))
		}
	}

	envUpper := strings.ToUpper(cl.Env)

	if len(toStart) == 0 && len(toStop) == 0 {
		// No changes: close the modal with an empty Ack and send an ephemeral notice
		b.ack(*req)
		if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
			slack.MsgOptionBlocks(
				slack.NewSectionBlock(
					slack.NewTextBlockObject("mrkdwn",
						fmt.Sprintf("ℹ️ *%s / %s / %s* — no changes", envUpper, cl.Name, meta.Namespace),
						false, false,
					),
					nil, nil,
				),
			),
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
			BlockSet: mrkdwnSections(previewHeader, previewLines),
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
	txn := b.startTransaction("SlackBot/Apply")
	if txn != nil {
		defer txn.End()
		txn.AddAttribute("user.name", callback.User.Name)
	}

	var meta previewMetadata
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil {
		slog.Error("failed to parse preview metadata", slog.String("error.message", err.Error()))
		return
	}

	// Defensive check: do nothing if the cluster vanished from the registry
	// (it is immutable after startup, so this is only a guard).
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

	if txn != nil {

		txn.AddAttribute("cluster.name", cl.Name)
		txn.AddAttribute("cluster.env", cl.Env)
		txn.AddAttribute("namespace", meta.Namespace)
		txn.AddAttribute("to_start_count", len(meta.ToStart))
		txn.AddAttribute("to_stop_count", len(meta.ToStop))
	}

	envUpper := strings.ToUpper(cl.Env)
	user := callback.User.Name
	var resultLines []string

	// ToStart / ToStop are workload names only; cluster / namespace come from meta.
	ns := meta.Namespace

	for _, name := range meta.ToStart {
		if name == "" {
			continue
		}
		b.store.Start(meta.Cluster, ns, name, user)
		resultLines = append(resultLines, fmt.Sprintf("🟢 %s → start", name))
		slog.Info("workload started", slog.String("service", name), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env), slog.String("namespace", ns), slog.String("user.name", user))
		b.recordCustomEvent("WorkloadKnocker:WorkloadOperation", map[string]any{
			"action":       "start",
			"env":          cl.Env,
			"cluster":      cl.Name,
			"namespace":    ns,
			"workload":     name,
			"user":         user,
			"service.name": "workload-knocker",
		})
	}

	for _, name := range meta.ToStop {
		if name == "" {
			continue
		}
		b.store.Stop(meta.Cluster, ns, name)
		resultLines = append(resultLines, fmt.Sprintf("🔴 %s → stop", name))
		slog.Info("workload stopped", slog.String("service", name), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env), slog.String("namespace", ns), slog.String("user.name", user))
		b.recordCustomEvent("WorkloadKnocker:WorkloadOperation", map[string]any{
			"action":       "stop",
			"env":          cl.Env,
			"cluster":      cl.Name,
			"namespace":    ns,
			"workload":     name,
			"user":         user,
			"service.name": "workload-knocker",
		})
	}

	resultHeader := fmt.Sprintf("🚀 <@%s> changed workloads (%s / %s / %s)",
		callback.User.ID, envUpper, cl.Name, meta.Namespace,
	)
	if len(meta.ToStart) > 0 {
		resultLines = append(resultLines, "⏰ Auto-stop: today 22:00 JST")
	}

	if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
		slack.MsgOptionBlocks(mrkdwnSections(resultHeader, resultLines)...),
	); err != nil {
		slog.Error("failed to send result message", slog.String("error.message", err.Error()))
	}
}
