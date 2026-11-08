package slackbot

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/model"
)

// wl はテスト用の workload を生成する。
func wl(name string, replicas int) *model.Workload {
	return &model.Workload{Env: "stg", Name: name, Namespace: "labs", Kind: "Deployment", Replicas: replicas}
}

// TestGroupWorkloads_MergesSuffixVariants は base 名が同じで suffix (api/app/worker) だけ
// 異なる workload が 1 エントリに merge され、表示順が a→z になることを検証する。
func TestGroupWorkloads_MergesSuffixVariants(t *testing.T) {
	t.Parallel()

	// store の map 走査順に依存しないよう、意図的にバラバラの順で渡す。
	in := []*model.Workload{
		wl("checkout-worker", 0),
		wl("payments-api", 0), // suffix 付きだが 1 件だけ → merge しない
		wl("menu-app", 0),
		wl("checkout-api", 0),
		wl("menu-api", 0),
		wl("bedrock-hello", 0), // suffix なし
		wl("checkout-app", 0),
	}

	groups := groupWorkloads(in)

	wantKeys := []string{"bedrock-hello", "checkout", "menu", "payments-api"}
	if len(groups) != len(wantKeys) {
		t.Fatalf("groups: got %d, want %d (%v)", len(groups), len(wantKeys), groupKeys(groups))
	}
	for i, want := range wantKeys {
		if groups[i].Key != want {
			t.Errorf("groups[%d].Key: got %q, want %q (all=%v)", i, groups[i].Key, want, groupKeys(groups))
		}
	}

	byKey := make(map[string]workloadGroup, len(groups))
	for _, g := range groups {
		byKey[g.Key] = g
	}

	// checkout: api/app/worker の 3 件が 1 チェックボックスに merge される。
	mm := byKey["checkout"]
	if !mm.Grouped {
		t.Errorf("checkout: Grouped = false, want true")
	}
	if got := strings.Join(mm.suffixes(), ","); got != "api,app,worker" {
		t.Errorf("checkout: suffixes = %q, want %q", got, "api,app,worker")
	}
	// option value は代表メンバー (name 昇順の先頭) の workload 名のみ。
	if got, want := mm.Value(), "checkout-api"; got != want {
		t.Errorf("checkout: Value() = %q, want %q", got, want)
	}

	// menu: api/app の 2 件で merge される。
	if got := len(byKey["menu"].Members); got != 2 {
		t.Errorf("menu: members = %d, want 2", got)
	}

	// suffix 付きでも 1 件だけなら従来通り workload 名のまま単体表示する。
	sa := byKey["payments-api"]
	if sa.Grouped {
		t.Errorf("payments-api: Grouped = true, want false")
	}
	if got := sa.label(); got != "🔴 payments-api" {
		t.Errorf("payments-api: label() = %q", got)
	}

	// suffix なしも単体表示。
	if byKey["bedrock-hello"].Grouped {
		t.Errorf("bedrock-hello: Grouped = true, want false")
	}
}

// TestGroupWorkloads_NamesCoverAllMembers は merge されたエントリの Names() が
// 全メンバーを個別に列挙することを検証する。1 チェックで app / api 両方の
// workload_knocker_activation を emit するために必須。
func TestGroupWorkloads_NamesCoverAllMembers(t *testing.T) {
	t.Parallel()

	groups := groupWorkloads([]*model.Workload{
		wl("checkout-app", 0),
		wl("checkout-api", 0),
		wl("checkout-worker", 0),
	})
	if len(groups) != 1 {
		t.Fatalf("groups: got %d, want 1", len(groups))
	}

	got := groups[0].Names()
	want := []string{"checkout-api", "checkout-app", "checkout-worker"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Names():\n got %v\nwant %v", got, want)
	}
}

// TestGroupWorkloads_IsDeterministic は入力順が変わっても同じ並びになり、
// 入力スライスを破壊しないことを検証する。render 時と submit 時で
// truncate 位置を一致させる前提。
func TestGroupWorkloads_IsDeterministic(t *testing.T) {
	t.Parallel()

	a := []*model.Workload{wl("charlie-api", 0), wl("alpha", 0), wl("charlie-app", 0), wl("bravo", 0)}
	b := []*model.Workload{wl("bravo", 0), wl("charlie-app", 0), wl("alpha", 0), wl("charlie-api", 0)}

	if got, want := strings.Join(groupKeys(groupWorkloads(a)), "|"), "alpha|bravo|charlie"; got != want {
		t.Errorf("keys(a) = %q, want %q", got, want)
	}
	if got, want := strings.Join(groupKeys(groupWorkloads(b)), "|"), "alpha|bravo|charlie"; got != want {
		t.Errorf("keys(b) = %q, want %q", got, want)
	}
	if a[0].Name != "charlie-api" {
		t.Errorf("input was mutated: a[0] = %q, want %q", a[0].Name, "charlie-api")
	}
}

// TestRenderedGroups_ClampsToRenderedCount は表示上限で truncate されたエントリが
// 差分計算から除外され、rendered が未設定 / 過大でも panic しないことを検証する。
// rendered は merge 後のエントリ数なので、旧 Modal (workload 実数) 由来の
// 過大な値が飛んできても安全に扱えなければならない。
func TestRenderedGroups_ClampsToRenderedCount(t *testing.T) {
	t.Parallel()

	groups := groupWorkloads([]*model.Workload{wl("a", 0), wl("b", 0), wl("c", 0)})

	cases := []struct {
		name     string
		rendered int
		want     int
	}{
		{name: "unset falls back to all", rendered: 0, want: 3},
		{name: "negative falls back to all", rendered: -1, want: 3},
		{name: "truncated", rendered: 2, want: 2},
		{name: "exact", rendered: 3, want: 3},
		{name: "stale larger count is clamped", rendered: 99, want: 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := len(renderedGroups(groups, tc.rendered)); got != tc.want {
				t.Errorf("renderedGroups(_, %d) = %d groups, want %d", tc.rendered, got, tc.want)
			}
		})
	}
}

// TestWorkloadGroup_Label は merge 済みエントリのラベルが起動状況を表すことを検証する。
func TestWorkloadGroup_Label(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []*model.Workload
		want string
	}{
		{
			name: "all running",
			in:   []*model.Workload{wl("menu-api", 1), wl("menu-app", 1)},
			want: "🟢 menu (api, app)",
		},
		{
			name: "partially running",
			in:   []*model.Workload{wl("menu-api", 1), wl("menu-app", 0)},
			want: "🟡 menu (api, app) 1/2 起動中",
		},
		{
			name: "all stopped",
			in:   []*model.Workload{wl("menu-api", 0), wl("menu-app", 0)},
			want: "🔴 menu (api, app)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			groups := groupWorkloads(tc.in)
			if len(groups) != 1 {
				t.Fatalf("groups: got %d, want 1", len(groups))
			}
			if got := groups[0].label(); got != tc.want {
				t.Errorf("label() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWorkloadGroup_Label_TruncatedToSlackLimit は長い base 名でも Slack の
// option text 上限 (75 文字) を超えないことを検証する。
func TestWorkloadGroup_Label_TruncatedToSlackLimit(t *testing.T) {
	t.Parallel()

	base := strings.Repeat("long-name-", 8) // 80 文字
	groups := groupWorkloads([]*model.Workload{wl(base+"-api", 1), wl(base+"-app", 0)})
	if len(groups) != 1 {
		t.Fatalf("groups: got %d, want 1", len(groups))
	}
	if runeLen := len([]rune(groups[0].label())); runeLen > slackOptionTextMaxLen {
		t.Errorf("label() has %d runes, want <= %d", runeLen, slackOptionTextMaxLen)
	}
}

// TestWorkloadGroup_ChangeLabel はプレビュー行が変更対象の suffix だけを列挙することを検証する。
func TestWorkloadGroup_ChangeLabel(t *testing.T) {
	t.Parallel()

	g := groupWorkloads([]*model.Workload{
		wl("checkout-api", 1), wl("checkout-app", 0), wl("checkout-worker", 0),
	})[0]

	if got, want := g.changeLabel([]string{"checkout-app", "checkout-worker"}), "checkout (app, worker)"; got != want {
		t.Errorf("changeLabel(partial) = %q, want %q", got, want)
	}

	single := groupWorkloads([]*model.Workload{wl("bedrock-hello", 0)})[0]
	if got, want := single.changeLabel([]string{"bedrock-hello"}), "bedrock-hello"; got != want {
		t.Errorf("changeLabel(single) = %q, want %q", got, want)
	}
}

// TestWorkloadGroup_AnyRunning は部分起動でもチェックが入ることを検証する。
// 部分起動でチェックを外すと「変更なしで Plan → 稼働中メンバーが停止」になるため。
func TestWorkloadGroup_AnyRunning(t *testing.T) {
	t.Parallel()

	partial := groupWorkloads([]*model.Workload{wl("menu-api", 1), wl("menu-app", 0)})[0]
	if !partial.anyRunning() {
		t.Errorf("partial group: anyRunning() = false, want true")
	}

	stopped := groupWorkloads([]*model.Workload{wl("menu-api", 0), wl("menu-app", 0)})[0]
	if stopped.anyRunning() {
		t.Errorf("stopped group: anyRunning() = true, want false")
	}
}

// TestGroupWorkloads_ReducesCheckboxBlocks は merge によって checkbox の
// 10 件上限に対する分割数が減ることを確認する。
func TestGroupWorkloads_ReducesCheckboxBlocks(t *testing.T) {
	t.Parallel()

	// 6 サービス × (api, app) = 12 workload → merge すると 6 エントリ = 1 block
	var in []*model.Workload
	for _, svc := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"} {
		in = append(in, wl(svc+"-api", 0), wl(svc+"-app", 0))
	}

	if got := len(workloadCheckboxBlocks("labs", groupWorkloads(in))); got != 1 {
		t.Errorf("merged: block count = %d, want 1", got)
	}
	// merge しない場合は 12 options = 2 block 必要だったことを対比として記録する。
	if len(in) <= checkboxMaxOptions {
		t.Fatalf("test premise broken: %d workloads should exceed %d", len(in), checkboxMaxOptions)
	}
}

// TestNamespaceInputBlock_SortedAToZ は namespace ドロップダウンが a→z 順で並び、
// 選択済み namespace が初期選択になることを検証する。
func TestNamespaceInputBlock_SortedAToZ(t *testing.T) {
	t.Parallel()

	in := []string{"labs", "inhouse-solutions", "gees", "acme"}
	block := namespaceInputBlock(in, "labs")

	sel, ok := block.Element.(*slack.SelectBlockElement)
	if !ok {
		t.Fatalf("block.Element: got %T, want *slack.SelectBlockElement", block.Element)
	}

	var got []string
	for _, opt := range sel.Options {
		got = append(got, opt.Value)
	}
	want := []string{"acme", "gees", "inhouse-solutions", "labs"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("namespace options:\n got %v\nwant %v", got, want)
	}

	if sel.InitialOption == nil || sel.InitialOption.Value != "labs" {
		t.Errorf("InitialOption: got %+v, want value=labs", sel.InitialOption)
	}

	// 呼び出し元のスライスを破壊しない (store のキャッシュを共有する可能性がある)。
	if in[0] != "labs" {
		t.Errorf("input slice was mutated: %v", in)
	}
}

func groupKeys(groups []workloadGroup) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.Key)
	}
	return out
}
