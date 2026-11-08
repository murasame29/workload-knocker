package store

import (
	"context"
	"testing"
	"time"

	kedafake "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned/fake"
	"k8s.io/client-go/kubernetes/fake"
	"pgregory.net/rapid"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
	nrx "github.com/murasame29/workload-knocker/pkg/newrelic"
)

// buildMultiStoreFromFixture は固定的に N 個の cluster を持つ MultiStore を返す。
// 各 cluster は同じ namespace 構成 (ns-1, ns-2) と workload (wl-a, wl-b) を持つ。
// cluster.Env で一意性を確保する。
func buildMultiStoreFromFixture(t *rapid.T, clusterCount int) (*MultiStore, []config.Cluster) {
	t.Helper()
	clusters := make([]config.Cluster, 0, clusterCount)
	for i := 0; i < clusterCount; i++ {
		env := rapid.StringMatching(`[a-z][a-z0-9-]{0,4}`).Draw(t, "env")
		clusters = append(clusters, config.Cluster{
			Env:  env,
			Name: env + "-cluster",
			Provider: config.ClusterProvider{
				Type: config.ProviderTypeEKS,
				EKS:  &config.ProviderEKS{Region: "ap-northeast-1"},
			},
			Slack: config.ClusterSlack{
				SlashCommand: "/workload-knocker-" + env,
			},
		})
	}
	// env / slashCommand の一意性確保
	seenEnv := map[string]bool{}
	seenSC := map[string]bool{}
	uniq := make([]config.Cluster, 0, len(clusters))
	for _, c := range clusters {
		if seenEnv[c.Env] || seenSC[c.Slack.SlashCommand] {
			continue
		}
		seenEnv[c.Env] = true
		seenSC[c.Slack.SlashCommand] = true
		uniq = append(uniq, c)
	}
	if len(uniq) == 0 {
		uniq = []config.Cluster{
			{
				Env:      "fixture",
				Name:     "fixture-cluster",
				Provider: config.ClusterProvider{Type: config.ProviderTypeEKS, EKS: &config.ProviderEKS{Region: "ap-northeast-1"}},
				Slack:    config.ClusterSlack{SlashCommand: "/fixture"},
			},
		}
	}
	cfg := &config.Config{
		Controller: config.Controller{Namespace: testControllerNs},
		Clusters:   uniq,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	reg := cfg

	stores := map[string]*ClusterStore{}
	m := &fakeMetrics{}
	for _, c := range uniq {
		kc := fake.NewClientset()
		kk := kedafake.NewSimpleClientset(
			makeScaledObject("ns-1", "wl-a"),
			makeScaledObject("ns-1", "wl-b"),
			makeScaledObject("ns-2", "wl-a"),
		)
		stores[c.Env] = NewClusterStore(c, k8s.NewKEDAScaledObjectLister(kk), k8s.NewConfigMapStateRepository(kc, testControllerNs), m, nrx.New(nil))
	}
	ms, err := NewMultiStoreFromClusterStores(reg, m, stores, 10*time.Second)
	if err != nil {
		t.Fatalf("NewMultiStoreFromClusterStores: %v", err)
	}
	if err := ms.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return ms, uniq
}

// TestProperty_ClusterIsolation_Start は cluster 境界を検証する:
// ある cluster で Start を呼んでも、他 cluster の workload は影響を受けない。
func TestProperty_ClusterIsolation_Start(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping property test in -short mode")
	}
	rapid.Check(t, func(t *rapid.T) {
		count := rapid.IntRange(2, 4).Draw(t, "clusterCount")
		ms, clusters := buildMultiStoreFromFixture(t, count)
		if len(clusters) < 2 {
			return
		}

		idx := rapid.IntRange(0, len(clusters)-1).Draw(t, "idx")
		target := clusters[idx]
		nsName := rapid.SampledFrom([]string{"ns-1", "ns-2"}).Draw(t, "ns")
		wlName := rapid.SampledFrom([]string{"wl-a", "wl-b"}).Draw(t, "wl")

		if nsName == "ns-2" && wlName == "wl-b" {
			return
		}

		if !ms.Start(target.Env, nsName, wlName, "user1") {
			t.Fatalf("Start(%s, %s, %s) returned false", target.Env, nsName, wlName)
		}

		wlsTarget := ms.WorkloadsByNamespace(target.Env, nsName)
		var found bool
		for _, w := range wlsTarget {
			if w.Name == wlName && w.Replicas == 1 && w.StartedBy == "user1" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("target cluster %s did not reflect Start", target.Env)
		}

		for _, c := range clusters {
			if c.Env == target.Env {
				continue
			}
			wls := ms.WorkloadsByNamespace(c.Env, nsName)
			for _, w := range wls {
				if w.Name == wlName && (w.Replicas != 0 || w.StartedBy != "") {
					t.Errorf("cluster %s leaked: workload %s has Replicas=%d StartedBy=%q (cluster boundary violated)",
						c.Env, w.Name, w.Replicas, w.StartedBy)
				}
			}
		}
	})
}

// TestProperty_StartIdempotent は冪等性を検証する:
// 同一引数で Start を複数回呼んでも、最終状態は同じ (replicas=1, startedBy=user)。
func TestProperty_StartIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping property test in -short mode")
	}
	rapid.Check(t, func(t *rapid.T) {
		ms, clusters := buildMultiStoreFromFixture(t, 1)
		c := clusters[0]
		repeats := rapid.IntRange(2, 5).Draw(t, "repeats")

		for i := 0; i < repeats; i++ {
			if !ms.Start(c.Env, "ns-1", "wl-a", "alice") {
				t.Fatalf("Start iter %d: false", i)
			}
		}

		wls := ms.WorkloadsByNamespace(c.Env, "ns-1")
		var (
			found bool
			gotR  int
			gotU  string
		)
		for _, x := range wls {
			if x.Name == "wl-a" {
				gotR = x.Replicas
				gotU = x.StartedBy
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("workload wl-a not found")
		}
		if gotR != 1 || gotU != "alice" {
			t.Errorf("after %d Start calls: Replicas=%d StartedBy=%q, want 1, alice",
				repeats, gotR, gotU)
		}
	})
}

// TestProperty_StopAllIdempotent は StopAll の冪等性を検証する:
// StopAll を 2 回連続で呼んでも、2 回目以降は新たな停止 (Stopped 数 0) になる。
func TestProperty_StopAllIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping property test in -short mode")
	}
	rapid.Check(t, func(t *rapid.T) {
		ms, clusters := buildMultiStoreFromFixture(t, 1)
		c := clusters[0]

		for _, ns := range []string{"ns-1", "ns-2"} {
			for _, wl := range []string{"wl-a", "wl-b"} {
				if ns == "ns-2" && wl == "wl-b" {
					continue
				}
				ms.Start(c.Env, ns, wl, "alice")
			}
		}

		first := ms.StopAll(context.Background())
		if first.TotalStopped == 0 {
			t.Fatalf("first StopAll: TotalStopped=0, want >0")
		}

		second := ms.StopAll(context.Background())
		if second.TotalStopped != 0 {
			t.Errorf("second StopAll: TotalStopped=%d, want 0 (idempotent)", second.TotalStopped)
		}
	})
}
