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

// A failed internal-API login is logged twice with client_ip: once by the
// internal auth middleware (the security log) and once by the access log.
// Both must carry X-Real-IP, which the public edge overwrites with the real
// peer, never the client's forged X-Forwarded-For, which the edge only
// appends to. Gin's defaults logged the forged XFF.
func TestClientIPIgnoresForgedForwardedFor(t *testing.T) {
	h := &Handler{cfg: &config.Config{InternalAPI: config.InternalAPIConfig{Secret: "s3cret"}}}
	r := h.Router()

	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"edge request: forged XFF, X-Real-IP set by edge",
			map[string]string{"X-Forwarded-For": "203.0.113.77, 198.51.100.9", "X-Real-IP": "198.51.100.9"}, "198.51.100.9"},
		{"no X-Real-IP: forged XFF ignored, TCP peer used",
			map[string]string{"X-Forwarded-For": "203.0.113.77"}, "10.0.0.5"},
		{"no headers", nil, "10.0.0.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			defer slog.SetDefault(prev)

			req := httptest.NewRequest(http.MethodPost, "/internal/v1/credits/grant", nil)
			req.RemoteAddr = "10.0.0.5:41234"
			req.Header.Set("Authorization", "Bearer wrong")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}

			logged := map[string]string{}
			dec := json.NewDecoder(&buf)
			for dec.More() {
				var rec map[string]any
				if err := dec.Decode(&rec); err != nil {
					t.Fatalf("decode log: %v", err)
				}
				if ip, ok := rec["client_ip"].(string); ok {
					logged[rec["msg"].(string)] = ip
				}
			}
			for _, msg := range []string{"invalid internal API authentication attempt", "http request"} {
				got, ok := logged[msg]
				if !ok {
					t.Fatalf("no %q log record with client_ip; got %v", msg, logged)
				}
				if got != tc.want {
					t.Errorf("%q client_ip = %q, want %q", msg, got, tc.want)
				}
			}
		})
	}
}
