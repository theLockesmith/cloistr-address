// Command cloistr-admin is the operator CLI for the Cloistr internal admin API
// (see arbiter/cloistr/architecture/admin-interface.md). Every request is signed
// as a NIP-98 event; mutations bind the request body via the payload tag.
//
// Auth (v1): the signing key comes from CLOISTR_ADMIN_NSEC (nsec1... or 64-hex).
// This is break-glass local signing for a trusted operator workstation; NIP-46
// bunker signing (no key on disk) is the planned follow-up and the model the
// admin UI uses. The API base URL comes from CLOISTR_ADMIN_API.
//
// Usage:
//
//	cloistr-admin me
//	cloistr-admin address grant   --user <pubkey> --handle <name> [--domain d] [--display-name n]
//	cloistr-admin address revoke  --handle <name> [--domain d] --reason <r>
//	cloistr-admin address transfer --handle <name> --to <pubkey> [--domain d]
//	cloistr-admin address list    --user <pubkey>
//	cloistr-admin reserve add     --handle <name> [--for <pubkey>] [--reason r]
//	cloistr-admin reserve remove  --handle <name>
//	cloistr-admin reserve list
//	cloistr-admin quota set       --user <pubkey> --type <id> --limit <n>   (0 = unlimited)
//	cloistr-admin quota reset     --user <pubkey> --type <id>
//	cloistr-admin quota get       --user <pubkey>
//	cloistr-admin credits grant   --user <pubkey> --sats <n> [--reason r] [--ref id]
//	cloistr-admin credits get     --user <pubkey>
//	cloistr-admin tier list
//	cloistr-admin tier update     --name <tier> --price <sats> [--disabled]
//	cloistr-admin audit tail      [--limit n] [--action a] [--actor pubkey]
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[1:]
	group := args[0]

	var err error
	switch group {
	case "me":
		err = doRequest(http.MethodGet, "/admin/v1/me", nil)
	case "address":
		err = cmdAddress(args[1:])
	case "reserve":
		err = cmdReserve(args[1:])
	case "quota":
		err = cmdQuota(args[1:])
	case "credits":
		err = cmdCredits(args[1:])
	case "tier":
		err = cmdTier(args[1:])
	case "audit":
		err = cmdAudit(args[1:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", group)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `cloistr-admin — Cloistr internal admin CLI
Env: CLOISTR_ADMIN_API (base URL, e.g. https://me.cloistr.xyz), CLOISTR_ADMIN_NSEC (nsec1... or 64-hex)
Run "cloistr-admin help" and see the package doc for the full command list.
`)
}

// --- command groups ---

func cmdAddress(a []string) error {
	if len(a) == 0 {
		return fmt.Errorf("address subcommand required (grant|revoke|transfer|list)")
	}
	fs := flag.NewFlagSet("address", flag.ExitOnError)
	user := fs.String("user", "", "target pubkey (64 hex)")
	handle := fs.String("handle", "", "username")
	domain := fs.String("domain", "", "domain (default: server domain)")
	display := fs.String("display-name", "", "display name")
	to := fs.String("to", "", "transfer target pubkey")
	reason := fs.String("reason", "", "reason")
	_ = fs.Parse(a[1:])

	switch a[0] {
	case "grant":
		body := map[string]any{"pubkey": *user, "username": *handle}
		if *domain != "" {
			body["domain"] = *domain
		}
		if *display != "" {
			body["display_name"] = *display
		}
		return doRequest(http.MethodPost, "/admin/v1/addresses/grant", body)
	case "revoke":
		body := map[string]any{"username": *handle, "reason": *reason}
		if *domain != "" {
			body["domain"] = *domain
		}
		return doRequest(http.MethodPost, "/admin/v1/addresses/revoke", body)
	case "transfer":
		body := map[string]any{"username": *handle, "to_pubkey": *to}
		if *domain != "" {
			body["domain"] = *domain
		}
		return doRequest(http.MethodPost, "/admin/v1/addresses/transfer", body)
	case "list":
		return doRequest(http.MethodGet, "/admin/v1/addresses?pubkey="+url.QueryEscape(*user), nil)
	default:
		return fmt.Errorf("unknown address subcommand %q", a[0])
	}
}

func cmdReserve(a []string) error {
	if len(a) == 0 {
		return fmt.Errorf("reserve subcommand required (add|remove|list)")
	}
	fs := flag.NewFlagSet("reserve", flag.ExitOnError)
	handle := fs.String("handle", "", "username")
	forPk := fs.String("for", "", "hold for this pubkey (omit = block entirely)")
	reason := fs.String("reason", "", "reason")
	_ = fs.Parse(a[1:])

	switch a[0] {
	case "add":
		body := map[string]any{"username": *handle}
		if *forPk != "" {
			body["for_pubkey"] = *forPk
		}
		if *reason != "" {
			body["reason"] = *reason
		}
		return doRequest(http.MethodPost, "/admin/v1/reserved", body)
	case "remove":
		return doRequest(http.MethodDelete, "/admin/v1/reserved/"+url.PathEscape(*handle), nil)
	case "list":
		return doRequest(http.MethodGet, "/admin/v1/reserved", nil)
	default:
		return fmt.Errorf("unknown reserve subcommand %q", a[0])
	}
}

func cmdQuota(a []string) error {
	if len(a) == 0 {
		return fmt.Errorf("quota subcommand required (set|reset|get)")
	}
	fs := flag.NewFlagSet("quota", flag.ExitOnError)
	user := fs.String("user", "", "target pubkey")
	qtype := fs.String("type", "", "quota type id (e.g. storage_bytes)")
	limit := fs.Int64("limit", 0, "limit (0 = unlimited)")
	_ = fs.Parse(a[1:])

	switch a[0] {
	case "set":
		return doRequest(http.MethodPost, "/admin/v1/quotas", map[string]any{"pubkey": *user, "quota_type_id": *qtype, "limit": *limit})
	case "reset":
		return doRequest(http.MethodPost, "/admin/v1/quotas/reset", map[string]any{"pubkey": *user, "quota_type_id": *qtype})
	case "get":
		return doRequest(http.MethodGet, "/admin/v1/quotas?pubkey="+url.QueryEscape(*user), nil)
	default:
		return fmt.Errorf("unknown quota subcommand %q", a[0])
	}
}

func cmdCredits(a []string) error {
	if len(a) == 0 {
		return fmt.Errorf("credits subcommand required (grant|get)")
	}
	fs := flag.NewFlagSet("credits", flag.ExitOnError)
	user := fs.String("user", "", "target pubkey")
	sats := fs.Int64("sats", 0, "amount in sats")
	reason := fs.String("reason", "admin_grant", "reason")
	ref := fs.String("ref", "", "reference id")
	_ = fs.Parse(a[1:])

	switch a[0] {
	case "grant":
		body := map[string]any{"pubkey": *user, "amount_sats": *sats, "reason": *reason}
		if *ref != "" {
			body["reference_id"] = *ref
		}
		return doRequest(http.MethodPost, "/admin/v1/credits/grant", body)
	case "get":
		return doRequest(http.MethodGet, "/admin/v1/credits?pubkey="+url.QueryEscape(*user), nil)
	default:
		return fmt.Errorf("unknown credits subcommand %q", a[0])
	}
}

func cmdTier(a []string) error {
	if len(a) == 0 {
		return fmt.Errorf("tier subcommand required (list|update)")
	}
	fs := flag.NewFlagSet("tier", flag.ExitOnError)
	name := fs.String("name", "", "tier name")
	price := fs.Int64("price", 0, "price in sats")
	disabled := fs.Bool("disabled", false, "disable the tier")
	_ = fs.Parse(a[1:])

	switch a[0] {
	case "list":
		return doRequest(http.MethodGet, "/admin/v1/tiers", nil)
	case "update":
		return doRequest(http.MethodPut, "/admin/v1/tiers", map[string]any{"tier_name": *name, "price_sats": *price, "enabled": !*disabled})
	default:
		return fmt.Errorf("unknown tier subcommand %q", a[0])
	}
}

func cmdAudit(a []string) error {
	if len(a) == 0 || a[0] != "tail" {
		return fmt.Errorf("audit subcommand required (tail)")
	}
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	limit := fs.Int("limit", 50, "max entries")
	action := fs.String("action", "", "filter by action")
	actor := fs.String("actor", "", "filter by actor pubkey")
	_ = fs.Parse(a[1:])

	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", *limit))
	if *action != "" {
		q.Set("action", *action)
	}
	if *actor != "" {
		q.Set("actor", *actor)
	}
	return doRequest(http.MethodGet, "/admin/v1/audit?"+q.Encode(), nil)
}

// --- HTTP + NIP-98 signing ---

func doRequest(method, path string, body map[string]any) error {
	base := strings.TrimRight(os.Getenv("CLOISTR_ADMIN_API"), "/")
	if base == "" {
		return fmt.Errorf("CLOISTR_ADMIN_API not set")
	}
	sk, err := loadSecretKey()
	if err != nil {
		return err
	}

	fullURL := base + path

	var bodyBytes []byte
	if body != nil {
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	authHeader, err := signNIP98(sk, method, fullURL, bodyBytes)
	if err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	var reqBody io.Reader
	if bodyBytes != nil {
		reqBody = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequest(method, fullURL, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authHeader)
	if bodyBytes != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)

	// Pretty-print JSON when possible.
	var pretty bytes.Buffer
	if json.Indent(&pretty, respBody, "", "  ") == nil {
		fmt.Println(pretty.String())
	} else {
		fmt.Println(string(respBody))
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return nil
}

// signNIP98 builds a kind-27235 event over method+URL (+payload hash for bodies)
// and returns the "Nostr <base64>" Authorization header value.
func signNIP98(sk, method, fullURL string, body []byte) (string, error) {
	tags := nostr.Tags{
		nostr.Tag{"u", fullURL},
		nostr.Tag{"method", method},
	}
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		tags = append(tags, nostr.Tag{"payload", hex.EncodeToString(sum[:])})
	}
	ev := nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      27235,
		Tags:      tags,
		Content:   "",
	}
	if err := ev.Sign(sk); err != nil {
		return "", err
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return "", err
	}
	return "Nostr " + base64.StdEncoding.EncodeToString(raw), nil
}

// loadSecretKey reads CLOISTR_ADMIN_NSEC (nsec1... or 64-hex) and returns hex.
func loadSecretKey() (string, error) {
	raw := strings.TrimSpace(os.Getenv("CLOISTR_ADMIN_NSEC"))
	if raw == "" {
		return "", fmt.Errorf("CLOISTR_ADMIN_NSEC not set")
	}
	if strings.HasPrefix(raw, "nsec1") {
		prefix, val, err := nip19.Decode(raw)
		if err != nil {
			return "", fmt.Errorf("decode nsec: %w", err)
		}
		if prefix != "nsec" {
			return "", fmt.Errorf("expected nsec, got %s", prefix)
		}
		sk, ok := val.(string)
		if !ok {
			return "", fmt.Errorf("unexpected nsec payload type")
		}
		return sk, nil
	}
	if len(raw) == 64 {
		if _, err := hex.DecodeString(raw); err != nil {
			return "", fmt.Errorf("invalid hex secret key")
		}
		return raw, nil
	}
	return "", fmt.Errorf("CLOISTR_ADMIN_NSEC must be nsec1... or 64-hex")
}
