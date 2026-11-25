package commands

import (
	"os"
	"path/filepath"
	"testing"

	"axon/internal/official"
)

// TestDeriveLive prints the recipes derived from a real cluster repository when DERIVE_LIVE_CLUSTER is set.
func TestDeriveLive(t *testing.T) {
	dir := os.Getenv("DERIVE_LIVE_CLUSTER")
	if dir == "" {
		t.Skip("DERIVE_LIVE_CLUSTER not set")
	}
	l, ok, err := official.Detect(dir)
	if err != nil || !ok {
		t.Fatal(err)
	}
	rs, notes := deriveLocalBuilds(dir, filepath.Dir(dir), l)
	for _, r := range rs {
		t.Logf("%s: file=%s ctx=%s stage=%q contexts=%v args=%v watch=%v", r.Targets[0], r.Dockerfile, r.Context, r.Target, r.Contexts, r.Args, r.Watch)
	}
	for _, n := range notes {
		t.Logf("note: %s", n)
	}
}
