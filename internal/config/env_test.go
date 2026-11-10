package config

import (
	"slices"
	"testing"
)

func TestGitOpsManagers(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want []string
	}{
		{"unset", "", []string{"argocd-controller", "kustomize-controller", "helm-controller"}},
		{"blank", "  ", []string{"argocd-controller", "kustomize-controller", "helm-controller"}},
		{"custom", "flux, other ,,", []string{"flux", "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITOPS_MANAGERS", tc.env)
			if got := GitOpsManagers(); !slices.Equal(got, tc.want) {
				t.Errorf("GitOpsManagers = %v, want %v", got, tc.want)
			}
		})
	}
}
