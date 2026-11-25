package commands

import (
	"testing"

	"axon/internal/official"
)

func obj(kind, n string) official.Object {
	return official.Object{"kind": kind, "metadata": map[string]any{"name": n}}
}

func TestSortForApplyPutsReferencesFirst(t *testing.T) {
	objs := []official.Object{obj("Ingress", "a"), obj("Deployment", "w"), obj("ConfigMap", "c"), obj("ServiceAccount", "w"), obj("Service", "s")}
	sortForApply(objs)
	var got []string
	for _, o := range objs {
		got = append(got, official.Key(o))
	}
	want := []string{"ServiceAccount/w", "ConfigMap/c", "Service/s", "Deployment/w", "Ingress/a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestSupersededSkipsWhatIsNotAWorkerReplacement(t *testing.T) {
	// app-x-worker's base app-x is rendered too, and app-y is not a worker: nothing to retire, and no cluster is read.
	objs := []official.Object{obj("Deployment", "app-x-worker"), obj("Deployment", "app-x"), obj("Deployment", "app-y"), obj("ServiceAccount", "app-z-worker")}
	if got := superseded(objs, "none", "none"); len(got) != 0 {
		t.Fatalf("superseded = %v, want none", got)
	}
}

func TestKindsOf(t *testing.T) {
	got := kindsOf([]official.Object{obj("Deployment", "a"), obj("ConfigMap", "b"), obj("Deployment", "c")})
	if len(got) != 2 || got[0] != "configmap" || got[1] != "deployment" {
		t.Fatalf("kinds = %v", got)
	}
}

func TestImagesOf(t *testing.T) {
	dep := official.Object{"kind": "Deployment", "metadata": map[string]any{"name": "d"}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"initContainers": []any{map[string]any{"image": "reg/init@sha256:1"}},
		"containers":     []any{map[string]any{"image": "reg/app@sha256:2"}, map[string]any{"image": "reg/init@sha256:1"}}}}}}
	got := imagesOf([]official.Object{obj("ConfigMap", "c"), dep})
	if len(got) != 2 || got[0] != "reg/init@sha256:1" || got[1] != "reg/app@sha256:2" {
		t.Fatalf("images = %v", got)
	}
}

func TestProjectOf(t *testing.T) {
	for in, want := range map[string]string{"ex.shop.app": "ex.shop", "ex.shop.wfc": "ex.shop", "ex.shop.orders": "ex.shop", "solo": "solo"} {
		if got := projectOf(in); got != want {
			t.Errorf("projectOf(%q) = %q, want %q", in, got, want)
		}
	}
}
