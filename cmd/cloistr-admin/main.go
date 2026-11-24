// Command cloistr-admin is the operator CLI for the Cloistr internal admin API
// (see arbiter/cloistr/architecture/admin-interface.md). Every request is signed
// as a NIP-98 event; mutations bind the request body via the payload tag.
//
// Auth: requests are signed by a NIP-46 remote signer (a "bunker") named by
// --bunker or CLOISTR_ADMIN_BUNKER, so the admin key never enters this process.
// The CLI keeps its own NIP-46 client key in CLOISTR_ADMIN_CLIENT_KEY_FILE
// (default <user config dir>/cloistr-admin/nip46-client.key, created 0600 on
// first use). Keep that file: the bunker authorizes this client key once, and
// its connect secret may be single-use.
//
// CLOISTR_ADMIN_NSEC (nsec1... or 64-hex) remains as break-glass local signing
// and warns on every use. Setting both is refused. The API base URL comes from
// CLOISTR_ADMIN_API.
//
// Usage:
//
//	cloistr-admin [--bunker bunker://...] me
//	cloistr-admin address grant   --user <pubkey> --handle <name> [--domain d] [--display-name n]
//	cloistr-admin address revoke  --handle <name> [--domain d] --reason <r>
//	cloistr-admin address transfer --handle <name> --to <pubkey> [--domain d]
//	cloistr-admin address lightning --handle <name> --mode proxy --address <ln-addr> [--domain d]
//	cloistr-admin address list    --user <pubkey>
//	cloistr-admin tenant create   --id <name> --owner <pubkey>
//	cloistr-admin tenant quota   --id <name> --type <quota_type> --limit <bytes>
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
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"

	"git.aegis-hq.xyz/coldforge/cloistr-common/crypto"
)

var errUsage = errors.New("usage")

func main() {
	err := run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	if errors.Is(err, errUsage) {
		os.Exit(2)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// cli carries what one invocation needs. env is the environment lookup
// (os.Getenv in production) so tests never touch the process environment.
type cli struct {
	env    func(string) string
	stdout io.Writer
	stderr io.Writer
	bunker string // --bunker, which overrides CLOISTR_ADMIN_BUNKER

	signer      crypto.Signer
	closeSigner func()
}

func run(args []string, env func(string) string, stdout, stderr io.Writer) error {
	c := &cli{env: env, stdout: stdout, stderr: stderr}
	defer func() {
		if c.closeSigner != nil {
			c.closeSigner()
		}
	}()

	// Global flags come before the command group.
	for len(args) > 0 && strings.HasPrefix(args[0], "--bunker") {
		if v, ok := strings.CutPrefix(args[0], "--bunker="); ok {
			c.bunker = v
			args = args[1:]
			continue
		}
		if args[0] != "--bunker" || len(args) < 2 {
			_, _ = fmt.Fprintln(stderr, "--bunker needs a bunker:// URL")
			return errUsage
		}
		c.bunker = args[1]
		args = args[2:]
	}
	if len(args) == 0 {
		usage(stderr)
		return errUsage
	}

	switch group := args[0]; group {
	case "me":
		return c.do(http.MethodGet, "/admin/v1/me", nil)
	case "address":
		return c.cmdAddress(args[1:])
	case "reserve":
		return c.cmdReserve(args[1:])
	case "quota":
		return c.cmdQuota(args[1:])
	case "credits":
		return c.cmdCredits(args[1:])
	case "tier":
		return c.cmdTier(args[1:])
	case "tenant":
		return c.cmdTenant(args[1:])
	case "audit":
		return c.cmdAudit(args[1:])
	case "-h", "--help", "help":
		usage(stderr)
		return nil
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", group)
		usage(stderr)
		return errUsage
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `cloistr-admin — Cloistr internal admin CLI
Usage: cloistr-admin [--bunker bunker://...] <command> ...
Env:
  CLOISTR_ADMIN_API              base URL, e.g. https://me.cloistr.xyz
  CLOISTR_ADMIN_BUNKER           bunker:// URL of the NIP-46 signer holding the admin key
  CLOISTR_ADMIN_CLIENT_KEY_FILE  this CLI's own NIP-46 client key (default: user config dir)
  CLOISTR_ADMIN_BUNKER_TIMEOUT   how long to wait for the bunker (default 30s)
  CLOISTR_ADMIN_NSEC             break-glass only: sign with a local key
Run "cloistr-admin help" and see the package doc for the full command list.
`)
}

// --- command groups ---

func (c *cli) cmdAddress(a []string) error {
	if len(a) == 0 {
		return fmt.Errorf("address subcommand required (grant|revoke|transfer|lightning|list)")
	}
	fs := flag.NewFlagSet("address", flag.ExitOnError)
	user := fs.String("user", "", "target pubkey (64 hex)")
	handle := fs.String("handle", "", "username")
	domain := fs.String("domain", "", "domain (default: server domain)")
	display := fs.String("display-name", "", "display name")
	to := fs.String("to", "", "transfer target pubkey")
	reason := fs.String("reason", "", "reason")
	mode := fs.String("mode", "proxy", "lightning mode (proxy|disabled)")
	lnAddr := fs.String("address", "", "proxy Lightning Address (e.g. alice@getalby.com)")
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
		return c.do(http.MethodPost, "/admin/v1/addresses/grant", body)
	case "revoke":
		body := map[string]any{"username": *handle, "reason": *reason}
		if *domain != "" {
			body["domain"] = *domain
		}
		return c.do(http.MethodPost, "/admin/v1/addresses/revoke", body)
	case "transfer":
		body := map[string]any{"username": *handle, "to_pubkey": *to}
		if *domain != "" {
			body["domain"] = *domain
		}
		return c.do(http.MethodPost, "/admin/v1/addresses/transfer", body)
	case "lightning":
		body := map[string]any{"username": *handle, "mode": *mode}
		if *lnAddr != "" {
			body["proxy_address"] = *lnAddr
		}
		if *domain != "" {
			body["domain"] = *domain
		}
		return c.do(http.MethodPost, "/admin/v1/addresses/lightning", body)
	case "list":
		return c.do(http.MethodGet, "/admin/v1/addresses?pubkey="+url.QueryEscape(*user), nil)
	default:
		return fmt.Errorf("unknown address subcommand %q", a[0])
	}
}

func (c *cli) cmdReserve(a []string) error {
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
		return c.do(http.MethodPost, "/admin/v1/reserved", body)
	case "remove":
		return c.do(http.MethodDelete, "/admin/v1/reserved/"+url.PathEscape(*handle), nil)
	case "list":
		return c.do(http.MethodGet, "/admin/v1/reserved", nil)
	default:
		return fmt.Errorf("unknown reserve subcommand %q", a[0])
	}
}

func (c *cli) cmdQuota(a []string) error {
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
		return c.do(http.MethodPost, "/admin/v1/quotas", map[string]any{"pubkey": *user, "quota_type_id": *qtype, "limit": *limit})
	case "reset":
		return c.do(http.MethodPost, "/admin/v1/quotas/reset", map[string]any{"pubkey": *user, "quota_type_id": *qtype})
	case "get":
		return c.do(http.MethodGet, "/admin/v1/quotas?pubkey="+url.QueryEscape(*user), nil)
	default:
		return fmt.Errorf("unknown quota subcommand %q", a[0])
	}
}

func (c *cli) cmdCredits(a []string) error {
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
		return c.do(http.MethodPost, "/admin/v1/credits/grant", body)
	case "get":
		return c.do(http.MethodGet, "/admin/v1/credits?pubkey="+url.QueryEscape(*user), nil)
	default:
		return fmt.Errorf("unknown credits subcommand %q", a[0])
	}
}

func (c *cli) cmdTier(a []string) error {
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
		return c.do(http.MethodGet, "/admin/v1/tiers", nil)
	case "update":
		return c.do(http.MethodPut, "/admin/v1/tiers", map[string]any{"tier_name": *name, "price_sats": *price, "enabled": !*disabled})
	default:
		return fmt.Errorf("unknown tier subcommand %q", a[0])
	}
}

func (c *cli) cmdAudit(a []string) error {
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
	return c.do(http.MethodGet, "/admin/v1/audit?"+q.Encode(), nil)
}

// --- HTTP + NIP-98 signing ---

func (c *cli) do(method, path string, body map[string]any) error {
	base := strings.TrimRight(c.env("CLOISTR_ADMIN_API"), "/")
	if base == "" {
		return fmt.Errorf("CLOISTR_ADMIN_API not set")
	}
	signer, err := c.loadSigner()
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

	ctx, cancel := context.WithTimeout(context.Background(), c.bunkerTimeout())
	authHeader, err := signNIP98(ctx, signer, method, fullURL, bodyBytes)
	cancel()
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
		_, _ = fmt.Fprintln(c.stdout, pretty.String())
	} else {
		_, _ = fmt.Fprintln(c.stdout, string(respBody))
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return nil
}

// signNIP98 builds a kind-27235 event over method+URL (+payload hash for bodies),
// has signer sign it, and returns the "Nostr <base64>" Authorization header value.
func signNIP98(ctx context.Context, signer crypto.Signer, method, fullURL string, body []byte) (string, error) {
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
	if err := signer.SignEvent(ctx, &ev); err != nil {
		return "", err
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return "", err
	}
	return "Nostr " + base64.StdEncoding.EncodeToString(raw), nil
}

// --- signer selection ---

const defaultBunkerTimeout = 30 * time.Second

func (c *cli) bunkerTimeout() time.Duration {
	if d, err := time.ParseDuration(c.env("CLOISTR_ADMIN_BUNKER_TIMEOUT")); err == nil && d > 0 {
		return d
	}
	return defaultBunkerTimeout
}

// loadSigner picks the signer once per invocation: the bunker when one is
// named, otherwise the break-glass local key. Both at once is refused, so a
// forgotten CLOISTR_ADMIN_NSEC cannot linger beside a working bunker.
func (c *cli) loadSigner() (crypto.Signer, error) {
	if c.signer != nil {
		return c.signer, nil
	}
	bunker := strings.TrimSpace(c.bunker)
	if bunker == "" {
		bunker = strings.TrimSpace(c.env("CLOISTR_ADMIN_BUNKER"))
	}
	nsec := strings.TrimSpace(c.env("CLOISTR_ADMIN_NSEC"))

	switch {
	case bunker != "" && nsec != "":
		return nil, fmt.Errorf("both a bunker and CLOISTR_ADMIN_NSEC are set; unset CLOISTR_ADMIN_NSEC so the admin key stays in the bunker")
	case bunker != "":
		keyPath, err := c.clientKeyPath()
		if err != nil {
			return nil, err
		}
		clientSK, err := loadClientKey(keyPath)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), c.bunkerTimeout())
		defer cancel()
		s, err := crypto.ConnectNIP46Signer(ctx, clientSK, bunker)
		if err != nil {
			return nil, fmt.Errorf("connect to bunker: %w", err)
		}
		c.signer, c.closeSigner = s, s.Close
	case nsec != "":
		_, _ = fmt.Fprintln(c.stderr, "warning: signing with CLOISTR_ADMIN_NSEC (break-glass). The admin key is in this process's environment; use --bunker or CLOISTR_ADMIN_BUNKER for normal work.")
		sk, err := decodeSecretKey(nsec)
		if err != nil {
			return nil, err
		}
		s, err := crypto.NewLocalSigner(sk)
		if err != nil {
			return nil, fmt.Errorf("CLOISTR_ADMIN_NSEC: %w", err)
		}
		c.signer = s
	default:
		return nil, fmt.Errorf("no signer: set CLOISTR_ADMIN_BUNKER (or pass --bunker) to the bunker:// URL of the signer holding the admin key; CLOISTR_ADMIN_NSEC is break-glass only")
	}
	return c.signer, nil
}

func (c *cli) clientKeyPath() (string, error) {
	if p := strings.TrimSpace(c.env("CLOISTR_ADMIN_CLIENT_KEY_FILE")); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no config dir for the NIP-46 client key; set CLOISTR_ADMIN_CLIENT_KEY_FILE: %w", err)
	}
	return filepath.Join(dir, "cloistr-admin", "nip46-client.key"), nil
}

// loadClientKey returns this CLI's NIP-46 client key (hex), creating it on
// first use. The key identifies the CLI to the bunker, not the admin: it can
// ask the bunker to sign, and only because the bunker authorized it once. A
// key readable by other users is refused, as ssh refuses a loose private key.
func loadClientKey(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		sk, _, err := crypto.GenerateKeypair()
		if err != nil {
			return "", fmt.Errorf("generate NIP-46 client key: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", fmt.Errorf("NIP-46 client key dir: %w", err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", fmt.Errorf("create NIP-46 client key: %w", err)
		}
		_, werr := f.WriteString(sk + "\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return "", fmt.Errorf("write NIP-46 client key: %w", werr)
		}
		return sk, nil
	}
	if err != nil {
		return "", fmt.Errorf("read NIP-46 client key: %w", err)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("NIP-46 client key %s is readable by other users; chmod 600 it", path)
	}
	sk := strings.TrimSpace(string(raw))
	if err := crypto.ValidateSecretKey(sk); err != nil {
		return "", fmt.Errorf("NIP-46 client key %s: %w", path, err)
	}
	return sk, nil
}

// decodeSecretKey accepts nsec1... or 64-hex and returns hex.
func decodeSecretKey(raw string) (string, error) {
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

func (c *cli) cmdTenant(a []string) error {
	if len(a) == 0 {
		return fmt.Errorf("tenant subcommand required (create|quota)")
	}
	fs := flag.NewFlagSet("tenant", flag.ExitOnError)
	id := fs.String("id", "", "tenant id (e.g. arbiter-fleet)")
	owner := fs.String("owner", "", "owner pubkey (64 hex)")
	qtype := fs.String("type", "", "quota type id (e.g. storage_bytes)")
	limit := fs.Int64("limit", 0, "quota limit in bytes (0 = unlimited)")
	_ = fs.Parse(a[1:])

	switch a[0] {
	case "create":
		return c.do(http.MethodPost, "/admin/v1/tenants", map[string]any{
			"id":           *id,
			"owner_pubkey": *owner,
		})
	case "quota":
		return c.do(http.MethodPost, "/admin/v1/tenants/quota", map[string]any{
			"tenant_id":  *id,
			"quota_type": *qtype,
			"limit":      *limit,
		})
	default:
		return fmt.Errorf("unknown tenant subcommand %q", a[0])
	}
}
