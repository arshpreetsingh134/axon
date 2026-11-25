package iamadmin

import (
	"os"
	"testing"
)

// TestLive runs against a real cluster IAM only when IAMADMIN_LIVE_ORIGIN is set (with IAMADMIN_LIVE_USER,
// IAMADMIN_LIVE_PASSWORD, and optionally IAMADMIN_LIVE_KEY + IAMADMIN_LIVE_CLIENT + IAMADMIN_LIVE_AUDIENCE for the
// token check). It reads only, apart from asking the token endpoint for a token.
func TestLive(t *testing.T) {
	origin := os.Getenv("IAMADMIN_LIVE_ORIGIN")
	if origin == "" {
		t.Skip("IAMADMIN_LIVE_ORIGIN not set")
	}
	c, err := SignIn(origin, os.Getenv("IAMADMIN_LIVE_USER"), os.Getenv("IAMADMIN_LIVE_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.ReadAccount()
	if err != nil || a.Account.Revision == "" || len(a.Sites) == 0 {
		t.Fatalf("account %+v %v", a, err)
	}
	svcs, err := c.Services()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("account %s revision %s, %d sites, %d service accounts", c.Account, a.Account.Revision, len(a.Sites), len(svcs))
	if k := os.Getenv("IAMADMIN_LIVE_KEY"); k != "" {
		pemb, _ := os.ReadFile(k)
		if err := TokenCheck(origin, origin, os.Getenv("IAMADMIN_LIVE_CLIENT"), os.Getenv("IAMADMIN_LIVE_AUDIENCE"), pemb); err != nil {
			t.Fatal(err)
		}
		t.Log("token issued")
	}
}
