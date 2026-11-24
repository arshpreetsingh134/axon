package ci

import "testing"

func TestPushedImages(t *testing.T) {
	log := "2026-11-24T06:11:13.8663284Z #28 exporting manifest list sha256:28f63e35d9def02914f62aa44d9776bae5081e262ee97f1864c0504884cb36be done\n" +
		"2026-11-24T06:11:14.0000000Z #28 pushing manifest for ghcr.io/example/business-migration:446822a6c670650f28f8cbfe555ffa34e83c5f9e@sha256:28f63e35d9def02914f62aa44d9776bae5081e262ee97f1864c0504884cb36be\n" +
		"2026-11-24T06:11:17.0000000Z #28 pushing manifest for ghcr.io/example/business-migration:446822a6c670650f28f8cbfe555ffa34e83c5f9e@sha256:28f63e35d9def02914f62aa44d9776bae5081e262ee97f1864c0504884cb36be 3.3s done\n" +
		"#30 pushing manifest for localhost:5000/team/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\r\n" +
		"unrelated line mentioning sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"
	got := PushedImages(log)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if got["ghcr.io/example/business-migration"] != "sha256:28f63e35d9def02914f62aa44d9776bae5081e262ee97f1864c0504884cb36be" {
		t.Errorf("migration: %v", got)
	}
	if got["localhost:5000/team/app"] != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("registry with a port: %v", got)
	}
}
