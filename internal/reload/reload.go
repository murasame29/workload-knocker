package reload

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/store"
)

// Registry reloads the cluster registry file into the live config and store set.
// Reload is not safe for concurrent use; a Watcher serializes calls.
type Registry struct {
	Path    string
	Env     *config.Env
	Config  *config.Holder
	Store   *store.MultiStore
	Build   store.ClusterFactory
	Metrics metrics.MetricsManager

	last []byte
}

// Seed records the file content in use at startup so an identical event is ignored.
func (r *Registry) Seed() { r.last, _ = os.ReadFile(r.Path) }

// Reload applies the registry file; on any error the previous config stays active.
func (r *Registry) Reload(ctx context.Context) {
	data, err := os.ReadFile(r.Path)
	if err != nil {
		r.fail(fmt.Errorf("read cluster registry file %s: %w", r.Path, err))
		return
	}
	if r.last != nil && bytes.Equal(data, r.last) {
		slog.Debug("cluster registry file unchanged")
		return
	}
	applied, retry, err := r.apply(ctx, data)
	if !retry {
		r.last = data
	}
	switch {
	case err != nil:
		r.fail(err)
	case applied:
		r.Metrics.IncConfigReload("registry", "success")
	}
}

func (r *Registry) fail(err error) {
	slog.Error("cluster registry reload failed, keeping the previous config", slog.String("path", r.Path), slog.String("error.message", err.Error()))
	r.Metrics.IncConfigReload("registry", "error")
}

// apply returns applied=false when the content is equivalent to the running config, and
// retry=true when the failure depends on cluster state rather than on the file content.
func (r *Registry) apply(ctx context.Context, data []byte) (applied, retry bool, err error) {
	next, err := config.ParseValidated(r.Path, data)
	if err != nil {
		return false, false, err
	}
	cur := r.Config.Load()
	if err := RestartRequired(cur, next, r.Env); err != nil {
		return false, false, err
	}
	if reflect.DeepEqual(cur.Controller, next.Controller) && reflect.DeepEqual(cur.Clusters, next.Clusters) {
		slog.Debug("cluster registry content unchanged")
		return false, false, nil
	}
	res, err := r.Store.Reload(ctx, next, r.Build)
	if err != nil {
		return false, true, err
	}
	r.Config.Store(next)
	slog.Info("cluster registry reloaded",
		slog.Any("clusters.added", res.Added), slog.Any("clusters.removed", res.Removed),
		slog.Any("clusters.changed", res.Changed), slog.Any("clusters.kept_previous", res.Kept),
		slog.Int("cluster.count", len(next.Clusters)))
	return true, false, nil
}

// RestartRequired returns an error naming why next cannot be applied without a restart.
func RestartRequired(cur, next *config.Config, env *config.Env) error {
	if cur.Controller.Namespace != next.Controller.Namespace {
		return fmt.Errorf("controller.namespace changed (%q -> %q): restart required", cur.Controller.Namespace, next.Controller.Namespace)
	}
	var added []string
	for _, f := range next.EnabledFrontends() {
		if !cur.FrontendEnabled(f) {
			added = append(added, f)
		}
	}
	if len(added) > 0 {
		return fmt.Errorf("frontend %s is newly enabled: restart required", strings.Join(added, ", "))
	}
	return env.ValidateFrontends(next)
}

// Authz reloads the authorization policy file.
// Reload is not safe for concurrent use; a Watcher serializes calls.
type Authz struct {
	Path    string
	Policy  *authz.Holder
	Metrics metrics.MetricsManager

	last []byte
}

// Seed records the file content in use at startup so an identical event is ignored.
func (a *Authz) Seed() { a.last, _ = os.ReadFile(a.Path) }

// Reload applies the policy file; on any error (including a deleted file) the previous policy stays.
func (a *Authz) Reload(context.Context) {
	data, err := os.ReadFile(a.Path)
	if err != nil {
		a.fail(fmt.Errorf("read authz policy %s: %w", a.Path, err))
		return
	}
	if a.last != nil && bytes.Equal(data, a.last) {
		slog.Debug("authorization policy file unchanged")
		return
	}
	a.last = data
	p, err := authz.ParseFile(a.Path, data)
	if err != nil {
		a.fail(err)
		return
	}
	was := a.Policy.Policy()
	a.Policy.Store(p)
	a.Metrics.IncConfigReload("authz", "success")
	slog.Info("authorization policy reloaded",
		slog.Int("rules", p.Rules()), slog.Bool("default.allow", p.DefaultAllow()), slog.Bool("was_enabled", was != nil))
}

func (a *Authz) fail(err error) {
	slog.Error("authorization policy reload failed, keeping the previous policy", slog.String("path", a.Path), slog.String("error.message", err.Error()))
	a.Metrics.IncConfigReload("authz", "error")
}
