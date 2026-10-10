package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
)

const testPeer = "10.0.0.5"

// serveAndLog sends one request through the real router from testPeer and
// returns the log records it produced, keyed by message.
func serveAndLog(t *testing.T, method, path string, headers map[string]string) (int, map[string]map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	h := &Handler{cfg: &config.Config{InternalAPI: config.InternalAPIConfig{Secret: "s3cret"}}}
	r := h.Router()

	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = testPeer + ":41234"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	records := map[string]map[string]any{}
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decode log: %v", err)
		}
		records[rec["msg"].(string)] = rec
	}
	return w.Code, records
}

func record(t *testing.T, records map[string]map[string]any, msg string) map[string]any {
	t.Helper()
	rec, ok := records[msg]
	if !ok {
		t.Fatalf("no %q log record; got %v", msg, records)
	}
	return rec
}

// Public traffic comes through the edge, which overwrites X-Real-IP with the
// real peer but only appends to X-Forwarded-For. client_ip must come from
// X-Real-IP, never the client's forged XFF. Gin's defaults logged the forged XFF.
func TestClientIPIgnoresForgedForwardedFor(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"edge request: forged XFF, X-Real-IP set by edge",
			map[string]string{"X-Forwarded-For": "203.0.113.77, 198.51.100.9", "X-Real-IP": "198.51.100.9"}, "198.51.100.9"},
		{"no X-Real-IP: forged XFF ignored, TCP peer used",
			map[string]string{"X-Forwarded-For": "203.0.113.77"}, testPeer},
		{"no headers", nil, testPeer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, records := serveAndLog(t, http.MethodGet, "/health", tc.headers)
			rec := record(t, records, "http request")
			if got := rec["client_ip"]; got != tc.want {
				t.Errorf("client_ip = %v, want %q", got, tc.want)
			}
			if got := rec["peer_addr"]; got != testPeer {
				t.Errorf("peer_addr = %v, want %q", got, testPeer)
			}
		})
	}
}

// The internal API is called pod to pod, so nothing overwrites X-Real-IP and
// an in-cluster caller can set it, or XFF, to anything. A failed internal login
// must name the TCP peer in both the security log and the access log, carry
// X-Real-IP only as a labelled claim, and never log a header value as client_ip.
func TestInternalAPILogsTCPPeerNotClaimedHeaders(t *testing.T) {
	headers := map[string]string{
		"Authorization":   "Bearer wrong",
		"X-Forwarded-For": "203.0.113.77",
		"X-Real-IP":       "198.51.100.66",
	}
	code, records := serveAndLog(t, http.MethodPost, "/internal/v1/credits/grant", headers)
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	for _, msg := range []string{"invalid internal API authentication attempt", "http request"} {
		rec := record(t, records, msg)
		if got := rec["peer_addr"]; got != testPeer {
			t.Errorf("%q peer_addr = %v, want %q", msg, got, testPeer)
		}
		if got := rec["claimed_x_real_ip"]; got != "198.51.100.66" {
			t.Errorf("%q claimed_x_real_ip = %v, want the header value labelled as a claim", msg, got)
		}
		if got, ok := rec["client_ip"]; ok {
			t.Errorf("%q logged client_ip = %v from a header on an in-cluster request", msg, got)
		}
	}
}
