package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	_ "github.com/lib/pq"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip46"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/api"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

/*
cloistr-admin signed every request with a raw admin key from CLOISTR_ADMIN_NSEC,
so the platform-admin key had to sit in the operator's environment. These tests
pin the bunker mode: the admin key lives only in a NIP-46 remote signer, the
CLI holds a stable client key of its own, and the nsec path survives only as a
loud break-glass fallback.

The bunker here is go-nostr's reference NIP-46 signer behind an in-memory
relay, the same harness cloistr-common uses for NIP46Signer.
*/

// --- in-memory relay (ported from cloistr-common crypto/nip46_test.go) ---

type stubRelay struct {
	srv     *httptest.Server
	mu      sync.Mutex
	subs    map[*stubConn]map[string][]nostr.Filter
	onEvent func(*nostr.Event) []*nostr.Event
}

type stubConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (c *stubConn) send(ctx context.Context, msg ...any) {
	b, _ := json.Marshal(msg)
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.Write(ctx, websocket.MessageText, b)
}

func newStubRelay(t *testing.T) *stubRelay {
	t.Helper()
	r := &stubRelay{subs: map[*stubConn]map[string][]nostr.Filter{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *stubRelay) url() string { return "ws" + strings.TrimPrefix(r.srv.URL, "http") }

func (r *stubRelay) serve(w http.ResponseWriter, req *http.Request) {
	ws, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	c := &stubConn{ws: ws}
	ctx := req.Context()
	defer func() {
		r.mu.Lock()
		delete(r.subs, c)
		r.mu.Unlock()
		_ = ws.CloseNow()
	}()
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var msg []json.RawMessage
		if json.Unmarshal(data, &msg) != nil || len(msg) < 2 {
			continue
		}
		var typ string
		_ = json.Unmarshal(msg[0], &typ)
		switch typ {
		case "EVENT":
			var evt nostr.Event
			if evt.UnmarshalJSON(msg[1]) != nil {
				continue
			}
			c.send(ctx, "OK", evt.ID, true, "")
			r.publish(ctx, &evt)
		case "REQ":
			var id string
			_ = json.Unmarshal(msg[1], &id)
			var filters []nostr.Filter
			for _, raw := range msg[2:] {
				var f nostr.Filter
				if f.UnmarshalJSON(raw) == nil {
					filters = append(filters, f)
				}
			}
			r.mu.Lock()
			if r.subs[c] == nil {
				r.subs[c] = map[string][]nostr.Filter{}
			}
			r.subs[c][id] = filters
			r.mu.Unlock()
			c.send(ctx, "EOSE", id)
		case "CLOSE":
			var id string
			_ = json.Unmarshal(msg[1], &id)
			r.mu.Lock()
			delete(r.subs[c], id)
			r.mu.Unlock()
		}
	}
}

// publish delivers evt to matching subscriptions (nothing is stored: NIP-46
// events are ephemeral), then publishes whatever the bunker hook replies.
func (r *stubRelay) publish(ctx context.Context, evt *nostr.Event) {
	type delivery struct {
		c  *stubConn
		id string
	}
	r.mu.Lock()
	var out []delivery
	for c, subs := range r.subs {
		for id, filters := range subs {
			for _, f := range filters {
				if f.MatchesIgnoringTimestampConstraints(evt) {
					out = append(out, delivery{c, id})
					break
				}
			}
		}
	}
	hook := r.onEvent
	r.mu.Unlock()
	for _, d := range out {
		d.c.send(ctx, "EVENT", d.id, evt)
	}
	if hook != nil {
		for _, reply := range hook(evt) {
			r.publish(ctx, reply)
		}
	}
}

// stubBunker holds userSK and signs for any client that presented the secret
// once. The secret is single-use, as real bunkers often make it: a client that
// comes back with a new key is refused.
type stubBunker struct {
	relay   *stubRelay
	userSK  string
	userPK  string
	secret  string
	mu      sync.Mutex
	used    bool
	clients map[string]bool // client pubkeys that have been authorized
}

func newStubBunker(t *testing.T, userSK string) *stubBunker {
	t.Helper()
	pk, _ := nostr.GetPublicKey(userSK)
	b := &stubBunker{relay: newStubRelay(t), userSK: userSK, userPK: pk, secret: "s3cret", clients: map[string]bool{}}
	signer := nip46.NewStaticKeySigner(userSK)
	signer.AuthorizeRequest = func(_ bool, from, secret string) bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		if secret == b.secret && !b.used {
			b.used = true
			b.clients[from] = true
		}
		return b.clients[from]
	}
	b.relay.onEvent = func(evt *nostr.Event) []*nostr.Event {
		if evt.Kind != nostr.KindNostrConnect || evt.Tags.FindWithValue("p", pk) == nil {
			return nil
		}
		_, _, reply, err := signer.HandleRequest(context.Background(), evt)
		if err != nil {
			return nil
		}
		return []*nostr.Event{&reply}
	}
	return b
}

func (b *stubBunker) url() string {
	return "bunker://" + b.userPK + "?relay=" + b.relay.url() + "&secret=" + b.secret
}

func (b *stubBunker) clientCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients)
}

// --- a fake admin API that only checks the NIP-98 signature ---

type seenRequest struct {
	pubkey string
	path   string
}

func nip98Server(t *testing.T) (*httptest.Server, func() []seenRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenRequest
	v := auth.NewNIP98Validator(auth.DefaultNIP98Config())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		pk, err := v.ValidateRequest(r)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusUnauthorized)
			return
		}
		if len(body) > 0 && payloadTag(r) != sha256hex(body) {
			http.Error(w, `{"error":"payload"}`, http.StatusUnauthorized)
			return
		}
		mu.Lock()
		seen = append(seen, seenRequest{pk, r.URL.Path})
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenRequest(nil), seen...)
	}
}

func payloadTag(r *http.Request) string {
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(r.Header.Get("Authorization"), "Nostr "))
	var ev nostr.Event
	if json.Unmarshal(raw, &ev) != nil {
		return ""
	}
	if tag := ev.Tags.Find("payload"); tag != nil {
		return tag[1]
	}
	return ""
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// envOf turns a map into the env lookup run takes, so no test reads or
// writes the real process environment.
func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func runCLI(t *testing.T, env map[string]string, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := run(args, envOf(env), &out, &errOut)
	return out.String(), errOut.String(), err
}

func keypair(t *testing.T) (string, string) {
	t.Helper()
	sk := nostr.GeneratePrivateKey()
	pk, err := nostr.GetPublicKey(sk)
	if err != nil {
		t.Fatal(err)
	}
	return sk, pk
}

// --- tests that need no database ---

func TestBunker_RequestsAreSignedByTheBunkersKey(t *testing.T) {
	adminSK, adminPK := keypair(t)
	b := newStubBunker(t, adminSK)
	srv, seen := nip98Server(t)
	keyFile := filepath.Join(t.TempDir(), "client.key")

	// --bunker flag, and a body-bearing call so the payload tag is checked too.
	_, stderr, err := runCLI(t, map[string]string{
		"CLOISTR_ADMIN_API":             srv.URL,
		"CLOISTR_ADMIN_CLIENT_KEY_FILE": keyFile,
	}, "--bunker", b.url(), "quota", "set", "--user", strings.Repeat("a", 64), "--type", "storage_bytes", "--limit", "5")
	if err != nil {
		t.Fatalf("run: %v (stderr %q)", err, stderr)
	}
	got := seen()
	if len(got) != 1 || got[0].pubkey != adminPK || got[0].path != "/admin/v1/quotas" {
		t.Fatalf("server saw %+v; want one /admin/v1/quotas request signed by the admin key", got)
	}
	if strings.Contains(strings.ToLower(stderr), "break-glass") {
		t.Errorf("bunker mode warned about break-glass: %q", stderr)
	}
}

func TestBunker_EnvVarWorksLikeTheFlag(t *testing.T) {
	adminSK, adminPK := keypair(t)
	b := newStubBunker(t, adminSK)
	srv, seen := nip98Server(t)

	_, stderr, err := runCLI(t, map[string]string{
		"CLOISTR_ADMIN_API":             srv.URL,
		"CLOISTR_ADMIN_BUNKER":          b.url(),
		"CLOISTR_ADMIN_CLIENT_KEY_FILE": filepath.Join(t.TempDir(), "client.key"),
	}, "me")
	if err != nil {
		t.Fatalf("run: %v (stderr %q)", err, stderr)
	}
	if got := seen(); len(got) != 1 || got[0].pubkey != adminPK {
		t.Fatalf("server saw %+v; want one request signed by %s", got, adminPK[:8])
	}
}

// The bunker's secret is single-use, so a second run only works if the CLI
// comes back with the same client key it was authorized under.
func TestBunker_ClientKeyIsStableAcrossRuns(t *testing.T) {
	adminSK, adminPK := keypair(t)
	b := newStubBunker(t, adminSK)
	srv, seen := nip98Server(t)
	keyFile := filepath.Join(t.TempDir(), "sub", "client.key")
	env := map[string]string{
		"CLOISTR_ADMIN_API":             srv.URL,
		"CLOISTR_ADMIN_BUNKER":          b.url(),
		"CLOISTR_ADMIN_CLIENT_KEY_FILE": keyFile,
	}

	if _, stderr, err := runCLI(t, env, "me"); err != nil {
		t.Fatalf("first run: %v (stderr %q)", err, stderr)
	}
	first, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("client key not saved: %v", err)
	}
	if fi, _ := os.Stat(keyFile); fi.Mode().Perm() != 0o600 {
		t.Errorf("client key mode = %o; want 600", fi.Mode().Perm())
	}

	if _, stderr, err := runCLI(t, env, "me"); err != nil {
		t.Fatalf("second run with the same client key: %v (stderr %q)", err, stderr)
	}
	second, _ := os.ReadFile(keyFile)
	if !bytes.Equal(first, second) {
		t.Error("client key changed between runs")
	}
	if n := b.clientCount(); n != 1 {
		t.Errorf("bunker authorized %d client keys; want 1", n)
	}
	if got := seen(); len(got) != 2 || got[1].pubkey != adminPK {
		t.Fatalf("server saw %+v; want two requests signed by the admin key", got)
	}
}

func TestBunker_UnreachableBunkerFailsFast(t *testing.T) {
	_, pk := keypair(t)
	srv, seen := nip98Server(t)
	start := time.Now()
	_, _, err := runCLI(t, map[string]string{
		"CLOISTR_ADMIN_API":             srv.URL,
		"CLOISTR_ADMIN_BUNKER":          "bunker://" + pk + "?relay=ws://127.0.0.1:1&secret=x",
		"CLOISTR_ADMIN_CLIENT_KEY_FILE": filepath.Join(t.TempDir(), "client.key"),
		"CLOISTR_ADMIN_BUNKER_TIMEOUT":  "2s",
	}, "me")
	if err == nil {
		t.Fatal("run succeeded with no reachable bunker")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %s to give up", d)
	}
	if len(seen()) != 0 {
		t.Error("a request reached the server without a signature from the bunker")
	}
}

func TestNsec_IsBreakGlassAndSaysSo(t *testing.T) {
	sk, pk := keypair(t)
	srv, seen := nip98Server(t)
	_, stderr, err := runCLI(t, map[string]string{
		"CLOISTR_ADMIN_API":  srv.URL,
		"CLOISTR_ADMIN_NSEC": sk,
	}, "me")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := seen(); len(got) != 1 || got[0].pubkey != pk {
		t.Fatalf("server saw %+v", got)
	}
	if !strings.Contains(strings.ToLower(stderr), "break-glass") {
		t.Errorf("nsec mode gave no break-glass warning; stderr %q", stderr)
	}
	if strings.Contains(stderr, sk) {
		t.Error("stderr echoed the secret key")
	}
}

func TestBunkerAndNsecTogetherAreRefused(t *testing.T) {
	sk, pk := keypair(t)
	srv, seen := nip98Server(t)
	_, _, err := runCLI(t, map[string]string{
		"CLOISTR_ADMIN_API":    srv.URL,
		"CLOISTR_ADMIN_NSEC":   sk,
		"CLOISTR_ADMIN_BUNKER": "bunker://" + pk + "?relay=ws://127.0.0.1:1",
	}, "me")
	if err == nil || !strings.Contains(err.Error(), "CLOISTR_ADMIN_NSEC") {
		t.Fatalf("err = %v; want a refusal naming CLOISTR_ADMIN_NSEC", err)
	}
	if len(seen()) != 0 {
		t.Error("a request was sent")
	}
}

func TestNoSignerConfigured(t *testing.T) {
	srv, _ := nip98Server(t)
	_, _, err := runCLI(t, map[string]string{"CLOISTR_ADMIN_API": srv.URL}, "me")
	if err == nil || !strings.Contains(err.Error(), "CLOISTR_ADMIN_BUNKER") {
		t.Fatalf("err = %v; want it to point at CLOISTR_ADMIN_BUNKER", err)
	}
}

// --- against a real cloistr-me admin API and database ---

func realAdminAPI(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB-integration test")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := httptest.NewServer(api.NewHandler(&config.Config{Domain: "cloistr.xyz"}, storage.NewWithDB(db)).Router())
	t.Cleanup(srv.Close)
	return srv, db
}

func insertUser(t *testing.T, db *sql.DB, pk string, admin bool) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO users (pubkey, is_platform_admin) VALUES ($1, $2) ON CONFLICT (pubkey) DO UPDATE SET is_platform_admin = $2`, pk, admin); err != nil {
		t.Fatalf("insert user: %v", err)
	}
}

func TestBunker_AdminCanSetQuotaAndGrantAddress(t *testing.T) {
	srv, db := realAdminAPI(t)
	adminSK, adminPK := keypair(t)
	insertUser(t, db, adminPK, true)
	_, targetPK := keypair(t)
	insertUser(t, db, targetPK, false)
	handle := "bk" + targetPK[:10]

	b := newStubBunker(t, adminSK)
	env := map[string]string{
		"CLOISTR_ADMIN_API":             srv.URL,
		"CLOISTR_ADMIN_BUNKER":          b.url(),
		"CLOISTR_ADMIN_CLIENT_KEY_FILE": filepath.Join(t.TempDir(), "client.key"),
	}

	const limit = "53687091200" // 50 GiB, the first real use
	if out, stderr, err := runCLI(t, env, "quota", "set", "--user", targetPK, "--type", "storage_bytes", "--limit", limit); err != nil {
		t.Fatalf("quota set: %v\n%s%s", err, out, stderr)
	}
	out, stderr, err := runCLI(t, env, "quota", "get", "--user", targetPK)
	if err != nil || !strings.Contains(out, limit) {
		t.Fatalf("quota get = %v\n%s%s; want the %s limit", err, out, stderr, limit)
	}

	if out, stderr, err := runCLI(t, env, "address", "grant", "--user", targetPK, "--handle", handle, "--display-name", "Bunker Test"); err != nil {
		t.Fatalf("address grant: %v\n%s%s", err, out, stderr)
	}
	var owner string
	if err := db.QueryRow(`SELECT pubkey FROM addresses WHERE username = $1`, handle).Scan(&owner); err != nil || owner != targetPK {
		t.Fatalf("granted address owner = %q (%v); want %s", owner, err, targetPK[:8])
	}
}

func TestBunker_NonAdminKeyIsRefused(t *testing.T) {
	srv, db := realAdminAPI(t)
	userSK, userPK := keypair(t)
	insertUser(t, db, userPK, false)
	_, targetPK := keypair(t)
	handle := "bn" + targetPK[:10]

	b := newStubBunker(t, userSK)
	env := map[string]string{
		"CLOISTR_ADMIN_API":             srv.URL,
		"CLOISTR_ADMIN_BUNKER":          b.url(),
		"CLOISTR_ADMIN_CLIENT_KEY_FILE": filepath.Join(t.TempDir(), "client.key"),
	}

	_, _, err := runCLI(t, env, "address", "grant", "--user", targetPK, "--handle", handle)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("non-admin grant err = %v; want a 403", err)
	}
	_, _, err = runCLI(t, env, "quota", "set", "--user", targetPK, "--type", "storage_bytes", "--limit", "1")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("non-admin quota set err = %v; want a 403", err)
	}
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM addresses WHERE username = $1`, handle).Scan(&n)
	if n != 0 {
		t.Fatalf("non-admin grant created %d address rows", n)
	}
}
