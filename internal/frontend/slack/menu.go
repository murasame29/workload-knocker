package slackbot

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/policy/authz"
	"github.com/murasame29/workload-knocker/internal/schedule/reservation"
)

// The slash command opens a menu modal; each button replaces it with one flow, so the whole
// interaction stays in the same view. Every flow starts with a namespace select.
const (
	callbackMenuModal   = "menu_modal"
	callbackStatusModal = "status_modal"

	actionMenuStartStop = "menu_start_stop"
	actionMenuSchedule  = "menu_schedule"
	actionMenuStatus    = "menu_status"

	actionScheduleNamespace = "schedule_namespace"
	actionStatusNamespace   = "status_namespace"
	blockPickNamespace      = "block_pick_namespace"

	// slackSelectMaxOptions is Slack's cap of options in a static_select.
	slackSelectMaxOptions = 100
)

// viewAPI is the part of slack.Client that opens and replaces modals; tests replace it.
type viewAPI interface {
	OpenView(triggerID string, view slack.ModalViewRequest) (*slack.ViewResponse, error)
	UpdateView(view slack.ModalViewRequest, externalID, hash, viewID string) (*slack.ViewResponse, error)
}

// statusMetadata is the private_metadata of the status modal.
type statusMetadata struct {
	ChannelID string `json:"c"`
	UserID    string `json:"u"`
	Cluster   string `json:"e"`
	Namespace string `json:"n,omitempty"`
}

// menuItem is one row of the menu: a button with a short description under it.
type menuItem struct {
	actionID, label, description string
	style                        slack.Style
}

// menuModal asks which flow to run. Slack has no button size, so each choice gets its own row
// (description, then its button) to stack them vertically and give them room.
func (u ui) menuModal(cl config.Cluster, meta string) slack.ModalViewRequest {
	items := []menuItem{
		{actionMenuStartStop, u.t("▶️  Start / Stop"), u.t("Start and stop the workloads in the chosen namespace."), slack.StylePrimary},
		{actionMenuSchedule, u.t("📅  Schedule"), u.t("Reserve when the workloads in the chosen namespace start and how long until they stop."), slack.StyleDefault},
		{actionMenuStatus, u.t("📊  Status"), u.t("Check which workloads in the chosen namespace are running. You can stop them here too."), slack.StyleDefault},
	}
	blocks := []slack.Block{
		slack.NewHeaderBlock(slack.NewTextBlockObject("plain_text", truncateText(strings.ToUpper(cl.Env)+" / "+cl.Name, 150), false, false)),
	}
	for i, it := range items {
		if i > 0 {
			blocks = append(blocks, slack.NewDividerBlock())
		}
		btn := slack.NewButtonBlockElement(it.actionID, "", slack.NewTextBlockObject("plain_text", it.label, true, false))
		if it.style != slack.StyleDefault {
			btn = btn.WithStyle(it.style)
		}
		blocks = append(blocks,
			slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", it.description, false, false), nil, nil),
			slack.NewActionBlock("block_"+it.actionID, btn),
		)
	}
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      callbackMenuModal,
		Title:           slack.NewTextBlockObject("plain_text", modalTitle(cl), false, false),
		Close:           slack.NewTextBlockObject("plain_text", u.t("Cancel"), false, false),
		PrivateMetadata: meta,
		Blocks:          slack.Blocks{BlockSet: blocks},
	}
}

// noticeModal replaces a flow with a message when it cannot continue.
func (u ui) noticeModal(cl config.Cluster, text string) slack.ModalViewRequest {
	return slack.ModalViewRequest{
		Type:   slack.VTModal,
		Title:  slack.NewTextBlockObject("plain_text", modalTitle(cl), false, false),
		Close:  slack.NewTextBlockObject("plain_text", u.t("Close"), false, false),
		Blocks: slack.Blocks{BlockSet: []slack.Block{slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", text, false, false), nil, nil)}},
	}
}

// namespacePicker is a section with a namespace static_select that fires a block action on change,
// so the modal needs no submit button before a namespace is chosen.
func (u ui) namespacePicker(actionID string, namespaces []string, selected string) *slack.SectionBlock {
	sorted := slices.Sorted(slices.Values(namespaces))
	if len(sorted) > slackSelectMaxOptions {
		sorted = sorted[:slackSelectMaxOptions]
	}
	options := make([]*slack.OptionBlockObject, 0, len(sorted))
	var initial *slack.OptionBlockObject
	for _, ns := range sorted {
		opt := slack.NewOptionBlockObject(ns, slack.NewTextBlockObject("plain_text", truncateOptionText(ns), false, false), nil)
		options = append(options, opt)
		if ns == selected {
			initial = opt
		}
	}
	sel := slack.NewOptionsSelectBlockElement("static_select", slack.NewTextBlockObject("plain_text", u.t("Select a namespace"), false, false), actionID, options...)
	if initial != nil {
		sel.InitialOption = initial
	}
	return slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", u.t("*Namespace*"), false, false), nil, slack.NewAccessory(sel), slack.SectionBlockOptionBlockID(blockPickNamespace))
}

// noNamespaceText explains why a user sees no namespace; total is every namespace of the cluster.
func (u ui) noNamespaceText(total int) string {
	if total > 0 {
		return u.t("⚠️ You are not authorized to operate any workload in this cluster.")
	}
	return u.t("⚠️ No managed workloads are registered yet. Please wait for ScaledObject discovery.")
}

// pickModal is the first step of every flow: a namespace select only.
func (u ui) pickModal(cl config.Cluster, callbackID, title, actionID string, namespaces []string, meta string) slack.ModalViewRequest {
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      callbackID,
		Title:           slack.NewTextBlockObject("plain_text", truncateText(title+cl.Env, slackModalTitleMaxLen), false, false),
		Close:           slack.NewTextBlockObject("plain_text", u.t("Close"), false, false),
		PrivateMetadata: meta,
		Blocks:          slack.Blocks{BlockSet: []slack.Block{u.namespacePicker(actionID, namespaces, "")}},
	}
}

// startStopStart returns the namespace select of the start/stop flow, or a notice. Choosing a
// namespace shows its workloads (handleNamespaceSelected).
func (b *Bot) startStopStart(sub authz.Subject, cl config.Cluster, channelID, userID string) slack.ModalViewRequest {
	namespaces, total := b.svc.Namespaces(sub, cl.Env)
	if len(namespaces) == 0 {
		return b.noticeModal(cl, b.noNamespaceText(total))
	}
	meta, _ := json.Marshal(modalMetadata{ChannelID: channelID, UserID: userID, Cluster: cl.Env})
	return b.pickModal(cl, callbackWorkloadModal, "🚀 ", actionSelectNamespace, namespaces, string(meta))
}

// scheduleStart returns the namespace select of the schedule flow, or a notice.
func (b *Bot) scheduleStart(sub authz.Subject, cl config.Cluster, channelID, userID string) slack.ModalViewRequest {
	if b.reservations == nil {
		return b.noticeModal(cl, b.t("⚠️ Reservations are not available."))
	}
	namespaces, total := b.svc.Namespaces(sub, cl.Env)
	if len(namespaces) == 0 {
		return b.noticeModal(cl, b.noNamespaceText(total))
	}
	meta, _ := json.Marshal(scheduleMetadata{ChannelID: channelID, UserID: userID, Cluster: cl.Env})
	return b.pickModal(cl, callbackScheduleModal, "📅 ", actionScheduleNamespace, namespaces, string(meta))
}

// statusStart returns the namespace select of the status flow, or a notice.
func (b *Bot) statusStart(sub authz.Subject, cl config.Cluster, channelID, userID string) slack.ModalViewRequest {
	namespaces, total := b.svc.Namespaces(sub, cl.Env)
	if len(namespaces) == 0 {
		return b.noticeModal(cl, b.noNamespaceText(total))
	}
	meta, _ := json.Marshal(statusMetadata{ChannelID: channelID, UserID: userID, Cluster: cl.Env})
	return b.pickModal(cl, callbackStatusModal, "📊 ", actionStatusNamespace, namespaces, string(meta))
}

// handleMenu replaces the menu with the chosen flow.
func (b *Bot) handleMenu(callback slack.InteractionCallback, actionID string) {
	var meta modalMetadata
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil {
		slog.Error("failed to parse menu metadata", slog.String("error.message", err.Error()))
		return
	}
	cl, ok := b.registry.ByEnv(meta.Cluster)
	if !ok {
		slog.Warn("cluster not found at menu selection")
		return
	}
	sub := b.subject(callback.User.ID)
	var view slack.ModalViewRequest
	switch actionID {
	case actionMenuStartStop:
		// Reflect the latest ScaledObject state before listing.
		b.svc.Reconcile(cl.Env)
		view = b.startStopStart(sub, cl, meta.ChannelID, meta.UserID)
	case actionMenuSchedule:
		view = b.scheduleStart(sub, cl, meta.ChannelID, meta.UserID)
	default:
		view = b.statusStart(sub, cl, meta.ChannelID, meta.UserID)
	}
	b.updateView(callback, view)
}

// updateView replaces the modal the interaction came from.
func (b *Bot) updateView(callback slack.InteractionCallback, view slack.ModalViewRequest) {
	if _, err := b.views.UpdateView(view, "", callback.View.Hash, callback.View.ID); err != nil {
		slog.Error("failed to update Modal", slog.String("view.callback_id", view.CallbackID), slog.String("error.message", err.Error()))
	}
}

// handleScheduleNamespace shows the reservation form of the chosen namespace.
func (b *Bot) handleScheduleNamespace(callback slack.InteractionCallback, namespace string) {
	var meta scheduleMetadata
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil {
		slog.Error("failed to parse schedule metadata", slog.String("error.message", err.Error()))
		return
	}
	cl, ok := b.registry.ByEnv(meta.Cluster)
	if !ok || b.reservations == nil {
		return
	}
	sub := b.subject(callback.User.ID)
	namespaces, _ := b.svc.Namespaces(sub, cl.Env)
	listing := b.svc.List(sub, cl.Env, namespace, listLimits)
	meta.Namespaces = []string{namespace}
	raw, _ := json.Marshal(meta)
	b.updateView(callback, b.scheduleModal(cl, namespaces, namespace, listing, string(raw)))
}

// handleStatusNamespace shows the status of the chosen namespace.
func (b *Bot) handleStatusNamespace(callback slack.InteractionCallback, namespace string) {
	var meta statusMetadata
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil {
		slog.Error("failed to parse status metadata", slog.String("error.message", err.Error()))
		return
	}
	cl, ok := b.registry.ByEnv(meta.Cluster)
	if !ok {
		return
	}
	meta.Namespace = namespace
	b.updateView(callback, b.statusModal(b.subject(callback.User.ID), viewerOf(callback.User), cl, meta, nil))
}

// statusModal renders the status of one namespace with its buttons and pending reservations.
func (b *Bot) statusModal(sub authz.Subject, v viewer, cl config.Cluster, meta statusMetadata, notes []string) slack.ModalViewRequest {
	namespaces, _ := b.svc.Namespaces(sub, cl.Env)
	st := namespaceStatus(b.svc.Status(sub, cl.Env), meta.Namespace)
	pending := namespaceReservations(b.visibleReservations(sub, cl.Env), meta.Namespace)
	picker := b.namespacePicker(actionStatusNamespace, namespaces, meta.Namespace)
	_, blocks := b.statusBlocks(cl, st, pending, notes, slackModalMaxBlocks-1, v)
	raw, _ := json.Marshal(meta)
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      callbackStatusModal,
		Title:           slack.NewTextBlockObject("plain_text", truncateText("📊 "+cl.Env, slackModalTitleMaxLen), false, false),
		Close:           slack.NewTextBlockObject("plain_text", b.t("Close"), false, false),
		PrivateMetadata: string(raw),
		Blocks:          slack.Blocks{BlockSet: append([]slack.Block{picker}, blocks...)},
	}
}

// namespaceStatus keeps only namespace ns of st.
func namespaceStatus(st frontend.Status, ns string) frontend.Status {
	out := frontend.Status{Env: st.Env}
	for _, n := range st.Namespaces {
		if n.Name == ns {
			out.Namespaces = append(out.Namespaces, n)
		}
	}
	return out
}

// namespaceReservations keeps the reservations of namespace ns.
func namespaceReservations(list []reservation.Reservation, ns string) []reservation.Reservation {
	var out []reservation.Reservation
	for _, r := range list {
		if r.Namespace == ns {
			out = append(out, r)
		}
	}
	return out
}

// refreshStatus redraws the status after a button: in place in the status modal, or by replacing
// the ephemeral message of older versions through its response_url.
func (b *Bot) refreshStatus(callback slack.InteractionCallback, cl config.Cluster, sub authz.Subject, notes []string) {
	if callback.View.ID != "" && callback.View.CallbackID == callbackStatusModal {
		var meta statusMetadata
		if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil {
			slog.Error("failed to parse status metadata", slog.String("error.message", err.Error()))
			return
		}
		// Replace regardless of the hash: the state changed by the action itself.
		if _, err := b.views.UpdateView(b.statusModal(sub, viewerOf(callback.User), cl, meta, notes), "", "", callback.View.ID); err != nil {
			slog.Error("failed to update the status modal", slog.String("error.message", err.Error()))
		}
		return
	}
	text, blocks := b.statusMessage(cl, b.svc.Status(sub, cl.Env), b.visibleReservations(sub, cl.Env), notes, viewerOf(callback.User))
	b.replaceOriginal(callback, text, blocks)
}
