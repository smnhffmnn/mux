package tools

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/smnhffmnn/mux/internal/config"
)

// The bunq connection type puts a bunq account (personal or business) behind
// the bunq public API: accounts with balances, payments, events, draft
// payments and statement exports, read with the owner's own API key.
//
// What makes bunq a type of its own rather than an http connection is the
// way in: before the first read the API wants an installation (an RSA
// public key, answered with an installation token), a device registration
// (the API key, bound to the calling IP) and a session (a token that lives
// as long as the auto-logout time set in the bunq app). Requests with a body
// are signed with the installation's RSA key. The type does all of that
// itself: it generates the key once and keeps it under the secret
// {name}-private-key, keeps installation, device and session under
// {name}-api-context, and signs in again when a session has run out.
//
// Money leaves an account only through a draft payment: the one write the
// type offers (create_draft_payment) files a draft the owner accepts in the
// bunq app. bunq itself does not insist on the app — a PUT with status
// ACCEPTED by the API key moves the money (verified against the sandbox,
// 2026-11-08) — so the guarantee is mux's: the direct payment endpoints are
// not in the registry below, the type never sends an acceptance, and the
// pass-through api_request refuses both whatever a caller passes.
//
// The contract is bunq's published OpenAPI document
// (https://github.com/bunq/doc, swagger.json): the endpoints the type calls
// and the fields it decodes are listed in bunqEndpoints and bunqSchemaFields
// and checked against the vendored snapshot by bunq_spec_test.go.

const (
	bunqRequestTimeout  = 60 * time.Second
	bunqMaxBody         = 8 << 20          // largest JSON response read
	bunqDefaultCount    = 50               // page size when a caller names none
	bunqMaxCount        = 200              // bunq's page cap
	bunqMaxPages        = 20               // since-driven paging stops here (20 × 200 payments)
	bunqAPIInlineMax    = 1024 * 1024      // raw api_get/api_request responses and statements are inlined up to this size
	bunqSessionSlack    = 5 * time.Minute  // a session this close to its expiry is renewed before the call
	bunqKeyBits         = 2048             // bunq wants 2048-bit RSA keys
	bunqStatementWait   = 30 * time.Second // a statement export that is not COMPLETED by then is reported as pending
	bunqDeviceDescLimit = 64               // bunq caps the device description
	bunqVersionPath     = "/v1"
	bunqIdempotencyKey  = "X-Bunq-Client-Idempotency-Key" // bunq remembers a POST under it for 24 h and answers a repeat with 409
)

// bunqStatementPollInterval is the pause between reads of a statement that
// is not COMPLETED yet; a variable so tests can shorten it.
var bunqStatementPollInterval = 2 * time.Second

// DefaultBunqInstructions provides guidance for LLMs using bunq tools.
const DefaultBunqInstructions = `bunq bank account behind the bunq public API. Amounts are strings with two decimals ("12.34") and a currency; dates are "YYYY-MM-DD HH:MM:SS.ffffff" in UTC as bunq reports them.
Start with list_accounts: it names the accounts (id, IBAN, balance, status) and the user the key belongs to. Every other tool takes account_id from there.
list_payments pages by id: newest first, older_id of the result continues towards the past, newer_id towards the present; since stops at a date and paces itself to bunq's rate limit (about one page per second). list_events is the user's timeline (payments, requests, cards, drafts). export_statement produces bunq's own CSV, MT940 or PDF for a date range; bunq keeps a record of every export.
A session with bunq runs out after the auto-logout time set in the app; the connection signs in again by itself. The API key is bound to the IP it was first used from.`

// BunqReadOnlyInstructions are appended on read-only connections.
const BunqReadOnlyInstructions = `Read-only connection: no draft payments, no api_request; nothing changes at bunq apart from the statement exports it keeps a record of.`

// BunqWriteInstructions are appended on connections that are not read-only.
const BunqWriteInstructions = `The one way money leaves an account here is create_draft_payment: it files a draft the owner accepts in the bunq app — nothing is transferred until then, and this connection never sends the acceptance. Direct payments, requests, cards, scheduled payments and any change to a draft are refused, also through api_request, which creates drafts, writes notes and exports only.`

// BunqInstructions composes the default instructions of a connection.
func BunqInstructions(conn config.Connection) string {
	parts := []string{DefaultBunqInstructions}
	if config.BunqSandbox(conn) {
		parts = append(parts, "This connection points at the bunq sandbox: test users, test money, no real accounts.")
	}
	if conn.ReadOnly {
		parts = append(parts, BunqReadOnlyInstructions)
	} else {
		parts = append(parts, BunqWriteInstructions)
	}
	return strings.Join(parts, "\n")
}

// bunqSecretStore is where the type keeps what it creates itself: the RSA
// key and the API context. The default reads and writes mux's secret store;
// tests swap in a map.
type bunqSecretStore interface {
	Get(key string) (string, error)
	Set(key, value string) error
}

type configSecretStore struct{}

func (configSecretStore) Get(key string) (string, error) { return config.GetSecret(key) }
func (configSecretStore) Set(key, value string) error    { return config.SaveSecret(key, value) }

// bunqAPIContext is the state bunq's way in leaves behind, persisted under
// {name}-api-context so a restart neither registers a new installation
// (bunq allows few per day) nor a new device. Host and KeyFingerprint tie
// the context to the API host and the RSA key it was created with: a
// context from the sandbox does not fit production, and a new RSA key needs
// a new installation. APIKeySum ties device and session to the API key: a
// rotated key keeps the installation and registers a new device.
type bunqAPIContext struct {
	Host              string    `json:"host"`
	KeyFingerprint    string    `json:"key_fingerprint"`
	InstallationToken string    `json:"installation_token"`
	ServerPublicKey   string    `json:"server_public_key"`
	APIKeySum         string    `json:"api_key_sum,omitempty"` // SHA-256 of the API key the device was registered with
	DeviceID          int64     `json:"device_id,omitempty"`
	SessionToken      string    `json:"session_token,omitempty"`
	SessionExpires    time.Time `json:"session_expires,omitempty"`
	SessionTimeout    int64     `json:"session_timeout,omitempty"` // seconds, as the user object reported it
	UserID            int64     `json:"user_id,omitempty"`
	UserType          string    `json:"user_type,omitempty"` // UserPerson, UserCompany, UserApiKey
	DisplayName       string    `json:"display_name,omitempty"`
}

// Bunq wraps a bunq account as MCP tools.
type Bunq struct {
	name      string
	baseURL   string // https://api.bunq.com or the sandbox host, no path
	apiKey    string
	readOnly  bool
	sandbox   bool
	userAgent string
	client    *http.Client
	secrets   bunqSecretStore

	// mu guards the key and the API context: the first call of a process
	// loads or creates them, a session that ran out is renewed under it.
	mu       sync.Mutex
	keyPEM   string // the configured key (conn.PrivateKey), parsed on first use
	key      *rsa.PrivateKey
	loaded   bool
	api      bunqAPIContext
	installs int // installations this process registered (tests count them)

	// now is swappable for tests.
	now func() time.Time
}

// NewBunq creates a bunq connection from config.
func NewBunq(conn config.Connection, dialer Dialer) (*Bunq, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("bunq: token (API key) is required")
	}
	if err := config.ValidateBunq(conn); err != nil {
		return nil, fmt.Errorf("bunq: %w", err)
	}
	baseURL := strings.TrimRight(strings.TrimSpace(conn.URL), "/")
	if baseURL == "" {
		baseURL = config.BunqProductionURL
	}
	transport := &http.Transport{}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}
	return &Bunq{
		name:      conn.Name,
		baseURL:   baseURL,
		apiKey:    conn.Token,
		readOnly:  conn.ReadOnly,
		sandbox:   config.BunqSandbox(conn),
		userAgent: "mux bunq connection",
		client:    &http.Client{Transport: transport, Timeout: bunqRequestTimeout},
		secrets:   configSecretStore{},
		keyPEM:    conn.PrivateKey,
		now:       time.Now,
	}, nil
}

// ReadOnly reports whether the connection registers reading tools only.
func (b *Bunq) ReadOnly() bool { return b.readOnly }

// --- Endpoint registry ---

// bunqEndpoint is one REST call the type makes. Path is the OpenAPI template
// under /v1; Params are the query parameters the call may send (bunq's
// document does not describe query parameters, so they are listed for the
// reader and checked at the call, not against the spec).
type bunqEndpoint struct {
	Method  string
	Path    string
	Params  []string
	Headers []string // request headers beyond the standard set the call may send
}

// bunqEndpoints is the complete list of REST calls the type can make through
// call. The direct payment endpoints are not in it — on purpose: the type
// moves money through draft payments only. The pass-through (api_get,
// api_request) is the one path beside the registry; bunqAPIPath confines it
// to /v1/ and bunqWriteAllowed to the resources a draft-only connection may
// write.
var bunqEndpoints = map[string]bunqEndpoint{
	"installation":      {Method: http.MethodPost, Path: "/installation"},
	"device-server":     {Method: http.MethodPost, Path: "/device-server"},
	"session-server":    {Method: http.MethodPost, Path: "/session-server"},
	"accounts":          {Method: http.MethodGet, Path: "/user/{userID}/monetary-account", Params: []string{"count", "older_id", "newer_id"}},
	"payments":          {Method: http.MethodGet, Path: "/user/{userID}/monetary-account/{monetary-accountID}/payment", Params: []string{"count", "older_id", "newer_id"}},
	"payment":           {Method: http.MethodGet, Path: "/user/{userID}/monetary-account/{monetary-accountID}/payment/{itemId}"},
	"events":            {Method: http.MethodGet, Path: "/user/{userID}/event", Params: []string{"count", "older_id", "newer_id", "monetary_account_id", "status"}},
	"drafts":            {Method: http.MethodGet, Path: "/user/{userID}/monetary-account/{monetary-accountID}/draft-payment", Params: []string{"count", "older_id", "newer_id"}},
	"draft.create":      {Method: http.MethodPost, Path: "/user/{userID}/monetary-account/{monetary-accountID}/draft-payment", Headers: []string{bunqIdempotencyKey}},
	"statement.create":  {Method: http.MethodPost, Path: "/user/{userID}/monetary-account/{monetary-accountID}/customer-statement"},
	"statement":         {Method: http.MethodGet, Path: "/user/{userID}/monetary-account/{monetary-accountID}/customer-statement/{itemId}"},
	"statement.content": {Method: http.MethodGet, Path: "/user/{userID}/monetary-account/{monetary-accountID}/customer-statement/{customer-statementID}/content"},
}

// bunqSchemaFields lists, per OpenAPI component schema, the properties the
// type decodes or sends. The spec test checks every name against the schema,
// so a property bunq removes or renames fails a test before it fails a user.
// What the document does not describe — the wrapper names of answers
// (CustomerStatement for a statement read), undocumented fields that arrive
// anyway (created on a draft listing) — the live test against the sandbox
// pins instead.
var bunqSchemaFields = map[string][]string{
	"Installation":                {"client_public_key"},
	"DeviceServer":                {"description", "secret"},
	"SessionServer":               {"secret"},
	"MonetaryAccountBankListing":  {"id", "created", "updated", "description", "status", "sub_status", "currency", "balance", "alias", "display_name", "user_id", "overdraft_limit"},
	"PaymentListing":              {"id", "created", "updated", "monetary_account_id", "amount", "alias", "counterparty_alias", "description", "type", "sub_type", "merchant_reference", "batch_id", "scheduled_id", "balance_after_mutation", "payment_fee"},
	"EventListing":                {"id", "created", "updated", "action", "user_id", "monetary_account_id", "object", "status"},
	"DraftPayment":                {"entries", "number_of_required_accepts"},
	"DraftPaymentEntry":           {"amount", "counterparty_alias", "description", "id"},
	"DraftPaymentListing":         {"id", "monetary_account_id", "status", "entries"}, // created and updated arrive too, undocumented
	"ExportStatement":             {"statement_format", "date_start", "date_end", "regional_format", "include_attachment"},
	"ExportStatementRead":         {"id", "created", "status", "date_start", "date_end", "statement_format", "regional_format"},
	"Amount":                      {"value", "currency"},
	"Pointer":                     {"type", "value", "name"},
	"LabelMonetaryAccount":        {"iban", "display_name", "country", "label_user"},
	"InstallationToken":           {"token"},
	"InstallationServerPublicKey": {"server_public_key"},
	"SessionServerToken":          {"token"},
}

// --- Keys and context ---

// bunqParseKey reads an RSA private key in PKCS#8 or PKCS#1 PEM.
func bunqParseKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if block.Type == "RSA PRIVATE KEY" {
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA key (%T)", k)
	}
	return rk, nil
}

// bunqPublicKeyPEM renders the installation's public key the way bunq
// expects it: PKIX, PEM.
func bunqPublicKeyPEM(key *rsa.PrivateKey) (string, string, error) {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(der)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), hex.EncodeToString(sum[:]), nil
}

// loadKey parses the configured key or the stored one, or generates and
// stores a new one. Caller holds mu.
func (b *Bunq) loadKey() error {
	if b.key != nil {
		return nil
	}
	pemText := strings.TrimSpace(b.keyPEM)
	if pemText == "" {
		if v, err := b.secrets.Get(b.name + "-private-key"); err == nil {
			pemText = strings.TrimSpace(v)
		}
	}
	if pemText != "" {
		k, err := bunqParseKey(pemText)
		if err != nil {
			return fmt.Errorf("bunq: the stored private key (%s-private-key) does not parse: %v — delete it and the connection creates a new one (and a new installation)", b.name, err)
		}
		b.key = k
		return nil
	}
	k, err := rsa.GenerateKey(rand.Reader, bunqKeyBits)
	if err != nil {
		return fmt.Errorf("bunq: generate RSA key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return fmt.Errorf("bunq: encode RSA key: %w", err)
	}
	text := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	// Stored before it is used: an installation registered with a key that
	// is then lost would be one of the few bunq allows per day, wasted.
	if err := b.secrets.Set(b.name+"-private-key", text); err != nil {
		return fmt.Errorf("bunq: store the RSA key as %s-private-key: %w", b.name, err)
	}
	b.key = k
	log.Printf("[bunq] %s: generated a new RSA key (%d bit), stored as %s-private-key", b.name, bunqKeyBits, b.name)
	return nil
}

// loadContext reads the stored API context and drops it when it belongs to
// another host or key. Caller holds mu.
func (b *Bunq) loadContext() {
	if b.loaded {
		return
	}
	b.loaded = true
	v, err := b.secrets.Get(b.name + "-api-context")
	if err != nil || strings.TrimSpace(v) == "" {
		return
	}
	var c bunqAPIContext
	if err := json.Unmarshal([]byte(v), &c); err != nil {
		log.Printf("[bunq] %s: stored API context does not parse (%v) — starting over", b.name, err)
		return
	}
	_, fp, err := bunqPublicKeyPEM(b.key)
	if err != nil {
		return
	}
	if c.Host != b.baseURL || c.KeyFingerprint != fp {
		log.Printf("[bunq] %s: stored API context belongs to another host or RSA key — starting over", b.name)
		return
	}
	if c.APIKeySum != bunqAPIKeySum(b.apiKey) {
		// Another API key: the installation (RSA key) stays, device and
		// session belonged to the old key and are dropped.
		log.Printf("[bunq] %s: the API key changed — registering a new device under the existing installation", b.name)
		c.APIKeySum, c.DeviceID = "", 0
		c.SessionToken, c.SessionExpires, c.SessionTimeout = "", time.Time{}, 0
		c.UserID, c.UserType, c.DisplayName = 0, "", ""
	}
	b.api = c
}

// bunqAPIKeySum identifies an API key in the stored context without storing
// the key itself.
func bunqAPIKeySum(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

// saveContext persists the API context. Caller holds mu.
func (b *Bunq) saveContext() error {
	data, err := json.Marshal(b.api)
	if err != nil {
		return err
	}
	if err := b.secrets.Set(b.name+"-api-context", string(data)); err != nil {
		return fmt.Errorf("bunq: store the API context as %s-api-context: %w", b.name, err)
	}
	return nil
}

// sessionValid reports whether the current session token can be used.
// Caller holds mu.
func (b *Bunq) sessionValid() bool {
	if b.api.SessionToken == "" {
		return false
	}
	if b.api.SessionExpires.IsZero() {
		return true // unknown lifetime: used until bunq says otherwise
	}
	// Renew a little before the end — but a short timeout must not make
	// every call a login: the slack is at most a quarter of the lifetime.
	slack := bunqSessionSlack
	if b.api.SessionTimeout > 0 {
		if quarter := time.Duration(b.api.SessionTimeout) * time.Second / 4; quarter < slack {
			slack = quarter
		}
	}
	return b.now().Add(slack).Before(b.api.SessionExpires)
}

// ensureSession makes sure the connection has a usable session: key loaded
// or created, installation and device registered once, session started or
// renewed. It returns the session token and the user id.
func (b *Bunq) ensureSession(ctx context.Context) (string, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.loadKey(); err != nil {
		return "", 0, err
	}
	b.loadContext()
	if b.sessionValid() {
		return b.api.SessionToken, b.api.UserID, nil
	}
	err := b.signIn(ctx)
	if err != nil && bunqInstallationLost(err) {
		// bunq no longer accepts the installation (revoked key, deleted
		// device): register again — once. An IP or API-key refusal is not
		// that case and is reported as it is.
		log.Printf("[bunq] %s: refused (%v) — registering the installation again", b.name, err)
		b.api = bunqAPIContext{}
		err = b.signIn(ctx)
	}
	if err != nil {
		return "", 0, err
	}
	return b.api.SessionToken, b.api.UserID, nil
}

// signIn does whatever is still missing of the way in: the installation,
// the device, the session. Each step is persisted when it succeeds, so a
// step that fails is the only one repeated next time — an installation
// whose device registration was refused is not registered again. Caller
// holds mu.
func (b *Bunq) signIn(ctx context.Context) error {
	if b.api.InstallationToken == "" {
		if err := b.install(ctx); err != nil {
			return err
		}
	}
	if b.api.DeviceID == 0 {
		if err := b.registerDevice(ctx); err != nil {
			return err
		}
	}
	return b.startSession(ctx)
}

// invalidateSession forgets the session token so the next call signs in
// again. Only the token that failed is dropped: a call that raced a renewal
// must not throw away the fresh one.
func (b *Bunq) invalidateSession(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.api.SessionToken == token {
		b.api.SessionToken = ""
		b.api.SessionExpires = time.Time{}
	}
}

// install registers the RSA key (installation) and persists the token at
// once: bunq allows few installations per day, so one must not be lost to a
// failing next step. Caller holds mu.
func (b *Bunq) install(ctx context.Context) error {
	pub, fp, err := bunqPublicKeyPEM(b.key)
	if err != nil {
		return fmt.Errorf("bunq: encode public key: %w", err)
	}
	env, err := b.request(ctx, "installation", nil, nil, "", nil, map[string]any{"client_public_key": pub})
	if err != nil {
		return fmt.Errorf("bunq installation: %w", err)
	}
	b.installs++
	var tok struct {
		Token string `json:"token"`
	}
	var srv struct {
		ServerPublicKey string `json:"server_public_key"`
	}
	if err := env.decode("Token", &tok); err != nil || tok.Token == "" {
		return fmt.Errorf("bunq installation: no token in the answer")
	}
	_ = env.decode("ServerPublicKey", &srv)
	b.api = bunqAPIContext{Host: b.baseURL, KeyFingerprint: fp, InstallationToken: tok.Token, ServerPublicKey: srv.ServerPublicKey}
	log.Printf("[bunq] %s: installation registered at %s", b.name, b.baseURL)
	return b.saveContext()
}

// registerDevice registers the API key under the installation. Caller
// holds mu.
func (b *Bunq) registerDevice(ctx context.Context) error {
	desc := []rune("mux " + b.name)
	if len(desc) > bunqDeviceDescLimit {
		desc = desc[:bunqDeviceDescLimit]
	}
	env, err := b.request(ctx, "device-server", nil, nil, b.api.InstallationToken, nil, map[string]any{"description": string(desc), "secret": b.apiKey})
	if err != nil {
		return fmt.Errorf("bunq device registration: %w%s", err, bunqIPHint(err))
	}
	var id struct {
		ID int64 `json:"id"`
	}
	_ = env.decode("Id", &id)
	b.api.DeviceID = id.ID
	b.api.APIKeySum = bunqAPIKeySum(b.apiKey)
	log.Printf("[bunq] %s: device registered at %s (device %d)", b.name, b.baseURL, id.ID)
	return b.saveContext()
}

// bunqIPHint explains bunq's refusal of a key from another address, which
// its message names as "Incorrect API key or IP address".
func bunqIPHint(err error) string {
	if !bunqIPRefusal(err) {
		return ""
	}
	return " — bunq ties the API key to the IP it was first used from (and names a wrong key the same way): run mux there, allow all IPs for the key in the bunq app, or create a new key"
}

// startSession opens a session with the installation token and the API key
// and records the user it belongs to. Caller holds mu.
func (b *Bunq) startSession(ctx context.Context) error {
	env, err := b.request(ctx, "session-server", nil, nil, b.api.InstallationToken, nil, map[string]any{"secret": b.apiKey})
	if err != nil {
		return fmt.Errorf("bunq session: %w%s", err, bunqIPHint(err))
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := env.decode("Token", &tok); err != nil || tok.Token == "" {
		return fmt.Errorf("bunq session: no token in the answer")
	}
	userType, user := env.user()
	if user == nil {
		return fmt.Errorf("bunq session: no user in the answer")
	}
	b.api.SessionToken = tok.Token
	b.api.UserType = userType
	b.api.UserID = user.ID
	b.api.DisplayName = user.DisplayName
	b.api.SessionExpires = time.Time{}
	b.api.SessionTimeout = user.SessionTimeout
	if user.SessionTimeout > 0 {
		b.api.SessionExpires = b.now().Add(time.Duration(user.SessionTimeout) * time.Second)
	}
	return b.saveContext()
}

// bunqIPRefusalRe matches bunq's wording for a key used from another
// address ("User credentials are incorrect. Incorrect API key or IP
// address."), on the error descriptions only.
var bunqIPRefusalRe = regexp.MustCompile(`(?i)\bip address\b|\bip\b`)

// bunqIPRefusal reports whether an error is bunq refusing the API key for
// this IP (or the key itself — bunq words both the same way).
func bunqIPRefusal(err error) bool {
	var he *bunqHTTPError
	if !errors.As(err, &he) {
		return false
	}
	return bunqIPRefusalRe.MatchString(he.descriptions())
}

// bunqInstallationLost reports whether a refusal on the way in means bunq
// no longer accepts the installation token: 401 or 403 on the device or
// session registration, where that token is the credential (the sandbox
// answers an unknown one with 403 "Insufficient authentication"). Then a new
// installation is the remedy. A 400 is about the body — a wrong API key, a
// key bound to another IP — and a new installation would not help, so it is
// reported as it is.
func bunqInstallationLost(err error) bool {
	var he *bunqHTTPError
	if !errors.As(err, &he) {
		return false
	}
	if bunqIPRefusal(err) {
		return false
	}
	return he.Status == 401 || he.Status == 403
}

// bunqSessionLost reports whether a call failed because the session token
// is no longer accepted.
func bunqSessionLost(err error) bool {
	var he *bunqHTTPError
	if !errors.As(err, &he) {
		return false
	}
	msg := strings.ToLower(he.descriptions())
	return he.Status == 401 || (he.Status == 403 && strings.Contains(msg, "authentication")) ||
		(he.Status == 400 && strings.Contains(msg, "session") && strings.Contains(msg, "expire"))
}

// --- HTTP ---

// bunqEnvelope is the shape of every bunq answer: Response, a list of
// one-key objects (the key names the type: Payment, MonetaryAccountBank,
// Token, Id, …), Pagination on lists, Error instead of Response on failures.
type bunqEnvelope struct {
	Response   []map[string]json.RawMessage `json:"Response"`
	Pagination *bunqPagination              `json:"Pagination"`
	Error      []bunqAPIError               `json:"Error"`
	Status     int                          `json:"-"`
}

type bunqPagination struct {
	OlderURL  *string `json:"older_url"`
	NewerURL  *string `json:"newer_url"`
	FutureURL *string `json:"future_url"`
}

type bunqAPIError struct {
	Description           string `json:"error_description"`
	DescriptionTranslated string `json:"error_description_translated"`
}

// items returns the objects of the given type in Response order.
func (e *bunqEnvelope) items(typ string) []json.RawMessage {
	var out []json.RawMessage
	for _, item := range e.Response {
		if raw, ok := item[typ]; ok {
			out = append(out, raw)
		}
	}
	return out
}

// decode unmarshals the first object of the given type into out.
func (e *bunqEnvelope) decode(typ string, out any) error {
	items := e.items(typ)
	if len(items) == 0 {
		return fmt.Errorf("no %s in the answer", typ)
	}
	return json.Unmarshal(items[0], out)
}

// bunqUser is the part of the session's user the type keeps: its id (the
// {userID} of every path), its name and the session timeout the app has
// set, in seconds.
type bunqUser struct {
	ID             int64  `json:"id"`
	DisplayName    string `json:"display_name"`
	SessionTimeout int64  `json:"session_timeout"`
}

// user finds the user object of a session answer: UserPerson, UserCompany
// or — for keys issued through OAuth — UserApiKey, whose id is the one to
// address and whose granting user carries the name.
func (e *bunqEnvelope) user() (string, *bunqUser) {
	for _, item := range e.Response {
		for typ, raw := range item {
			if !strings.HasPrefix(typ, "User") {
				continue
			}
			var u bunqUser
			if err := json.Unmarshal(raw, &u); err != nil {
				continue
			}
			if typ == "UserApiKey" {
				var ak struct {
					GrantedByUser map[string]bunqUser `json:"granted_by_user"`
				}
				if json.Unmarshal(raw, &ak) == nil {
					for _, g := range ak.GrantedByUser {
						u.DisplayName = g.DisplayName
						if u.SessionTimeout == 0 {
							u.SessionTimeout = g.SessionTimeout
						}
					}
				}
			}
			return typ, &u
		}
	}
	return "", nil
}

// olderID and newerID read the ids bunq's pagination URLs carry. On the
// newest page newer_url is null and future_url names the id to poll from
// for items that arrive later — the same direction, so it stands in.
func (p *bunqPagination) olderID() string {
	if p == nil {
		return ""
	}
	return bunqPageID(p.OlderURL, "older_id")
}

func (p *bunqPagination) newerID() string {
	if p == nil {
		return ""
	}
	if id := bunqPageID(p.NewerURL, "newer_id"); id != "" {
		return id
	}
	if id := bunqPageID(p.FutureURL, "newer_id"); id != "" && id != "0" {
		return id
	}
	return ""
}

func bunqPageID(raw *string, param string) string {
	if raw == nil || *raw == "" {
		return ""
	}
	u, err := url.Parse(*raw)
	if err != nil {
		return ""
	}
	return u.Query().Get(param)
}

// bunqHTTPError is a non-2xx answer, with what bunq said about it.
type bunqHTTPError struct {
	Status int
	Errors []bunqAPIError
	Body   string
}

// descriptions joins what bunq said (the error descriptions), without the
// raw body — for matching, where a JSON key like error_description must not
// count as text.
func (e *bunqHTTPError) descriptions() string {
	var parts []string
	for _, er := range e.Errors {
		if er.Description != "" {
			parts = append(parts, er.Description)
		}
	}
	return strings.Join(parts, "; ")
}

func (e *bunqHTTPError) Error() string {
	msg := fmt.Sprintf("bunq API error (HTTP %d)", e.Status)
	var parts []string
	for _, er := range e.Errors {
		if er.Description != "" {
			parts = append(parts, er.Description)
		}
	}
	if len(parts) > 0 {
		msg += ": " + strings.Join(parts, "; ")
	} else if e.Body != "" {
		msg += ": " + e.Body
	}
	if e.Status == http.StatusTooManyRequests {
		msg += " — bunq's rate limit; wait a few seconds before the next call"
	}
	return msg
}

// sign returns the X-Bunq-Client-Signature of a body: SHA-256, RSA PKCS#1
// v1.5, base64. Caller holds mu or has the key loaded.
func (b *Bunq) sign(body []byte) (string, error) {
	sum := sha256.Sum256(body)
	sig, err := rsa.SignPKCS1v15(rand.Reader, b.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

func bunqRequestID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// buildPath fills the {placeholders} of an endpoint's path template.
func bunqBuildPath(ep bunqEndpoint, pathParams map[string]string) (string, error) {
	path := ep.Path
	for k, v := range pathParams {
		if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "/?#%\\") {
			return "", fmt.Errorf("bunq: %s %q is not a valid id", k, v)
		}
		path = strings.ReplaceAll(path, "{"+k+"}", url.PathEscape(v))
	}
	if strings.Contains(path, "{") {
		return "", fmt.Errorf("bunq: path %s has an unfilled parameter", path)
	}
	return bunqVersionPath + path, nil
}

// do performs one HTTP request: the bunq headers, the authentication token
// when given, the signature when there is a body. path is the full path
// under the host (/v1/…).
func (b *Bunq) do(ctx context.Context, method, path string, query url.Values, auth string, headers map[string]string, body []byte) (*http.Response, error) {
	u := b.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", b.userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Bunq-Language", "en_US")
	req.Header.Set("X-Bunq-Region", "nl_NL")
	req.Header.Set("X-Bunq-Client-Request-Id", bunqRequestID())
	req.Header.Set("X-Bunq-Geolocation", "0 0 0 0 000")
	if auth != "" {
		req.Header.Set("X-Bunq-Client-Authentication", auth)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		sig, err := b.sign(body)
		if err != nil {
			return nil, fmt.Errorf("sign request: %w", err)
		}
		req.Header.Set("X-Bunq-Client-Signature", sig)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

// readEnvelope reads a response as the bunq envelope and turns a non-2xx
// status into a bunqHTTPError.
func bunqReadEnvelope(resp *http.Response) (*bunqEnvelope, error) {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, bunqMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	if len(data) > bunqMaxBody {
		if !ok {
			return nil, &bunqHTTPError{Status: resp.StatusCode, Body: trimForError(data)}
		}
		return nil, fmt.Errorf("bunq answer exceeds %d bytes", bunqMaxBody)
	}
	env := bunqEnvelope{Status: resp.StatusCode}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &env); err != nil {
			if !ok {
				return nil, &bunqHTTPError{Status: resp.StatusCode, Body: trimForError(data)}
			}
			return nil, fmt.Errorf("decode bunq response (HTTP %d): %w", resp.StatusCode, err)
		}
	}
	if !ok {
		return nil, &bunqHTTPError{Status: resp.StatusCode, Errors: env.Error, Body: trimForError(data)}
	}
	return &env, nil
}

// request performs a registered call with an explicit token (the way in:
// installation without one, device and session with the installation token)
// and decodes the envelope. headers are extra request headers; each must be
// listed in the endpoint's Headers. Caller holds mu when the key may still
// be unloaded.
func (b *Bunq) request(ctx context.Context, key string, pathParams map[string]string, query url.Values, auth string, headers map[string]string, body any) (*bunqEnvelope, error) {
	ep, ok := bunqEndpoints[key]
	if !ok {
		return nil, fmt.Errorf("bunq: endpoint %q is not registered", key)
	}
	path, err := bunqBuildPath(ep, pathParams)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, p := range ep.Params {
		allowed[p] = true
	}
	for k := range query {
		if !allowed[k] {
			return nil, fmt.Errorf("bunq: endpoint %q does not take query parameter %q", key, k)
		}
	}
	allowedHeaders := map[string]bool{}
	for _, h := range ep.Headers {
		allowedHeaders[strings.ToLower(h)] = true
	}
	for k := range headers {
		if !allowedHeaders[strings.ToLower(k)] {
			return nil, fmt.Errorf("bunq: endpoint %q does not take header %q", key, k)
		}
	}
	var data []byte
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
	}
	resp, err := b.do(ctx, ep.Method, path, query, auth, headers, data)
	if err != nil {
		return nil, err
	}
	return bunqReadEnvelope(resp)
}

// call performs a registered call inside the user's session: {userID} is
// filled in, the session is started or renewed first, and a call bunq
// refuses for a lost session is repeated once after signing in again.
func (b *Bunq) call(ctx context.Context, key string, pathParams map[string]string, query url.Values, body any) (*bunqEnvelope, error) {
	return b.callHeaders(ctx, key, pathParams, query, nil, body)
}

// callHeaders is call with extra request headers (listed in the endpoint's
// Headers). A repeat after a renewed session sends the same headers, so an
// idempotency key stays the same.
func (b *Bunq) callHeaders(ctx context.Context, key string, pathParams map[string]string, query url.Values, headers map[string]string, body any) (*bunqEnvelope, error) {
	for attempt := 0; ; attempt++ {
		token, userID, err := b.ensureSession(ctx)
		if err != nil {
			return nil, err
		}
		params := map[string]string{"userID": fmt.Sprint(userID)}
		for k, v := range pathParams {
			params[k] = v
		}
		env, err := b.request(ctx, key, params, query, token, headers, body)
		if err != nil && attempt == 0 && bunqSessionLost(err) {
			b.invalidateSession(token)
			continue
		}
		return env, err
	}
}

// callRaw performs a pass-through request inside the session (api_get,
// api_request): the caller's path under /v1/, the session token, the
// signature when there is a body. The response is returned as is; a lost
// session is renewed once like in call.
func (b *Bunq) callRaw(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		token, userID, err := b.ensureSession(ctx)
		if err != nil {
			return nil, err
		}
		p := strings.ReplaceAll(path, "{user}", fmt.Sprint(userID))
		resp, err := b.do(ctx, method, p, nil, token, nil, body)
		if err != nil {
			return nil, err
		}
		if attempt == 0 && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			// Read what bunq said: a lost session is renewed, anything else
			// (a right the key lacks) is the caller's answer.
			data, _ := io.ReadAll(io.LimitReader(resp.Body, bunqMaxBody))
			resp.Body.Close()
			var env bunqEnvelope
			_ = json.Unmarshal(data, &env)
			if bunqSessionLost(&bunqHTTPError{Status: resp.StatusCode, Errors: env.Error, Body: trimForError(data)}) {
				b.invalidateSession(token)
				continue
			}
			resp.Body = io.NopCloser(bytes.NewReader(data))
			return resp, nil
		}
		return resp, nil
	}
}

// --- Verify ---

// BunqVerifyInfo is what Verify learned for the Test button: the user the
// key belongs to and the number of accounts it sees.
type BunqVerifyInfo struct {
	UserID      int64
	UserType    string
	DisplayName string
	Accounts    int
	Active      int
	Sandbox     bool
	Expires     time.Time
}

// Verify signs in (installation, device and session as needed) and lists
// the accounts — the two things every tool starts with.
func (b *Bunq) Verify(ctx context.Context) (*BunqVerifyInfo, error) {
	accounts, user, err := b.listAccounts(ctx)
	if err != nil {
		return nil, err
	}
	info := &BunqVerifyInfo{UserID: user.ID, UserType: user.Type, DisplayName: user.DisplayName, Accounts: len(accounts), Sandbox: b.sandbox}
	for _, a := range accounts {
		if a.Status == "ACTIVE" {
			info.Active++
		}
	}
	b.mu.Lock()
	info.Expires = b.api.SessionExpires
	b.mu.Unlock()
	return info, nil
}
