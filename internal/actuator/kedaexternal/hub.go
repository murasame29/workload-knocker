// Package kedaexternal is the keda-external actuator: it serves KEDA's external scaler gRPC protocol
// and answers each ScaledObject's external-push trigger with the desired state of its workload,
// without Prometheus. See docs/en/configuration.md for the ScaledObject trigger.
package kedaexternal

import (
	"maps"
	"sync"
	"time"

	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/metrics"
)

const (
	// DefaultAddressMatch is the scalerAddress substring that selects triggers served by this controller.
	DefaultAddressMatch = "workload-knocker"
	defaultKeepalive    = 30 * time.Second
)

// key identifies a workload or a ScaledObject within one cluster.
type key struct{ namespace, name string }

// clusterState is what the hub knows about one cluster, keyed by config.Cluster.Env.
type clusterState struct {
	name    string
	metrics metrics.MetricsManager
	running map[key]bool
	// aliases maps a ScaledObject to its workload for triggers without explicit metadata.
	aliases map[key]key
	streams int
}

// Hub holds the desired state of every keda-external cluster and serves it to KEDA. One Hub is
// shared by all clusters of the process.
type Hub struct {
	mu        sync.Mutex
	clusters  map[string]*clusterState
	changed   chan struct{} // closed and replaced on every state change
	match     string
	keepalive time.Duration
	done      chan struct{}
	closeOnce sync.Once

	pb.UnimplementedExternalScalerServer
}

var shared = NewHub()

// Shared returns the process-wide Hub that the factory registers clusters with.
func Shared() *Hub { return shared }

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{
		clusters:  map[string]*clusterState{},
		changed:   make(chan struct{}),
		match:     DefaultAddressMatch,
		keepalive: defaultKeepalive,
		done:      make(chan struct{}),
	}
}

// SetAddressMatch sets the scalerAddress substring that selects triggers served by this controller.
func (h *Hub) SetAddressMatch(match string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.match = match
}

func (h *Hub) addressMatch() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.match
}

// Close ends every open stream so that KEDA reconnects to the next instance.
func (h *Hub) Close() { h.closeOnce.Do(func() { close(h.done) }) }

// register adds or refreshes the cluster c; state published earlier for the same env is kept.
func (h *Hub) register(c config.Cluster, m metrics.MetricsManager) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cs := h.clusterLocked(c.Env)
	cs.name, cs.metrics = c.Name, m
}

func (h *Hub) clusterLocked(env string) *clusterState {
	cs, ok := h.clusters[env]
	if !ok {
		cs = &clusterState{running: map[key]bool{}, aliases: map[key]key{}}
		h.clusters[env] = cs
	}
	return cs
}

func (h *Hub) setRunning(env string, k key, running bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cs := h.clusterLocked(env)
	if v, ok := cs.running[k]; ok && v == running {
		return
	}
	cs.running[k] = running
	h.notifyLocked()
}

func (h *Hub) forget(env string, k key) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cs := h.clusterLocked(env)
	if _, ok := cs.running[k]; !ok {
		return
	}
	delete(cs.running, k)
	h.notifyLocked()
}

func (h *Hub) setAliases(env string, aliases map[key]key) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cs := h.clusterLocked(env)
	if maps.Equal(cs.aliases, aliases) {
		return
	}
	cs.aliases = aliases
	h.notifyLocked()
}

func (h *Hub) notifyLocked() {
	close(h.changed)
	h.changed = make(chan struct{})
}

// lookup returns the desired state for t, whether t is a known cluster and workload, and a channel
// closed on the next state change.
func (h *Hub) lookup(t target) (running, found bool, changed <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cs, ok := h.clusters[t.env]
	if !ok {
		return false, false, h.changed
	}
	running, found = cs.running[t.resolve(cs)]
	return running, found, h.changed
}

// knownCluster reports whether env is registered.
func (h *Hub) knownCluster(env string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.clusters[env]
	return ok
}

// streamOpened and streamClosed track the open streams of env for the metric.
func (h *Hub) streamOpened(env string) { h.addStreams(env, 1) }
func (h *Hub) streamClosed(env string) { h.addStreams(env, -1) }

func (h *Hub) addStreams(env string, delta int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cs, ok := h.clusters[env]
	if !ok {
		return
	}
	cs.streams += delta
	if cs.metrics != nil {
		cs.metrics.SetExternalScalerStreams(cs.name, cs.streams)
	}
}
