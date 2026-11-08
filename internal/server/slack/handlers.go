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

	// slackModalTitleMaxLen は Slack Modal の plain_text title の最大文字数。
	// 超過すると invalid_arguments で OpenView が拒否される。
	slackModalTitleMaxLen = 24

	// checkboxMaxOptions は Slack checkboxes element に載せられる options の上限。
	// 超過すると invalid_arguments で OpenView / UpdateView が拒否されるため、
	// 表示エントリはこの件数ごとに複数の input block へ分割する。
	checkboxMaxOptions = 10

	// slackOptionTextMaxLen は option object の text の最大文字数。
	slackOptionTextMaxLen = 75

	// slackModalMaxBlocks は Modal 1 つに含められる block 数の上限。
	slackModalMaxBlocks = 100

	// reservedModalBlocks は workload checkbox 以外に確保しておく block 数。
	// (namespace select + 表示上限超過の警告 section)
	reservedModalBlocks = 2

	// maxWorkloadCheckboxBlocks / maxWorkloadOptions は 1 つの Modal で扱える
	// checkbox block 数と表示エントリ件数の上限。
	// エントリは suffix merge 後の単位なので、workload 実数はこれより多くなり得る。
	maxWorkloadCheckboxBlocks = (slackModalMaxBlocks - reservedModalBlocks) / checkboxMaxOptions
	maxWorkloadOptions        = maxWorkloadCheckboxBlocks * checkboxMaxOptions

	// slackPrivateMetadataMaxLen は view.private_metadata の最大文字数。
	slackPrivateMetadataMaxLen = 3000

	// slackSectionTextMaxLen は section block の text の最大文字数。
	slackSectionTextMaxLen = 3000
)

// modalTitle は Slack Modal の title を生成する。
// 24 文字以内に収めるため "🚀 <key>" 形式を基本とし、それでも超過する場合は truncate する。
// emoji 1 文字 + space + key の組み合わせで実用上収まる前提。
func modalTitle(cl config.Cluster) string {
	title := "🚀 " + cl.Env
	runes := []rune(title)
	if len(runes) <= slackModalTitleMaxLen {
		return title
	}
	return string(runes[:slackModalTitleMaxLen])
}

// sortedWorkloads は workload を name 昇順に並べたコピーを返す。
// store は map 走査で返すため順序が不定であり、そのままでは
//   - checkbox の分割境界と表示順が毎回入れ替わる
//   - 表示上限で truncate する対象が render 時と submit 時でズレる
//
// という問題が起きる。ここで順序を固定して両者を一致させる。
// 表示エントリ全体の並びは groupWorkloads が Key 昇順で確定させ、
// 本関数は merge されたエントリ内のメンバー順 (= suffix の表示順) を担う。
func sortedWorkloads(ws []*model.Workload) []*model.Workload {
	out := slices.Clone(ws)
	slices.SortFunc(out, func(a, b *model.Workload) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// workloadLabel は checkbox に表示する単体 workload のラベルを返す。
func workloadLabel(w *model.Workload) string {
	if w.Replicas > 0 {
		return fmt.Sprintf("🟢 %s (%d pod)", w.Name, w.Replicas)
	}
	return fmt.Sprintf("🔴 %s", w.Name)
}

// namespaceInputBlock は namespace の static_select ブロックを生成する。
// 表示順は a→z に固定する (store は map 走査で返すため順序が不定)。
// selected が非空かつ一覧に含まれる場合は初期選択にする。
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
		slack.NewTextBlockObject("plain_text", "Namespace を選択", false, false),
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

// workloadCheckboxBlocks は表示エントリを checkboxMaxOptions 件ずつの input block に
// 分割して返す。block_id / action_id は blockSelectWorkloads / actionSelectWorkloads に
// 連番を付与した形 (例: "block_workloads_0") で、submit 時は prefix でまとめて回収する。
// option の value は代表 workload 名のみ (option value は 75 文字上限があるため
// "cluster/namespace/name" のフル tuple は使わない)。
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

		// 分割時はどの範囲の block か分かるよう label に連番を出す。
		label := fmt.Sprintf("Workloads (%s)", namespace)
		if len(groups) > checkboxMaxOptions {
			label = fmt.Sprintf("Workloads (%s) %d-%d", namespace, start+1, end)
		}

		// hint は先頭 block だけに付けて縦の冗長さを抑える。
		var hint *slack.TextBlockObject
		if idx == 0 {
			hint = slack.NewTextBlockObject("plain_text", "✅ = 起動  ☐ = 停止", false, false)
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

// selectedWorkloadNames は分割された全 checkbox block の選択状態を 1 つの集合に畳み込む。
// prefix 一致で走査するので、分割前の単一 block ("block_workloads") 由来の
// 送信内容もそのまま回収できる。
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

// mrkdwnSections は header + lines を section block の text 上限 (3000 文字) に
// 収まるよう複数 section に分割する。1 行あたりの長さは workload 名 (K8s 上限 253 文字)
// で抑えられているため、1 行が単独で上限を超えることはない。
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

// modalMetadata は Modal の private_metadata に埋め込む情報。
// Cluster は Slack スラッシュコマンドから解決した cluster.Key を保持する。
type modalMetadata struct {
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace,omitempty"`
	// Rendered は Modal の checkbox に実際に描画した workload 件数。
	// 表示上限で truncate された workload を submit 時に「未チェック = 停止」と
	// 誤判定しないよう、差分計算の対象範囲を復元するために使う。
	Rendered int `json:"rendered,omitempty"`
}

// previewMetadata はプレビュー Modal の private_metadata。
// ToStart / ToStop は workload 名のみを保持する (cluster / namespace は同一 struct の
// フィールドで一意に決まる)。private_metadata は 3000 文字上限があり、
// "cluster/namespace/name" のフル tuple では件数が増えたときに破綻するため。
type previewMetadata struct {
	ChannelID string   `json:"channel_id"`
	UserID    string   `json:"user_id"`
	Cluster   string   `json:"cluster"`
	Namespace string   `json:"namespace"`
	ToStart   []string `json:"to_start"` // workload 名
	ToStop    []string `json:"to_stop"`  // workload 名
}

// handleSlashCommand は /workload-knocker-* を処理する。
// cluster は b.registry.BySlashCommand(cmd.Command) で解決する。
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
			slack.MsgOptionText("⚠️ このスラッシュコマンドは Cluster Registry に登録されていません", false),
		); err != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()))
		}
		return
	}

	if txn != nil {

		txn.AddAttribute("cluster.name", cl.Name)
		txn.AddAttribute("cluster.env", cl.Env)
	}

	slog.Info("slash command 受信", slog.String("user.name", cmd.UserName), slog.String("command", cmd.Command), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env))

	// 最新の ScaledObject 状態をオンデマンドで反映する。
	// 失敗してもキャッシュ済みの registry で続行する。
	reconcileCtx, cancel := context.WithTimeout(context.Background(), b.cfg.ClusterReconcileTimeout)
	defer cancel()
	if err := b.store.Reconcile(reconcileCtx, cl.Env); err != nil {
		slog.Warn("ScaledObject の reconcile に失敗、キャッシュで続行", slog.String("error.message", err.Error()))
	}

	namespaces := b.store.Namespaces(cl.Env)

	// namespace が未登録の場合は Modal を開かずにエラーを返す
	if len(namespaces) == 0 {
		if _, err := b.client.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText("⚠️ 管理対象のワークロードがまだ登録されていません。ScaledObject の検出をお待ちください。", false),
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
		slog.Error("Modal のオープンに失敗", slog.String("error.message", err.Error()))
	}
}

// handleInteraction は Slack のインタラクションを処理する（view_submission 以外）
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

// handleViewSubmission は view_submission イベントを処理する
// Socket Mode では Ack payload に response_action を含めることでモーダルを制御する
func (b *Bot) handleViewSubmission(callback slack.InteractionCallback, req *socketmode.Request) {
	slog.Debug("view submission dispatched", slog.String("view.callback_id", callback.View.CallbackID), slog.String("view.id", callback.View.ID))
	switch callback.View.CallbackID {
	case callbackWorkloadModal:
		b.handleModalSubmit(callback, req)
	case callbackPreviewModal:
		// Apply: 空 Ack でモーダルを閉じてからチャンネルに結果を投稿
		b.ack(*req)
		b.handlePreviewConfirm(callback)
	default:
		slog.Warn("unknown view submission callback id", slog.String("view.callback_id", callback.View.CallbackID))
		b.ack(*req)
	}
}

// handleNamespaceSelected は namespace 選択後に workload checkboxes を表示する。
// 稼働中のワークロードは initial_options で初期選択状態にする。
func (b *Bot) handleNamespaceSelected(callback slack.InteractionCallback, namespace string) {
	txn := b.startTransaction("SlackBot/NamespaceSelected")
	if txn != nil {
		defer txn.End()
		txn.AddAttribute("user.name", callback.User.Name)
		txn.AddAttribute("namespace", namespace)
	}

	// private_metadata から cluster を取り出して registry で解決する。
	var meta modalMetadata
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil {
		slog.Error("private_metadata のパースに失敗", slog.String("error.message", err.Error()))
		return
	}

	cl, ok := b.registry.ByEnv(meta.Cluster)
	if !ok {
		// 起動後 registry は immutable なので、この時点で消えるのは想定外。防御的にガード。
		slog.Warn("cluster not found at namespace selection")
		return
	}

	if txn != nil {

		txn.AddAttribute("cluster.name", cl.Name)
		txn.AddAttribute("cluster.env", cl.Env)
	}

	slog.Info("namespace 選択", slog.String("user.name", callback.User.Name), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env), slog.String("namespace", namespace))

	// base 名が同じで suffix (api/app/worker) だけ違う workload は 1 エントリに merge する。
	// 以降の分割・表示上限・差分計算はすべてこのエントリ単位で扱う。
	groups := groupWorkloads(b.store.WorkloadsByNamespace(cl.Env, namespace))

	// Modal の block 数上限を超える分は表示できないので切り落とし、警告を出す。
	truncated := 0
	if len(groups) > maxWorkloadOptions {
		truncated = len(groups) - maxWorkloadOptions
		groups = groups[:maxWorkloadOptions]
		slog.Warn("表示エントリ件数が Modal の表示上限を超過したため一部を非表示にしました",
			slog.String("cluster.env", cl.Env), slog.String("namespace", namespace),
			slog.Int("limit", maxWorkloadOptions), slog.Int("truncated", truncated))
	}

	// namespace select（選択済み状態を維持。表示順は a→z）
	nsBlock := namespaceInputBlock(b.store.Namespaces(cl.Env), namespace)

	// workload checkboxes（稼働中は初期選択。表示順は a→z）
	// Slack の checkboxes は 10 options 上限なので複数 block に分割する。
	blocks := make([]slack.Block, 0, 2+maxWorkloadCheckboxBlocks)
	blocks = append(blocks, nsBlock)
	blocks = append(blocks, workloadCheckboxBlocks(namespace, groups)...)

	if truncated > 0 {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf(
				":warning: 対象が %d 件あり、Slack Modal の表示上限 (%d 件) を超えています。"+
					"残り %d 件はこの画面では変更できません。",
				len(groups)+truncated, maxWorkloadOptions, truncated,
			), false, false),
			nil, nil,
		))
	}

	// private_metadata に namespace と描画件数 (merge 後のエントリ数) を追加 (cluster は維持)
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
		slog.Error("Modal の更新に失敗", slog.String("error.message", err.Error()))
	}
}

// handleModalSubmit は Plan ボタン押下時に差分を計算してプレビュー Modal を表示する。
// Socket Mode の view_submission では Ack payload に response_action: update を含めて
// 同じモーダルをプレビューに切り替える。
func (b *Bot) handleModalSubmit(callback slack.InteractionCallback, req *socketmode.Request) {
	txn := b.startTransaction("SlackBot/Plan")
	if txn != nil {
		defer txn.End()
		txn.AddAttribute("user.name", callback.User.Name)
	}

	var meta modalMetadata
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil {
		slog.Error("private_metadata のパースに失敗", slog.String("error.message", err.Error()))
		b.ack(*req)
		return
	}

	cl, ok := b.registry.ByEnv(meta.Cluster)
	if !ok {
		slog.Warn("cluster not found at plan submit")
		b.ack(*req)
		if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
			slack.MsgOptionText("⚠️ 対象 Cluster が Registry から消えています。再度コマンドを実行してください。", false),
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

	// 選択されたワークロード（チェックあり = 起動したい）。分割された全 checkbox block を畳み込む。
	selectedSet := selectedWorkloadNames(callback.View.State.Values)

	// 現在の状態と比較して差分を計算。render 時と同じ順序・同じ merge 単位で組み直す。
	// Modal に描画されていなかったエントリは差分計算から除外する。
	// 除外しないと「未表示 = 未チェック = 停止」と解釈され、画面に出ていない
	// 稼働中の workload が意図せず停止されてしまう。
	groups := renderedGroups(
		groupWorkloads(b.store.WorkloadsByNamespace(cl.Env, meta.Namespace)),
		meta.Rendered,
	)

	var toStart, toStop []string
	var previewLines []string

	for _, g := range groups {
		// merge されたエントリは代表メンバー名 1 つで全メンバーを表す。
		// 代表が reconcile で消えて value がずれても追従できるよう、
		// いずれかのメンバー名が選択されていれば ON とみなす。
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
			previewLines = append(previewLines, fmt.Sprintf("🟢 %s: 停止 → *起動*", g.changeLabel(startNames)))
		}
		if len(stopNames) > 0 {
			previewLines = append(previewLines, fmt.Sprintf("🔴 %s: 稼働中 → *停止*", g.changeLabel(stopNames)))
		}
	}

	envUpper := strings.ToUpper(cl.Env)

	if len(toStart) == 0 && len(toStop) == 0 {
		// 変更なし — 空 Ack でモーダルを閉じてエフェメラル通知
		b.ack(*req)
		if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
			slack.MsgOptionBlocks(
				slack.NewSectionBlock(
					slack.NewTextBlockObject("mrkdwn",
						fmt.Sprintf("ℹ️ *%s / %s / %s* — 変更はありません", envUpper, cl.Name, meta.Namespace),
						false, false,
					),
					nil, nil,
				),
			),
		); err != nil {
			slog.Error("変更なしメッセージの送信に失敗", slog.String("error.message", err.Error()))
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
	// private_metadata は 3000 文字上限。差分が極端に大きい場合はプレビューを
	// 開けないので、分割実行を促して打ち切る (ユーザー側で対処可能)。
	if err != nil || len(previewMeta) > slackPrivateMetadataMaxLen {
		slog.Warn("preview metadata が private_metadata の上限を超過",
			slog.Int("size", len(previewMeta)), slog.Int("limit", slackPrivateMetadataMaxLen),
			slog.Int("to_start", len(toStart)), slog.Int("to_stop", len(toStop)))
		b.ack(*req)
		if _, perr := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
			slack.MsgOptionText(fmt.Sprintf(
				"⚠️ 一度に変更する件数が多すぎます (起動 %d 件 / 停止 %d 件)。数回に分けて実行してください。",
				len(toStart), len(toStop)), false),
		); perr != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.message", perr.Error()))
		}
		return
	}

	// プレビュー Modal を response_action: update で表示する
	previewHeader := fmt.Sprintf("*%s / %s / %s* の変更プレビュー:\n",
		envUpper, cl.Name, meta.Namespace,
	)

	previewModal := slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      callbackPreviewModal,
		Title:           slack.NewTextBlockObject("plain_text", "変更プレビュー", false, false),
		Submit:          slack.NewTextBlockObject("plain_text", "✅ Apply", false, false),
		Close:           slack.NewTextBlockObject("plain_text", "Back", false, false),
		PrivateMetadata: string(previewMeta),
		Blocks: slack.Blocks{
			BlockSet: mrkdwnSections(previewHeader, previewLines),
		},
	}

	// Ack payload に response_action: update を含めてモーダルを切り替える
	b.ack(*req, map[string]any{
		"response_action": "update",
		"view":            previewModal,
	})
}

// handlePreviewConfirm はプレビューの Apply ボタンで実際に起動/停止を実行する。
func (b *Bot) handlePreviewConfirm(callback slack.InteractionCallback) {
	txn := b.startTransaction("SlackBot/Apply")
	if txn != nil {
		defer txn.End()
		txn.AddAttribute("user.name", callback.User.Name)
	}

	var meta previewMetadata
	if err := json.Unmarshal([]byte(callback.View.PrivateMetadata), &meta); err != nil {
		slog.Error("preview metadata のパースに失敗", slog.String("error.message", err.Error()))
		return
	}

	// 防御的チェック: cluster が registry から消えている場合は何もしない。
	// (registry は起動後 immutable なので通常は発生しないが、ガードとして残す)
	cl, ok := b.registry.ByEnv(meta.Cluster)
	if !ok {
		slog.Warn("cluster not found in registry at apply time")
		if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
			slack.MsgOptionText("⚠️ 対象 Cluster が Registry から消えています。再度コマンドを実行してください。", false),
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

	// ToStart / ToStop は workload 名のみ。cluster / namespace は meta から解決する。
	ns := meta.Namespace

	for _, name := range meta.ToStart {
		if name == "" {
			continue
		}
		b.store.Start(meta.Cluster, ns, name, user)
		resultLines = append(resultLines, fmt.Sprintf("🟢 %s → 起動", name))
		slog.Info("ワークロード起動", slog.String("service", name), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env), slog.String("namespace", ns), slog.String("user.name", user))
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
		resultLines = append(resultLines, fmt.Sprintf("🔴 %s → 停止", name))
		slog.Info("ワークロード停止", slog.String("service", name), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env), slog.String("namespace", ns), slog.String("user.name", user))
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

	resultHeader := fmt.Sprintf("🚀 <@%s> がワークロードを変更しました (%s / %s / %s)",
		callback.User.ID, envUpper, cl.Name, meta.Namespace,
	)
	if len(meta.ToStart) > 0 {
		resultLines = append(resultLines, "⏰ 自動停止: 今日 22:00 JST")
	}

	if _, err := b.client.PostEphemeral(meta.ChannelID, meta.UserID,
		slack.MsgOptionBlocks(mrkdwnSections(resultHeader, resultLines)...),
	); err != nil {
		slog.Error("結果メッセージの送信に失敗", slog.String("error.message", err.Error()))
	}
}
