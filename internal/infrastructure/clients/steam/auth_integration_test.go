//go:build integration

// Manual Steam authentication check. Requires real credentials and is
// excluded from normal CI runs:
//
//	STEAM_ID=... STEAM_REFRESH_TOKEN=... go test -tags=integration ./internal/infrastructure/clients/steam/ -run TestSteamIntegration -v
//
// It authenticates against the harmless finalize endpoint family and
// verifies a community session works. Cookies and tokens are never
// printed; failure is either a network/Steam issue or an expired token
// (re-run the Node CLI in tools/steam-auth).
package steam_test

import (
	"context"
	"os"
	"testing"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/steam"
)

func TestSteamIntegration(t *testing.T) {
	steamID := os.Getenv("STEAM_ID")
	refreshToken := os.Getenv("STEAM_REFRESH_TOKEN")
	if steamID == "" || refreshToken == "" {
		t.Skip("STEAM_ID and STEAM_REFRESH_TOKEN must be set")
	}
	ctx := context.Background()
	auth := steam.NewSteamAuthClient(steamID, refreshToken, nil, "")
	cookies, err := auth.GetWebCookies(ctx)
	if err != nil {
		t.Fatalf("GetWebCookies: %v", err)
	}
	names := map[string]bool{}
	for _, c := range cookies {
		names[c.Name] = true
	}
	if !names["steamLoginSecure"] {
		t.Fatal("no steamLoginSecure cookie issued")
	}
	if !names["sessionid"] {
		t.Fatal("no sessionid cookie issued")
	}
	if tok, ok := auth.RotatedToken(); ok && tok != "" {
		t.Fatal("rotation observed but the token must never be printed")
	}
	t.Logf("authenticated %d cookies for %d domains", len(cookies), len(names))
}
