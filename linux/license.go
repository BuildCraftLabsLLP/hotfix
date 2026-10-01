//go:build linux

package main

// Subscription licensing. The Linux build is a $1/month subscription locked to
// one machine. There is no license key: the app derives a fingerprint of this
// machine and asks the licensing API (worker/src/license.js) whether that
// fingerprint has an active subscription. Paying happens in the browser on a
// Stripe Checkout page the API creates — no Stripe key of any kind is in this
// binary.
//
// The API answers with a token signed by an Ed25519 key only the server holds;
// this binary carries just the public half. A token names the fingerprint it
// was issued for, so copying one to another machine (or pointing the app at a
// fake server) doesn't unlock anything. The last good token is cached so a
// subscribed machine keeps working offline until that token expires (72h).

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// licenseAPIBase is the licensing API root (no trailing slash).
	licenseAPIBase = "https://hotfix.buildcraft.town/license"

	// licensePublicKey is the base64 Ed25519 public key that verifies license
	// tokens. It is stamped in at build time:
	//
	//	go build -ldflags "-X main.licensePublicKey=<base64>"
	//
	// Release builds always carry it (CI refuses to release without it). A
	// build without it — a plain local `go build` — does not enforce licensing.
	licensePublicKey = ""
)

const (
	licenseCheckInterval = 6 * time.Hour    // re-verify while licensed
	licenseRetryInterval = time.Hour        // re-verify while unlicensed
	licenseErrorInterval = 30 * time.Second // server unreachable (e.g. network not up yet at login)
	licenseFastInterval  = 10 * time.Second // …right after the user clicks Subscribe
	licenseFastWindow    = 15 * time.Minute
)

// machineIDPaths are read in order for the system's stable machine ID
// (systemd's, then the D-Bus one on non-systemd distros).
var machineIDPaths = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

// machineFingerprint returns this machine's license fingerprint: 64 hex chars.
func machineFingerprint() (string, error) {
	for _, p := range machineIDPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(data)); id != "" {
			return fingerprintFromMachineID(id), nil
		}
	}
	return "", errors.New("no machine ID found (/etc/machine-id is missing or empty)")
}

// fingerprintFromMachineID derives the fingerprint as an app-specific keyed
// hash of the machine ID. The raw ID never leaves the machine (machine-id(5)
// says to treat it as confidential), and the result can't be correlated with
// other software's use of the same ID.
func fingerprintFromMachineID(id string) string {
	mac := hmac.New(sha256.New, []byte(id))
	mac.Write([]byte("hotfix-license-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

// licenseClaims is the signed payload of a license token.
type licenseClaims struct {
	Fingerprint string `json:"fp"`
	Status      string `json:"status"` // "active" or "inactive"
	IssuedAt    int64  `json:"iat"`
	Expires     int64  `json:"exp"`
	PeriodEnd   int64  `json:"period_end"`
}

// parseLicenseToken verifies a token's signature and that it was issued for
// this machine and has not expired. It does not check Status: a correctly
// signed "inactive" answer is a valid token.
//
// Token = base64url(JSON claims) + "." + base64url(signature over the encoded
// claims string), as produced by signToken in worker/src/license.js.
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
	if c.Fingerprint != fp {
		return c, errors.New("token was issued for a different machine")
	}
	if now.Unix() >= c.Expires {
		return c, errors.New("token expired")
	}
	return c, nil
}

// licensePubKey decodes licensePublicKey, or returns nil if it is unset/invalid.
func licensePubKey() ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(licensePublicKey))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(raw)
}

// licenseEnforced reports whether this build checks for a subscription.
func licenseEnforced() bool {
	return strings.TrimSpace(licensePublicKey) != ""
}

// --- State ---

var (
	licenseActive atomic.Bool
	licenseMu     sync.Mutex // guards licenseReason
	licenseReason string     // why the app is locked, for the tray label

	licenseFastUntil atomic.Int64 // UnixNano; poll quickly until then
	licensePoke      = make(chan struct{}, 1)
)

// licensed reports whether the app may run its monitor: always in a build that
// doesn't enforce licensing, otherwise only with a verified active subscription.
func licensed() bool {
	return !licenseEnforced() || licenseActive.Load()
}

// licenseStatusLabel is the tray status shown while locked.
func licenseStatusLabel() string {
	licenseMu.Lock()
	defer licenseMu.Unlock()
	if licenseReason == "" {
		return "Checking subscription…"
	}
	return licenseReason
}

// setLicenseState records the outcome of a check and, when it changed, brings
// the monitor and tray in line and tells the user.
func setLicenseState(active bool, reason string) {
	licenseMu.Lock()
	reasonChanged := licenseReason != reason
	licenseReason = reason
	licenseMu.Unlock()

	changed := licenseActive.Swap(active) != active
	if !changed && !reasonChanged {
		return
	}

	applyMu.Lock()
	applyRuntime(getConfig())
	applyMu.Unlock()

	if !changed {
		return
	}
	if active {
		logf("license: subscription active — monitoring unlocked")
		notifyToast("Hotfix — Subscription active", "Thanks! Hotfix is now protecting this computer.")
	} else {
		logf("license: locked (%s)", reason)
		notifyToast("Hotfix — Subscription required",
			"Hotfix is paused on this computer. Open the Hotfix tray menu and choose Subscribe ($1/month).")
	}
}

// --- Cache ---

func licenseCachePath() string {
	dir := stateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "license.token")
}

func loadCachedToken() string {
	p := licenseCachePath()
	if p == "" {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveCachedToken(token string) {
	if p := licenseCachePath(); p != "" {
		_ = os.MkdirAll(filepath.Dir(p), 0755)
		_ = os.WriteFile(p, []byte(token+"\n"), 0600)
	}
}

func clearCachedToken() {
	if p := licenseCachePath(); p != "" {
		_ = os.Remove(p)
	}
}

// cachedLicenseActive reports whether the cached token is a still-valid
// "active" answer for this machine.
func cachedLicenseActive(pub ed25519.PublicKey, fp string) bool {
	token := loadCachedToken()
	if token == "" {
		return false
	}
	c, err := parseLicenseToken(token, pub, fp, time.Now())
	return err == nil && c.Status == "active"
}

// --- Verification ---

// fetchLicenseToken asks the licensing API for this machine's signed status.
func fetchLicenseToken(fp string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodGet, licenseAPIBase+"/verify?fp="+url.QueryEscape(fp), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("Hotfix/%s (Linux)", currentVersion))

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("license server returned %s", resp.Status)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		return "", errors.New("unexpected license server response")
	}
	return out.Token, nil
}

// refreshLicense runs one full check against the server and applies the result.
// If the server can't be reached (or answers with something we can't trust), a
// cached, unexpired "active" token keeps the app unlocked; anything else locks.
// It reports whether the server gave a trustworthy answer (false ⇒ retry soon).
func refreshLicense() bool {
	if !licenseEnforced() {
		return true
	}
	pub := licensePubKey()
	if pub == nil {
		logf("license: built with an invalid public key")
		setLicenseState(false, "License check unavailable")
		return true
	}
	fp, err := machineFingerprint()
	if err != nil {
		logf("license: %v", err)
		setLicenseState(false, "No machine ID — can't verify subscription")
		return true
	}

	token, err := fetchLicenseToken(fp)
	if err == nil {
		var c licenseClaims
		if c, err = parseLicenseToken(token, pub, fp, time.Now()); err == nil {
			if c.Status == "active" {
				saveCachedToken(token)
				setLicenseState(true, "")
			} else {
				clearCachedToken()
				setLicenseState(false, "Subscription required")
			}
			return true
		}
	}

	logf("license: check failed: %v", err)
	if cachedLicenseActive(pub, fp) {
		setLicenseState(true, "")
	} else {
		setLicenseState(false, "Can't verify subscription — check your connection")
	}
	return false
}

// startLicenseWatcher performs the startup check and keeps re-verifying in the
// background. A valid cached token unlocks immediately so a subscribed machine
// doesn't wait on the network at every launch.
func startLicenseWatcher() {
	if !licenseEnforced() {
		logf("license: this build does not enforce licensing")
		return
	}
	if pub := licensePubKey(); pub != nil {
		if fp, err := machineFingerprint(); err == nil && cachedLicenseActive(pub, fp) {
			licenseActive.Store(true)
		}
	}

	safeGo("license-watcher", func() {
		for {
			reached := refreshLicense()

			wait := licenseRetryInterval
			switch {
			case !reached:
				wait = licenseErrorInterval
			case time.Now().UnixNano() < licenseFastUntil.Load() && !licenseActive.Load():
				wait = licenseFastInterval
			case licenseActive.Load():
				wait = licenseCheckInterval
			}
			select {
			case <-time.After(wait):
			case <-licensePoke:
			}
		}
	})
}

// recheckLicenseNow wakes the watcher for an immediate check.
func recheckLicenseNow() {
	select {
	case licensePoke <- struct{}{}:
	default:
	}
}

// openSubscribePage sends the user to Stripe Checkout for this machine and
// polls quickly for a while so the app unlocks moments after they pay.
func openSubscribePage() {
	openLicensePage("checkout")
	licenseFastUntil.Store(time.Now().Add(licenseFastWindow).UnixNano())
	recheckLicenseNow()
}

// openManagePage opens the Stripe billing portal (update card, cancel).
func openManagePage() {
	openLicensePage("portal")
}

func openLicensePage(endpoint string) {
	fp, err := machineFingerprint()
	if err != nil {
		logf("license: %v", err)
		notifyToast("Hotfix — Can't identify this computer", "No machine ID was found, so a subscription can't be linked to it.")
		return
	}
	openURL(licenseAPIBase + "/" + endpoint + "?fp=" + url.QueryEscape(fp))
}
