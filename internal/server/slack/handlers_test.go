package slackbot

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/model"
)

// helperConfig は handlers_test 共通の Cluster Registry 設定を返す (validate 済み)。
func helperConfig(t *testing.T, cs ...config.Cluster) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Controller: config.Controller{Namespace: "workload-knocker"},
		Clusters:   cs,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return cfg
}

// TestRegistryLookup_UnknownCommandReturnsNotFound は未登録のスラッシュコマンドが
// registry で解決できないことを検証する。
func TestRegistryLookup_UnknownCommandReturnsNotFound(t *testing.T) {
	t.Parallel()

	reg := helperConfig(t,
		config.Cluster{
			Env:      "stg",
			Name:     "stg-cluster",
			Provider: config.ClusterProvider{Type: config.ProviderTypeEKS, EKS: &config.ProviderEKS{Region: "ap-northeast-1"}},
			Slack:    config.ClusterSlack{SlashCommand: "/workload-knocker-stg"},
		},
	)

	if _, ok := reg.BySlashCommand("/unknown"); ok {
		t.Errorf("BySlashCommand(/unknown): got found, want not found")
	}
}

// TestRegistryLookup_KnownCommandResolvesCluster は登録済みのスラッシュコマンドが
// 期待通りの cluster を返すことを検証する。handleSlashCommand が Modal の
// private_metadata に使う Env / Name を信頼できることを保証する。
func TestRegistryLookup_KnownCommandResolvesCluster(t *testing.T) {
	t.Parallel()

	want := config.Cluster{
		Env:      "stg",
		Name:     "example-staging",
		Provider: config.ClusterProvider{Type: config.ProviderTypeEKS, EKS: &config.ProviderEKS{Region: "ap-northeast-1"}},
		Slack:    config.ClusterSlack{SlashCommand: "/workload-knocker-stg"},
	}

	reg := helperConfig(t, want)

	got, ok := reg.BySlashCommand("/workload-knocker-stg")
	if !ok {
		t.Fatalf("BySlashCommand(/workload-knocker-stg): got not found, want found")
	}
	if got.Env != want.Env {
		t.Errorf("cluster.Env: got %q, want %q", got.Env, want.Env)
	}
	if got.Name != want.Name {
		t.Errorf("cluster.Name: got %q, want %q", got.Name, want.Name)
	}
}

// TestModalMetadata_RoundTrip は Modal の private_metadata に格納する modalMetadata の
// JSON シリアライズ/デシリアライズで cluster (= env) フィールドが保持されることを検証する。
func TestModalMetadata_RoundTrip(t *testing.T) {
	t.Parallel()

	original := modalMetadata{
		ChannelID: "C12345",
		UserID:    "U67890",
		Cluster:   "stg",
		Namespace: "inhouse-solutions",
	}

	bytes, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var got modalMetadata
	if err := json.Unmarshal(bytes, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if got != original {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, original)
	}
}

// TestModalMetadata_OmitsEmptyNamespace は namespace 未設定時に omitempty が効いて
// private_metadata のサイズを抑えられることを確認する。
func TestModalMetadata_OmitsEmptyNamespace(t *testing.T) {
	t.Parallel()

	meta := modalMetadata{
		ChannelID: "C12345",
		UserID:    "U67890",
		Cluster:   "stg",
	}

	bytes, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	if strings.Contains(string(bytes), "namespace") {
		t.Errorf("expected namespace to be omitted, got: %s", string(bytes))
	}
	if !strings.Contains(string(bytes), `"cluster":"stg"`) {
		t.Errorf("expected cluster to be present, got: %s", string(bytes))
	}
}

// TestPreviewMetadata_NameOnlyEncodingBuysHeadroom は ToStart / ToStop を
// workload 名のみで持つ設計により、10 件超の差分が private_metadata (3000 文字) に
// 収まることを検証する。同じ差分を "cluster/namespace/name" のフル tuple で持つと
// 上限を超えるため、この encoding が必須であることも合わせて記録する。
func TestPreviewMetadata_NameOnlyEncodingBuysHeadroom(t *testing.T) {
	t.Parallel()

	const count = 50 // 実運用で想定される上限規模
	names := make([]string, 0, count)
	tuples := make([]string, 0, count)
	for i := range count {
		// 実運用相当の長さ (40 文字) の workload 名
		name := fmt.Sprintf("security-checksheet-search-app-%09d", i)
		names = append(names, name)
		tuples = append(tuples, "stg/inhouse-solutions/"+name)
	}

	base := previewMetadata{
		ChannelID: "C12345",
		UserID:    "U67890",
		Cluster:   "stg",
		Namespace: "inhouse-solutions",
	}

	nameOnly := base
	nameOnly.ToStart = names[:count/2]
	nameOnly.ToStop = names[count/2:]

	got, err := json.Marshal(nameOnly)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if len(got) > slackPrivateMetadataMaxLen {
		t.Errorf("name-only private_metadata = %d chars, want <= %d", len(got), slackPrivateMetadataMaxLen)
	}

	fullTuple := base
	fullTuple.ToStart = tuples[:count/2]
	fullTuple.ToStop = tuples[count/2:]

	legacy, err := json.Marshal(fullTuple)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if len(legacy) <= slackPrivateMetadataMaxLen {
		t.Errorf("full-tuple private_metadata = %d chars; expected it to exceed %d "+
			"(if this no longer holds, the name-only encoding rationale needs revisiting)",
			len(legacy), slackPrivateMetadataMaxLen)
	}
}

// TestWorkloadCheckboxBlocks_SplitsAtSlackLimit は checkboxes element の 10 options
// 上限を超えないように block が分割され、かつ全 workload が 1 度ずつ現れることを検証する。
func TestWorkloadCheckboxBlocks_SplitsAtSlackLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		count      int
		wantBlocks int
	}{
		{count: 0, wantBlocks: 0},
		{count: 1, wantBlocks: 1},
		{count: 10, wantBlocks: 1},
		{count: 11, wantBlocks: 2},
		{count: 20, wantBlocks: 2},
		{count: 25, wantBlocks: 3},
		{count: maxWorkloadOptions, wantBlocks: maxWorkloadCheckboxBlocks},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("count=%d", tc.count), func(t *testing.T) {
			t.Parallel()

			blocks := workloadCheckboxBlocks("ns", groupWorkloads(makeWorkloads(tc.count)))
			if len(blocks) != tc.wantBlocks {
				t.Fatalf("block count = %d, want %d", len(blocks), tc.wantBlocks)
			}

			seenName := make(map[string]int)
			seenBlockID := make(map[string]bool)
			seenActionID := make(map[string]bool)

			for i, b := range blocks {
				input, ok := b.(*slack.InputBlock)
				if !ok {
					t.Fatalf("block %d: type = %T, want *slack.InputBlock", i, b)
				}
				if !input.Optional {
					t.Errorf("block %d: Optional = false, want true", i)
				}
				if seenBlockID[input.BlockID] {
					t.Errorf("block %d: duplicated block_id %q", i, input.BlockID)
				}
				seenBlockID[input.BlockID] = true
				if !strings.HasPrefix(input.BlockID, blockSelectWorkloads) {
					t.Errorf("block %d: block_id %q lacks prefix %q", i, input.BlockID, blockSelectWorkloads)
				}

				cb, ok := input.Element.(*slack.CheckboxGroupsBlockElement)
				if !ok {
					t.Fatalf("block %d: element type = %T, want *slack.CheckboxGroupsBlockElement", i, input.Element)
				}
				if seenActionID[cb.ActionID] {
					t.Errorf("block %d: duplicated action_id %q", i, cb.ActionID)
				}
				seenActionID[cb.ActionID] = true
				if !strings.HasPrefix(cb.ActionID, actionSelectWorkloads) {
					t.Errorf("block %d: action_id %q lacks prefix %q", i, cb.ActionID, actionSelectWorkloads)
				}
				if len(cb.Options) > checkboxMaxOptions {
					t.Errorf("block %d: %d options, want <= %d", i, len(cb.Options), checkboxMaxOptions)
				}
				for _, opt := range cb.Options {
					seenName[opt.Value]++
					if len(opt.Value) > 75 {
						t.Errorf("option value %q is %d chars, want <= 75", opt.Value, len(opt.Value))
					}
				}
			}

			if len(seenName) != tc.count {
				t.Errorf("distinct options = %d, want %d", len(seenName), tc.count)
			}
			for name, n := range seenName {
				if n != 1 {
					t.Errorf("option %q appeared %d times, want 1", name, n)
				}
			}
		})
	}
}

// TestWorkloadCheckboxBlocks_InitialOptionsReflectRunning は稼働中の workload だけが
// initial_options に入ることを検証する。空スライスは Slack が拒否するため nil であること。
func TestWorkloadCheckboxBlocks_InitialOptionsReflectRunning(t *testing.T) {
	t.Parallel()

	workloads := makeWorkloads(15)
	// 偶数 index のみ稼働中にする
	running := make(map[string]bool)
	for i, w := range workloads {
		if i%2 == 0 {
			w.Replicas = 1
			running[w.Name] = true
		}
	}

	gotInitial := make(map[string]bool)
	for _, b := range workloadCheckboxBlocks("ns", groupWorkloads(workloads)) {
		cb := b.(*slack.InputBlock).Element.(*slack.CheckboxGroupsBlockElement)
		if cb.InitialOptions != nil && len(cb.InitialOptions) == 0 {
			t.Error("InitialOptions is an empty slice, want nil (Slack rejects [])")
		}
		for _, opt := range cb.InitialOptions {
			gotInitial[opt.Value] = true
		}
	}

	if len(gotInitial) != len(running) {
		t.Fatalf("initial options = %d, want %d", len(gotInitial), len(running))
	}
	for name := range running {
		if !gotInitial[name] {
			t.Errorf("running workload %q missing from initial_options", name)
		}
	}
}

// TestWorkloadCheckboxBlocks_FitsModalBlockLimit は上限件数を描画しても
// Modal の 100 block 制限に収まることを検証する。
func TestWorkloadCheckboxBlocks_FitsModalBlockLimit(t *testing.T) {
	t.Parallel()

	blocks := workloadCheckboxBlocks("ns", groupWorkloads(makeWorkloads(maxWorkloadOptions)))
	total := len(blocks) + reservedModalBlocks // namespace select + 警告 section
	if total > slackModalMaxBlocks {
		t.Errorf("total blocks = %d, want <= %d", total, slackModalMaxBlocks)
	}
}

// TestSelectedWorkloadNames_CollectsAcrossBlocks は分割された複数 checkbox block の
// 選択状態が 1 つの集合に畳み込まれ、無関係な block が混入しないことを検証する。
func TestSelectedWorkloadNames_CollectsAcrossBlocks(t *testing.T) {
	t.Parallel()

	values := map[string]map[string]slack.BlockAction{
		blockSelectWorkloads + "_0": {
			actionSelectWorkloads + "_0": {SelectedOptions: []slack.OptionBlockObject{
				{Value: "app-a"}, {Value: "app-b"},
			}},
		},
		blockSelectWorkloads + "_1": {
			actionSelectWorkloads + "_1": {SelectedOptions: []slack.OptionBlockObject{
				{Value: "app-c"},
			}},
		},
		// 分割前の単一 block 形式も prefix 一致で回収できる
		blockSelectWorkloads: {
			actionSelectWorkloads: {SelectedOptions: []slack.OptionBlockObject{
				{Value: "app-d"},
			}},
		},
		// namespace select は対象外
		blockSelectNamespace: {
			actionSelectNamespace: {SelectedOption: slack.OptionBlockObject{Value: "ns"}},
		},
	}

	got := selectedWorkloadNames(values)
	want := []string{"app-a", "app-b", "app-c", "app-d"}

	if len(got) != len(want) {
		t.Fatalf("selected count = %d, want %d (%v)", len(got), len(want), got)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("%q not selected", name)
		}
	}
	if got["ns"] {
		t.Error("namespace select value leaked into workload selection")
	}
}

// TestSortedWorkloads_IsDeterministic は store の map 走査順に依存せず
// name 昇順で安定した並びになることを検証する。checkbox の分割境界と
// submit 時の truncate 位置を一致させる前提。
func TestSortedWorkloads_IsDeterministic(t *testing.T) {
	t.Parallel()

	input := []*model.Workload{
		{Name: "charlie"}, {Name: "alpha"}, {Name: "bravo"},
	}
	want := []string{"alpha", "bravo", "charlie"}

	got := sortedWorkloads(input)
	for i, w := range got {
		if w.Name != want[i] {
			t.Errorf("index %d: got %q, want %q", i, w.Name, want[i])
		}
	}
	// 入力スライスを破壊しないこと (store が返す実体を並べ替えないため)
	if input[0].Name != "charlie" {
		t.Errorf("input was mutated: input[0] = %q, want %q", input[0].Name, "charlie")
	}
}

// TestMrkdwnSections_RespectsTextLimit は section block の 3000 文字上限を超えないよう
// 分割され、全行が保持されることを検証する。
func TestMrkdwnSections_RespectsTextLimit(t *testing.T) {
	t.Parallel()

	lines := make([]string, 0, maxWorkloadOptions)
	for i := range maxWorkloadOptions {
		lines = append(lines, fmt.Sprintf("🟢 security-checksheet-search-app-%09d: 停止 → *起動*", i))
	}

	blocks := mrkdwnSections("header", lines)
	if len(blocks) < 2 {
		t.Fatalf("block count = %d, want >= 2 (should have split)", len(blocks))
	}

	var joined string
	for i, b := range blocks {
		section, ok := b.(*slack.SectionBlock)
		if !ok {
			t.Fatalf("block %d: type = %T, want *slack.SectionBlock", i, b)
		}
		if len(section.Text.Text) > slackSectionTextMaxLen {
			t.Errorf("block %d: text is %d chars, want <= %d", i, len(section.Text.Text), slackSectionTextMaxLen)
		}
		joined += section.Text.Text
	}

	if !strings.Contains(joined, "header") {
		t.Error("header missing from rendered sections")
	}
	for _, line := range lines {
		if !strings.Contains(joined, line) {
			t.Errorf("line %q missing from rendered sections", line)
		}
	}
}

// makeWorkloads は停止状態の workload を n 件生成する (name は昇順)。
func makeWorkloads(n int) []*model.Workload {
	out := make([]*model.Workload, 0, n)
	for i := range n {
		out = append(out, &model.Workload{
			Env:       "stg",
			Namespace: "ns",
			Name:      fmt.Sprintf("app-%03d", i),
			Kind:      "Deployment",
		})
	}
	return out
}

// TestModalTitle_FitsWithinSlackLimit は modalTitle が Slack の plain_text title 上限
// (24 文字) を超えないことを検証する。OpenView が invalid_arguments で失敗するのを防ぐ。
func TestModalTitle_FitsWithinSlackLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cl   config.Cluster
	}{
		{
			name: "short env",
			cl:   config.Cluster{Env: "stg", Name: "example-staging"},
		},
		{
			name: "us env",
			cl:   config.Cluster{Env: "stg-us", Name: "example-staging"},
		},
		{
			name: "long env truncated",
			cl:   config.Cluster{Env: "extremely-long-env-key-that-overflows", Name: "n"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			title := modalTitle(tc.cl)
			runeLen := len([]rune(title))
			if runeLen > slackModalTitleMaxLen {
				t.Errorf("modalTitle(%+v): %q has %d runes, want <= %d", tc.cl, title, runeLen, slackModalTitleMaxLen)
			}
		})
	}
}
