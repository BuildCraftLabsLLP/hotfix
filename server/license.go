package main

// Licensing, the client side of worker/src/server-api.js. Adapted from the
// earlier desktop licensing (git show 1c713ee:linux/license.go): the same
// Ed25519-signed token format, verified against a public key compiled in at
// build time, plus seats, a per-machine trial and service-friendly state.
//
// A server is licensed when it holds a valid "active" token: either a seat on
// a paid license (`hotfixd license activate <key>`) or its 14-day trial.
// Without one, hotfixd keeps watching but never kills (monitor mode).

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	// licensePublicKey is stamped by CI:
	//   -ldflags "-X main.licensePublicKey=<base64 Ed25519 public key>"
	// A build without it (a plain local `go build`) doesn't enforce licensing.
	licensePublicKey = ""

	// licenseAPIBase is the licensing API (no trailing slash).
	licenseAPIBase = "https://hotfix.buildcraft.town/api/server"
)

const (
	licenseProduct       = "server"
	licenseCheckInterval = 6 * time.Hour
	licenseRetryInterval = time.Minute // server unreachable
	licenseLockedRetry   = time.Hour   // answered "inactive"
)

func licenseEnforced() bool { return strings.TrimSpace(licensePublicKey) != "" }

func licensePubKey() ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(licensePublicKey))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(raw)
}

// licenseClaims is a token's signed payload.
type licenseClaims struct {
	Product       string `json:"product"`
	Fingerprint   string `json:"fp"`
	SeatID        string `json:"seat_id,omitempty"`
	LicenseID     string `json:"license_id,omitempty"`
	Status        string `json:"status"` // "active" or "inactive"
	Plan          string `json:"plan"`   // subscription | trial | trial_expired | lapsed
	IssuedAt      int64  `json:"iat"`
	Expires       int64  `json:"exp"`
	TrialEnd      int64  `json:"trial_end,omitempty"`
	LatestVersion string `json:"latest_version,omitempty"`
}

// parseLicenseToken verifies the signature, product, machine and expiry.
// It doesn't check Status: a signed "inactive" answer is a valid token.
func parseLicenseToken(token string, pub ed25519.PublicKey, fp string, now time.Time) (licenseClaims, error) {
	var c licenseClaims
	body, sigPart, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok {
		return c, errors.New("malformed token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil {
		return c, errors.New("malformed token signature")
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, []byte(body), sig) {
		return c, errors.New("bad token signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return c, errors.New("malformed token body")
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, errors.New("malformed token claims")
	}
	if c.Product != licenseProduct {
		return c, errors.New("token is for another product")
	}
	if c.Fingerprint != fp {
		return c, errors.New("token was issued for a different machine")
	}
	if now.Unix() >= c.Expires {
		return c, errors.New("token expired")
	}
	return c, nil
}

// fingerprintFromMachineID derives the license fingerprint as an app-specific
// keyed hash: the raw machine ID never leaves the machine.
func fingerprintFromMachineID(id string) string {
	mac := hmac.New(sha256.New, []byte(id))
	mac.Write([]byte("hotfix-server-license-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

// readMachineID is a variable so tests can fake a machine.
var readMachineID = machineID

func machineFingerprint() (string, error) {
	id, err := readMachineID()
	if err != nil {
		return "", err
	}
	return fingerprintFromMachineID(id), nil
}

// licenseState is persisted in license.json (admin-only). It never holds the
// activation key: a seat refreshes with its own secret.
type licenseState struct {
	InstanceID  string `json:"instance_id"`
	Fingerprint string `json:"fingerprint"` // the fingerprint instance_id was made for
	SeatID      string `json:"seat_id,omitempty"`
	SeatSecret  string `json:"seat_secret,omitempty"`
	Token       string `json:"token,omitempty"`
	// ClockOffset = server time − local time when the token was fetched, so a
	// machine with a skewed clock doesn't misjudge expiry.
	ClockOffset int64  `json:"clock_offset,omitempty"`
	Notified    string `json:"notified_version,omitempty"` // last "update available" logged
}

// licenseStatePath is a variable so tests can point it at a temp dir.
var licenseStatePath = func() string { return filepath.Join(stateDir(), "license.json") }

func loadLicenseState() licenseState {
	var st licenseState
	if data, err := os.ReadFile(licenseStatePath()); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

func saveLicenseState(st licenseState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := licenseStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := restrictToAdmins(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// identity returns the machine's fingerprint and instance id, minting a new
// instance id when there is none or the fingerprint changed (a cloned or
// reinstalled machine): one instance id = one seat.
func (st *licenseState) identity() (fp string, changed bool, err error) {
	fp, err = machineFingerprint()
	if err != nil {
		return "", false, err
	}
	if st.InstanceID == "" || st.Fingerprint != fp {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", false, err
		}
		st.InstanceID = hex.EncodeToString(b[:])
		st.Fingerprint = fp
		st.SeatID, st.SeatSecret, st.Token = "", "", ""
		changed = true
	}
	return fp, changed, nil
}

// claims verifies the stored token for this machine.
func (st licenseState) claims(pub ed25519.PublicKey, fp string, now time.Time) (licenseClaims, error) {
	if st.Token == "" {
		return licenseClaims{}, errors.New("no license yet")
	}
	return parseLicenseToken(st.Token, pub, fp, now.Add(time.Duration(st.ClockOffset)*time.Second))
}

// --- API -----------------------------------------------------------------

type apiError struct {
	Status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("license server answered %d %s", e.Status, e.Code)
}

func machineRequest(st licenseState) map[string]any {
	host, _ := os.Hostname()
	return map[string]any{
		"fingerprint": st.Fingerprint,
		"instance_id": st.InstanceID,
		"hostname":    host,
		"os":          runtime.GOOS + "/" + runtime.GOARCH,
		"version":     version,
		"boot_id":     bootID(),
	}
}

type apiResult struct {
	Token             string `json:"token"`
	SeatID            string `json:"seat_id"`
	SeatSecret        string `json:"seat_secret"`
	DuplicateIdentity bool   `json:"duplicate_identity"`
}

func licenseCall(cfg Config, endpoint string, body map[string]any) (apiResult, error) {
	var out apiResult
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, licenseAPIBase+"/"+endpoint, bytes.NewReader(data))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("hotfixd/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH))
	resp, err := httpClient(cfg, 20*time.Second).Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return out, err
	}
	if resp.StatusCode != http.StatusOK {
		e := &apiError{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, e)
		return out, e
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, errors.New("unexpected answer from the license server")
	}
	return out, nil
}

func isAPIError(err error, code string) bool {
	var e *apiError
	return errors.As(err, &e) && e.Code == code
}

// acceptToken checks a token from the server and stores it.
func (st *licenseState) acceptToken(token string, pub ed25519.PublicKey) (licenseClaims, error) {
	if token == "" {
		return licenseClaims{}, errors.New("unexpected answer from the license server (no token)")
	}
	var raw struct {
		IssuedAt int64 `json:"iat"`
	}
	if body, _, ok := strings.Cut(token, "."); ok {
		if b, err := base64.RawURLEncoding.DecodeString(body); err == nil {
			_ = json.Unmarshal(b, &raw)
		}
	}
	offset := int64(0)
	if raw.IssuedAt > 0 {
		offset = raw.IssuedAt - time.Now().Unix()
	}
	c, err := parseLicenseToken(token, pub, st.Fingerprint, time.Now().Add(time.Duration(offset)*time.Second))
	if err != nil {
		return c, err
	}
	st.Token, st.ClockOffset = token, offset
	return c, nil
}

// activate takes a seat with an activation key.
func (st *licenseState) activate(cfg Config, key string, pub ed25519.PublicKey) (licenseClaims, error) {
	req := machineRequest(*st)
	req["key"] = strings.TrimSpace(key)
	res, err := licenseCall(cfg, "activate", req)
	if err != nil {
		return licenseClaims{}, err
	}
	c, err := st.acceptToken(res.Token, pub)
	if err != nil {
		return c, err
	}
	st.SeatID, st.SeatSecret = res.SeatID, res.SeatSecret
	return c, nil
}

// refresh renews the seat's token, or the trial's when there is no seat.
func (st *licenseState) refresh(cfg Config, pub ed25519.PublicKey) (licenseClaims, apiResult, error) {
	if st.SeatID == "" {
		res, err := licenseCall(cfg, "trial", map[string]any{"fingerprint": st.Fingerprint})
		if err != nil {
			return licenseClaims{}, res, err
		}
		c, err := st.acceptToken(res.Token, pub)
		return c, res, err
	}
	req := machineRequest(*st)
	req["seat_id"], req["seat_secret"] = st.SeatID, st.SeatSecret
	res, err := licenseCall(cfg, "refresh", req)
	if err != nil {
		return licenseClaims{}, res, err
	}
	c, err := st.acceptToken(res.Token, pub)
	return c, res, err
}

func (st *licenseState) deactivate(cfg Config) error {
	if st.SeatID == "" {
		return nil
	}
	_, err := licenseCall(cfg, "deactivate", map[string]any{"seat_id": st.SeatID, "seat_secret": st.SeatSecret})
	if err == nil || isAPIError(err, "seat_gone") {
		st.SeatID, st.SeatSecret, st.Token = "", "", ""
		return nil
	}
	return err
}

// licenseKeyFromConfig returns a key to activate with automatically
// (HOTFIXD_LICENSE_KEY, or the file named by license_key_file), for images
// and autoscaling groups.
func licenseKeyFromConfig(cfg Config) string {
	if k := strings.TrimSpace(os.Getenv("HOTFIXD_LICENSE_KEY")); k != "" {
		return k
	}
	if cfg.LicenseKeyFile != "" {
		if data, err := os.ReadFile(cfg.LicenseKeyFile); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

// --- the service's license manager ---------------------------------------

// licenseManager keeps the service's license current in the background and
// tells the engine whether it may kill.
type licenseManager struct {
	pub    ed25519.PublicKey
	config func() Config

	mu     sync.Mutex
	claims licenseClaims
	ok     bool   // a valid, active token
	reason string // why not, for status/logs
	stamp  time.Time
	wake   chan struct{}
	loaded licenseState // as read at the start of the current refresh (Run's goroutine only)
}

func newLicenseManager(config func() Config) *licenseManager {
	return &licenseManager{pub: licensePubKey(), config: config, wake: make(chan struct{}, 1)}
}

// Licensed reports whether the service may kill.
func (m *licenseManager) Licensed() bool {
	if !licenseEnforced() {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ok {
		return false
	}
	// The token can expire between refreshes (e.g. offline for a week).
	return time.Now().Unix() < m.claims.Expires
}

// Summary describes the license for status.json.
func (m *licenseManager) Summary() licenseSummary {
	if !licenseEnforced() {
		return licenseSummary{Plan: "dev", Note: "unlicensed development build"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := licenseSummary{Plan: m.claims.Plan, Licensed: m.ok && time.Now().Unix() < m.claims.Expires, Expires: m.claims.Expires, TrialEnd: m.claims.TrialEnd, Note: m.reason}
	if s.Plan == "" {
		s.Plan = "none"
	}
	return s
}

type licenseSummary struct {
	Plan     string `json:"plan"`
	Licensed bool   `json:"licensed"`
	Expires  int64  `json:"expires,omitempty"`
	TrialEnd int64  `json:"trial_end,omitempty"`
	Note     string `json:"note,omitempty"`
}

// loadFromDisk adopts the stored token (e.g. one the CLI just wrote) if it
// changed since the last look.
func (m *licenseManager) loadFromDisk() {
	stamp := modTime(licenseStatePath())
	m.mu.Lock()
	same := stamp.Equal(m.stamp)
	m.mu.Unlock()
	if same {
		return
	}
	st := loadLicenseState()
	fp, err := machineFingerprint()
	if err != nil {
		m.set(licenseClaims{}, false, err.Error())
		return
	}
	c, err := st.claims(m.pub, fp, time.Now())
	m.mu.Lock()
	m.stamp = stamp
	m.mu.Unlock()
	if err != nil {
		m.set(licenseClaims{}, false, err.Error())
		return
	}
	m.set(c, c.Status == "active", lockReason(c))
}

func lockReason(c licenseClaims) string {
	switch {
	case c.Status == "active":
		return ""
	case c.Plan == "trial_expired":
		return "the 14-day trial has ended; activate a license key (hotfix.buildcraft.town/server.html)"
	default:
		return "the license's subscription is not active; renew it at hotfix.buildcraft.town/api/server/manage"
	}
}

func (m *licenseManager) set(c licenseClaims, ok bool, reason string) {
	m.mu.Lock()
	changed := ok != m.ok || c.Plan != m.claims.Plan
	m.claims, m.ok, m.reason = c, ok, reason
	m.mu.Unlock()
	if changed && licenseEnforced() {
		if ok {
			if c.Plan == "trial" {
				logf("license: trial, %d days left; kills are allowed", trialDaysLeft(c.TrialEnd))
			} else {
				logf("license: licensed (seat %s)", c.SeatID)
			}
		} else {
			logf("license: not licensed: %s. Monitoring only; nothing will be killed.", reason)
		}
	}
}

func trialDaysLeft(end int64) int {
	left := end - time.Now().Unix()
	if left <= 0 {
		return 0
	}
	return int((left + 86399) / 86400)
}

// Run keeps the license current until stop is closed.
func (m *licenseManager) Run(stop <-chan struct{}) {
	if !licenseEnforced() {
		return
	}
	if m.pub == nil {
		logf("license: this build has an invalid license key; monitoring only")
		return
	}
	m.loadFromDisk()
	for {
		wait := m.refreshOnce()
		select {
		case <-stop:
			return
		case <-m.wake:
		case <-time.After(wait):
		}
	}
}

// Poke asks Run for an immediate refresh.
func (m *licenseManager) Poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// refreshOnce does one round with the license server and returns how long to
// wait before the next.
func (m *licenseManager) refreshOnce() time.Duration {
	cfg := m.config()
	st := loadLicenseState()
	m.loaded = st
	fp, changed, err := st.identity()
	if err != nil {
		m.set(licenseClaims{}, false, err.Error())
		return licenseLockedRetry
	}
	if changed && st.SeatID == "" {
		logf("license: this machine's identity changed (new install or cloned machine); it needs its own seat or trial")
	}

	// A key supplied by config/env activates a server that has no seat.
	if st.SeatID == "" {
		if key := licenseKeyFromConfig(cfg); key != "" {
			if c, err := st.activate(cfg, key, m.pub); err == nil {
				logf("license: activated with the configured key")
				m.save(st, c)
				return licenseCheckInterval
			} else {
				logf("license: activating with the configured key failed: %v", err)
			}
		}
	}

	c, res, err := st.refresh(cfg, m.pub)
	switch {
	case isAPIError(err, "seat_gone"):
		logf("license: this server's seat was released or taken by another machine with the same identity. Run `hotfixd license activate <key>` (or `hotfixd license reset-identity` on a cloned machine).")
		st.SeatID, st.SeatSecret, st.Token = "", "", ""
		m.save(st, licenseClaims{})
		return licenseRetryInterval
	case err != nil:
		// Offline: the stored token keeps working until it expires.
		if prev, perr := st.claims(m.pub, fp, time.Now()); perr == nil {
			m.set(prev, prev.Status == "active", lockReason(prev))
		} else {
			m.set(licenseClaims{}, false, "can't reach the license server ("+err.Error()+")")
		}
		return licenseRetryInterval
	}
	if res.DuplicateIdentity {
		logf("license: this seat is also used by another machine (cloned image?). Run `hotfixd license reset-identity` on the clone.")
	}
	if c.LatestVersion != "" && c.LatestVersion != st.Notified && isNewer(c.LatestVersion, version) {
		logf("update: Hotfix Server %s is available (this is %s): https://hotfix.buildcraft.town/server.html", c.LatestVersion, version)
		st.Notified = c.LatestVersion
	}
	m.save(st, c)
	if c.Status != "active" {
		return licenseLockedRetry
	}
	wait := licenseCheckInterval
	if c.Plan == "trial" && c.TrialEnd > 0 {
		if untilEnd := time.Until(time.Unix(c.TrialEnd, 0)) + time.Second; untilEnd < wait {
			wait = max(untilEnd, time.Minute)
		}
	}
	return wait
}

// save stores the result of a refresh, unless the CLI changed the license
// meanwhile (e.g. `hotfixd license activate` while a refresh was in flight):
// then the CLI's version wins and is adopted on the next look.
func (m *licenseManager) save(st licenseState, c licenseClaims) {
	if disk := loadLicenseState(); disk.SeatID != m.loaded.SeatID || disk.InstanceID != m.loaded.InstanceID || disk.Token != m.loaded.Token {
		m.mu.Lock()
		m.stamp = time.Time{}
		m.mu.Unlock()
		m.loadFromDisk()
		return
	}
	if err := saveLicenseState(st); err != nil {
		logf("license: saving state: %v", err)
	}
	m.mu.Lock()
	m.stamp = modTime(licenseStatePath())
	m.mu.Unlock()
	m.set(c, c.Status == "active", lockReason(c))
}

// isNewer reports whether version a is newer than b (dotted numbers; anything
// unparsable is never newer).
func isNewer(a, b string) bool {
	pa, pb := parseVersion(a), parseVersion(b)
	if pa == nil || pb == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseVersion(v string) []int {
	v = strings.TrimPrefix(strings.TrimPrefix(v, "server-"), "v")
	v, _, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n := 0
		for _, ch := range p {
			if ch < '0' || ch > '9' {
				return nil
			}
			n = n*10 + int(ch-'0')
		}
		out[i] = n
	}
	return out
}
