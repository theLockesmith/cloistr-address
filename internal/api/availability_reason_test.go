package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

/*
ME-7: GET /api/v1/addresses/check/:username answered "Available!" for reserved
words (admin, root, support) in production, and for an invalid-format name the
UI silently reported the same thing because it sanitized the characters out of
the string before ever calling this endpoint.

The backend's two failure reasons were already correct for format ("Invalid
username format...") but collapsed "reserved" and "already taken" into one
generic "Username is not available", giving the UI (which now surfaces
`reason`) nothing to distinguish them with. These tests pin the two reasons
apart.
*/

func TestCheckUsernameAvailability_ReservedNameReason(t *testing.T) {
	r, mock := priceHandler(t)
	mock.ExpectQuery("is_username_available").
		WillReturnRows(sqlmock.NewRows([]string{"is_username_available"}).AddRow(false))
	mock.ExpectQuery("reserved_usernames").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/addresses/check/admin", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var got UsernameAvailabilityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Available {
		t.Fatalf("available = true, want false for a reserved name")
	}
	const want = "This username is reserved."
	if got.Reason != want {
		t.Fatalf("reason = %q, want %q", got.Reason, want)
	}
}

func TestCheckUsernameAvailability_TakenNameReason(t *testing.T) {
	r, mock := priceHandler(t)
	mock.ExpectQuery("is_username_available").
		WillReturnRows(sqlmock.NewRows([]string{"is_username_available"}).AddRow(false))
	mock.ExpectQuery("reserved_usernames").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/addresses/check/coldforge", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var got UsernameAvailabilityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Available {
		t.Fatalf("available = true, want false for a taken name")
	}
	const want = "Username is already taken."
	if got.Reason != want {
		t.Fatalf("reason = %q, want %q", got.Reason, want)
	}
}

// TestCheckUsernameAvailability_InvalidFormatRejected is a characterization
// test, not a regression test: this path was already correct before ME-7 (the
// bug was in the UI, which stripped the disallowed characters before this
// endpoint ever saw them -- see cloistr-me-ui's UsernameInput.tsx). It passes
// unmodified, confirming the backend was not the broken side for this half of
// ME-7.
func TestCheckUsernameAvailability_InvalidFormatRejected(t *testing.T) {
	r, _ := priceHandler(t)

	w := httptest.NewRecorder()
	// URL-encoded "bad name!" -- gin decodes this back to the raw string for
	// c.Param("username").
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/addresses/check/bad%20name%21", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var got UsernameAvailabilityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Available {
		t.Fatalf("available = true, want false for %q", got.Username)
	}
	if got.Reason == "" {
		t.Fatalf("reason missing for invalid-format username")
	}
}
