package authz

import (
	"os"
	"testing"
)

// TestExamplePolicyParses keeps example/authz.example.yaml valid.
func TestExamplePolicyParses(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../../example/authz.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if p.Rules() != 3 {
		t.Errorf("rules = %d, want 3", p.Rules())
	}
}
