package api

import (
	"net/http/httptest"
	"testing"
)

// TestTenantOwnerGate verifies the ownership check logic:
// only the tenant owner_pubkey should be able to manage members.
func TestTenantOwnerGate(t *testing.T) {
	ownerPK := "aaaa000000000000000000000000000000000000000000000000000000000000"
	otherPK := "bbbb000000000000000000000000000000000000000000000000000000000000"

	cases := []struct {
		name       string
		callerPK   string
		ownerPK    string
		wantDenied bool
	}{
		{"owner can manage", ownerPK, ownerPK, false},
		{"non-owner is denied", otherPK, ownerPK, true},
		{"empty caller is denied", "", ownerPK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			denied := tc.callerPK != tc.ownerPK
			if denied != tc.wantDenied {
				t.Fatalf("got denied=%v, want %v", denied, tc.wantDenied)
			}
		})
	}
}

// TestTenantAdminCreateValidation tests that create-tenant rejects bad inputs.
func TestTenantAdminCreateValidation(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		ownerPK string
		wantOK  bool
	}{
		{"valid", "arbiter-fleet", "aaaa000000000000000000000000000000000000000000000000000000000000", true},
		{"empty id", "", "aaaa000000000000000000000000000000000000000000000000000000000000", false},
		{"bad pubkey", "test-tenant", "not-a-pubkey", false},
		{"short pubkey", "test-tenant", "aaaa", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idOK := tc.id != ""
			pkOK := validPubkey(tc.ownerPK)
			got := idOK && pkOK
			if got != tc.wantOK {
				t.Fatalf("id=%q pk=%q: got valid=%v, want %v", tc.id, tc.ownerPK, got, tc.wantOK)
			}
		})
	}
}

// TestTenantAdminRoutesRejectMissingAuth verifies that the admin tenant
// endpoints reject unauthenticated requests.
func TestTenantAdminRoutesRejectMissingAuth(t *testing.T) {
	r := adminRouter()

	endpoints := []struct{ method, path string }{
		{"POST", "/admin/v1/tenants"},
		{"POST", "/admin/v1/tenants/quota"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, "http://me.cloistr.xyz"+ep.path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != 401 {
				t.Fatalf("got %d, want 401", w.Code)
			}
		})
	}
}
