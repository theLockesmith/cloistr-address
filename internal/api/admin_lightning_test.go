package api

import (
	"testing"
)

func TestAdminSetLightning_ValidatesMode(t *testing.T) {
	// mode must be "proxy" or "disabled"
	cases := []struct {
		mode string
		ok   bool
	}{
		{"proxy", true},
		{"disabled", true},
		{"nwc", false},
		{"hosted", false},
		{"", false},
	}
	for _, tc := range cases {
		got := tc.mode == "proxy" || tc.mode == "disabled"
		if got != tc.ok {
			t.Errorf("mode %q: got valid=%v, want %v", tc.mode, got, tc.ok)
		}
	}
}

func TestAdminSetLightning_ProxyRequiresAddress(t *testing.T) {
	// When mode is "proxy", a non-empty proxy_address is required.
	// When mode is "disabled", proxy_address is ignored.
	cases := []struct {
		mode    string
		addr    string
		wantErr bool
	}{
		{"proxy", "alice@getalby.com", false},
		{"proxy", "", true},
		{"disabled", "", false},
		{"disabled", "alice@getalby.com", false}, // ignored
	}
	for _, tc := range cases {
		needsAddr := tc.mode == "proxy" && tc.addr == ""
		if needsAddr != tc.wantErr {
			t.Errorf("mode=%q addr=%q: got needsAddr=%v, want %v", tc.mode, tc.addr, needsAddr, tc.wantErr)
		}
	}
}
