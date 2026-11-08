package slackbot

import (
	"fmt"
	"slices"
	"strings"

	"github.com/murasame29/workload-knocker/internal/model"
)

// mergeableSuffixes は 1 チェックボックスに merge する対象の workload 名 suffix。
//
// labs namespace のように 1 サービスが役割ごとの Deployment に分かれている
// (<service>-api / <service>-app / <service>-worker) ケースでは、利用者から見ると
// 「サービス 1 つ」なので個別にチェックさせる意味がない。base 名が同じで suffix だけが
// 異なる workload を束ね、1 回のチェックで全メンバーを起動/停止する。
//
// 副作用として checkbox の options 件数が減るため、10 件上限の分割数も抑えられる。
var mergeableSuffixes = []string{"api", "app", "worker"}

// workloadGroup は Slack Modal 上で 1 つのチェックボックスとして扱う workload の集合。
// Grouped=false のときはメンバー 1 件の単体 workload を表す。
type workloadGroup struct {
	// Key は表示・ソートに使う識別子。merge 時は共通の base 名、単体時は workload 名。
	Key string
	// Grouped は複数 workload を 1 チェックボックスに merge したかどうか。
	Grouped bool
	// Members は対象 workload。name の昇順。
	Members []*model.Workload
}

// groupWorkloads は workload 一覧を Modal の表示単位に束ね、Key の昇順 (a→z) で返す。
//
// merge するのは base 名が同じ mergeable suffix 付き workload が 2 件以上あるときだけ。
// 1 件しかない場合 (例: api だけ存在) は従来通り workload 名のまま単体表示する。
//
// 並び順を固定するのは表示のためだけでなく、checkbox の分割境界と
// 表示上限で truncate する位置を render 時と submit 時で一致させるため
// (sortedWorkloads と同じ理由)。
func groupWorkloads(workloads []*model.Workload) []workloadGroup {
	byBase := make(map[string][]*model.Workload)
	var singles []*model.Workload
	for _, w := range workloads {
		base, ok := splitMergeableSuffix(w.Name)
		if !ok {
			singles = append(singles, w)
			continue
		}
		byBase[base] = append(byBase[base], w)
	}

	groups := make([]workloadGroup, 0, len(byBase)+len(singles))
	for base, members := range byBase {
		if len(members) < 2 {
			// suffix 付きでも 1 件だけなら merge しない。
			singles = append(singles, members...)
			continue
		}
		groups = append(groups, workloadGroup{
			Key:     base,
			Grouped: true,
			Members: sortedWorkloads(members),
		})
	}
	for _, w := range singles {
		groups = append(groups, workloadGroup{Key: w.Name, Members: []*model.Workload{w}})
	}

	// Key 昇順。Key が衝突した場合 (base 名と同名の workload が併存) は
	// option value で安定化する。
	slices.SortFunc(groups, func(a, b workloadGroup) int {
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return strings.Compare(a.Value(), b.Value())
	})
	return groups
}

// renderedGroups は private_metadata の rendered 件数に合わせて差分計算の対象を絞る。
// 表示上限で truncate された group を「未チェック = 停止」と誤判定しないためのガード。
// rendered が 0 (旧 Modal 由来で未設定) や現在の件数以上のときは全件を返す。
func renderedGroups(groups []workloadGroup, rendered int) []workloadGroup {
	if rendered <= 0 || rendered >= len(groups) {
		return groups
	}
	return groups[:rendered]
}

// splitMergeableSuffix は name が "<base>-<mergeable suffix>" 形式なら base を返す。
func splitMergeableSuffix(name string) (string, bool) {
	for _, sfx := range mergeableSuffixes {
		trailer := "-" + sfx
		if len(name) > len(trailer) && strings.HasSuffix(name, trailer) {
			return name[:len(name)-len(trailer)], true
		}
	}
	return "", false
}

// Value は Slack option の value。代表メンバー (name 昇順の先頭) の workload 名を使う。
// メンバー全員の名前を詰め込むと option value の 75 文字上限を超えるため、
// submit 側では「いずれかのメンバー名が選択されていれば ON」として解釈する。
func (g workloadGroup) Value() string { return g.Members[0].Name }

// Names はメンバーの workload 名を name 昇順で返す。
func (g workloadGroup) Names() []string {
	out := make([]string, 0, len(g.Members))
	for _, w := range g.Members {
		out = append(out, w.Name)
	}
	return out
}

// runningCount は起動中 (replicas > 0) のメンバー数を返す。
func (g workloadGroup) runningCount() int {
	n := 0
	for _, w := range g.Members {
		if w.Replicas > 0 {
			n++
		}
	}
	return n
}

// anyRunning は 1 件以上のメンバーが起動中かを返す。
// チェックボックスの初期選択に使う。部分起動 (🟡) でチェックを外してしまうと
// 「変更なしで Plan → 稼働中のメンバーが停止される」ことになるため、
// 稼働側に寄せて「不足メンバーを起動する」方向に収束させる。
func (g workloadGroup) anyRunning() bool { return g.runningCount() > 0 }

// suffixes は merge されたメンバーの suffix を name 昇順で返す。
func (g workloadGroup) suffixes() []string {
	out := make([]string, 0, len(g.Members))
	for _, w := range g.Members {
		out = append(out, strings.TrimPrefix(w.Name, g.Key+"-"))
	}
	return out
}

// label はチェックボックスに表示するテキストを返す。
//
//	単体      : workloadLabel と同じ ("🟢 name (N pod)" / "🔴 name")
//	merge 済み: "🟢 base (api, app, worker)"             … 全メンバー起動中
//	            "🟡 base (api, app, worker) 2/3 起動中"  … 部分起動
//	            "🔴 base (api, app, worker)"             … 全メンバー停止中
func (g workloadGroup) label() string {
	if !g.Grouped {
		return truncateOptionText(workloadLabel(g.Members[0]))
	}

	running := g.runningCount()
	icon := "🔴"
	switch {
	case running == len(g.Members):
		icon = "🟢"
	case running > 0:
		icon = "🟡"
	}

	text := fmt.Sprintf("%s %s (%s)", icon, g.Key, strings.Join(g.suffixes(), ", "))
	if running > 0 && running < len(g.Members) {
		text += fmt.Sprintf(" %d/%d 起動中", running, len(g.Members))
	}
	return truncateOptionText(text)
}

// changeLabel はプレビュー行に出す対象表記を返す。names は今回変更するメンバー名。
// merge 済みの場合は変更対象の suffix だけを列挙する (例: "checkout (app, worker)")。
func (g workloadGroup) changeLabel(names []string) string {
	if len(names) == 0 {
		return g.Key
	}
	if !g.Grouped {
		return names[0]
	}
	sfx := make([]string, 0, len(names))
	for _, n := range names {
		sfx = append(sfx, strings.TrimPrefix(n, g.Key+"-"))
	}
	return fmt.Sprintf("%s (%s)", g.Key, strings.Join(sfx, ", "))
}

// truncateOptionText は Slack option text の文字数上限に収める。
func truncateOptionText(s string) string {
	runes := []rune(s)
	if len(runes) <= slackOptionTextMaxLen {
		return s
	}
	return string(runes[:slackOptionTextMaxLen])
}
