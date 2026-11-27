package commands

import (
	"testing"

	"axon/internal/descriptor"
)

func TestWantedFormsGrants(t *testing.T) {
	got, err := wantedFormsGrants(&descriptor.FormsPolicyGrants{Tenant: "demo", Grants: map[string][]string{
		"forms-worker-registry": {"ecm_archive:*:read", "ecm_archive:*:archive"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Principal != "service:forms-worker-registry" || got[0].Action != "archive" ||
		got[1].Action != "read" || got[0].Tenant != "demo" || got[0].ResourceID != "*" {
		t.Fatalf("grants: %+v", got)
	}
	if _, err := wantedFormsGrants(&descriptor.FormsPolicyGrants{Grants: map[string][]string{"x": {"bad"}}}); err == nil {
		t.Fatal("a malformed grant was accepted")
	}
}
