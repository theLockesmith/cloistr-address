package api

import "testing"

/*
The signup flow is: unauthenticated availability check, then an authenticated
POST /purchase/quote, then POST /purchase/invoice. The middleware provisions an
adjective-noun-NNNN address for any authenticated pubkey that has none — which
on that middle call means minting a throwaway seconds before the user claims a
real name, and AtomicRegisterAddress then KEEPS it as a permanent alias.

Someone claiming a name is not a nameless identity. Every other authenticated
path must keep provisioning, or an identity that never claims a name has no
deliverable address at all.
*/

func TestIsNameClaimPath(t *testing.T) {
	claims := []string{
		"/api/v1/purchase/quote",
		"/api/v1/purchase/invoice",
	}
	for _, p := range claims {
		if !isNameClaimPath(p) {
			t.Errorf("%s must skip auto-provisioning: it is part of claiming a name", p)
		}
	}

	// Everything else still provisions. /addresses/me matters most: a nameless
	// identity opening their dashboard is exactly who the auto address is for.
	notClaims := []string{
		"/api/v1/addresses/me",
		"/api/v1/credits",
		"/api/v1/addresses/lightning",
		"/api/v1/addresses/check/coldforge",
		"/api/v1/credits/withdraw",
		"",
		"/",
	}
	for _, p := range notClaims {
		if isNameClaimPath(p) {
			t.Errorf("%s must still auto-provision; skipping it strands identities with no address", p)
		}
	}
}

func TestIsNameClaimPath_DoesNotBlanketTheWholePurchasePrefix(t *testing.T) {
	// A prefix match on /purchase would silently opt future endpoints out of
	// provisioning. Anything new under /purchase has to be added deliberately.
	for _, p := range []string{
		"/api/v1/purchase/history",
		"/api/v1/purchase/status/abc123",
		"/api/v1/purchase",
	} {
		if isNameClaimPath(p) {
			t.Errorf("%s matched by accident — the check must name each claim endpoint", p)
		}
	}
}
