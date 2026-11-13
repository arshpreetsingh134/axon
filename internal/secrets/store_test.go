package secrets

import "testing"

func TestValidNameAndStoreName(t *testing.T) {
	for n, ok := range map[string]bool{"OPENROUTER_API_KEY": true, "_X": true, "A1": true, "lower": false, "1A": false, "A-B": false, "A=B": false, "": false} {
		if ValidName(n) != ok {
			t.Errorf("ValidName(%q) = %v", n, !ok)
		}
	}
	if s := New("orbstack", "Insurance-Safepoint", "DEV"); s.Name != "insurance-safepoint.dev" || s.Context != "orbstack" {
		t.Fatalf("store %+v", s)
	}
}
