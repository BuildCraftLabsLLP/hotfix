//go:build linux

package main

// Subscription licensing. The Linux build is free for 7 days, then a $1/month
// subscription, locked to one machine. There is no license key: the app
// derives a fingerprint of this machine and asks the licensing API
// (worker/src/license.js) whether that fingerprint may run — because it's
// subscribed or still in its free trial, which the server starts the first
// time it sees the machine. Paying happens in the browser on a Razorpay
// Checkout page the API serves — no Razorpay key of any kind is in this binary.
//
// The API answers with a token signed by an Ed25519 key only the server holds;
// this binary carries just the public half. A token names the fingerprint it
// was issued for, so copying one to another machine (or pointing the app at a
// fake server) doesn't unlock anything. The last good token is cached so a
// subscribed machine keeps working offline until that token expires (72h; a
// trial token expires with the trial, so going offline can't extend it).

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
	Status      string `json:"status"`         // "active" or "inactive"
	Plan        string `json:"plan,omitempty"` // "subscription", "trial", "trial_expired" or "lapsed"
	IssuedAt    int64  `json:"iat"`
	Expires     int64  `json:"exp"`
	PeriodEnd   int64  `json:"period_end"`
	TrialEnd    int64  `json:"trial_end,omitempty"` // when the machine's free trial ends
}

// onTrial reports whether the claims unlock the app through the free trial
// rather than a subscription.
func (c licenseClaims) onTrial() bool {
	return c.Status == "active" && c.Plan == "trial" && c.TrialEnd > 0
}

// trialEnd is the trial's end for the license state: 0 unless on the trial.
func (c licenseClaims) trialEnd() int64 {
	if c.onTrial() {
		return c.TrialEnd
	}
	return 0
}

// lockReason is the tray status for an inactive answer.
func (c licenseClaims) lockReason() string {
	if c.Plan == "trial_expired" {
		return reasonTrialEnded
	}
	return reasonSubscriptionRequired
}

const (
	reasonTrialEnded           = "Free trial ended"
	reasonSubscriptionRequired = "Subscription required"
)

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
	licenseActive   atomic.Bool
	licenseTrialEnd atomic.Int64 // unix seconds while on the free trial, else 0
	licenseMu       sync.Mutex   // guards licenseReason
	licenseReason   string       // why the app is locked, for the tray label

	licenseFastUntil atomic.Int64 // UnixNano; poll quickly until then
	licensePoke      = make(chan struct{}, 1)
)

// licensed reports whether the app may run its monitor: always in a build that
// doesn't enforce licensing, otherwise only with a verified active subscription.
func licensed() bool {
	return !licenseEnforced() || licenseActive.Load()
}

// subscribed reports whether a paid subscription (not the trial) unlocks the app.
func subscribed() bool {
	return licenseActive.Load() && licenseTrialEnd.Load() == 0
}

// trialStatusNote is appended to the tray status during the free trial
// (" · Free trial: 3 days left"); empty otherwise.
func trialStatusNote() string {
	end := licenseTrialEnd.Load()
	if end == 0 || !licensed() {
		return ""
	}
	days := trialDaysLeft(end, time.Now())
	if days == 1 {
		return " · Free trial: 1 day left"
	}
	return fmt.Sprintf(" · Free trial: %d days left", days)
}

// trialDaysLeft counts the days until end, rounding up (a few hours left is
// "1 day"), never below 1 while the trial runs.
func trialDaysLeft(end int64, now time.Time) int {
	left := end - now.Unix()
	days := int((left + 86400 - 1) / 86400)
	if days < 1 {
		days = 1
	}
	return days
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
// the monitor and tray in line and tells the user. trialEnd is non-zero while
// the free trial (rather than a subscription) unlocks the app.
func setLicenseState(active bool, trialEnd int64, reason string) {
	licenseMu.Lock()
	reasonChanged := licenseReason != reason
	licenseReason = reason
	licenseMu.Unlock()

	wasTrial := licenseTrialEnd.Swap(trialEnd) != 0
	changed := licenseActive.Swap(active) != active
	trialChanged := wasTrial != (trialEnd != 0)
	// During the trial the tray's days-left count changes, so always refresh.
	if !changed && !reasonChanged && !trialChanged && trialEnd == 0 {
		return
	}

	applyMu.Lock()
	applyRuntime(getConfig())
	applyMu.Unlock()

	switch {
	case active && trialEnd != 0 && changed:
		days := trialDaysLeft(trialEnd, time.Now())
		logf("license: free trial — %d day(s) left", days)
		notifyToast("Hotfix — Free trial",
			fmt.Sprintf("Hotfix is free on this computer for %s, then $1/month.", pluralDays(days)))
	case active && trialEnd == 0 && (changed || wasTrial):
		logf("license: subscription active — monitoring unlocked")
		notifyToast("Hotfix — Subscription active", "Thanks! Hotfix is now protecting this computer.")
	case !active && changed && reason == reasonTrialEnded:
		logf("license: locked (%s)", reason)
		notifyToast("Hotfix — Free trial ended",
			"Hotfix is paused on this computer. To keep it running, open the Hotfix tray menu and choose Subscribe ($1/month).")
	case !active && changed:
		logf("license: locked (%s)", reason)
		notifyToast("Hotfix — Subscription required",
			"Hotfix is paused on this computer. Open the Hotfix tray menu and choose Subscribe ($1/month).")
	}
}

func pluralDays(n int) string {
	if n == 1 {
		return "1 more day"
	}
	return fmt.Sprintf("%d more days", n)
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

// cachedLicense returns the cached token's claims if it is a still-valid
// "active" answer for this machine.
func cachedLicense(pub ed25519.PublicKey, fp string) (licenseClaims, bool) {
	token := loadCachedToken()
	if token == "" {
		return licenseClaims{}, false
	}
	c, err := parseLicenseToken(token, pub, fp, time.Now())
	return c, err == nil && c.Status == "active"
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
		setLicenseState(false, 0, "License check unavailable")
		return true
	}
	fp, err := machineFingerprint()
	if err != nil {
		logf("license: %v", err)
		setLicenseState(false, 0, "No machine ID — can't verify subscription")
		return true
	}

	token, err := fetchLicenseToken(fp)
	if err == nil {
		var c licenseClaims
		if c, err = parseLicenseToken(token, pub, fp, time.Now()); err == nil {
			if c.Status == "active" {
				saveCachedToken(token)
				setLicenseState(true, c.trialEnd(), "")
			} else {
				clearCachedToken()
				setLicenseState(false, 0, c.lockReason())
			}
			return true
		}
	}

	logf("license: check failed: %v", err)
	if c, ok := cachedLicense(pub, fp); ok {
		setLicenseState(true, c.trialEnd(), "")
	} else {
		setLicenseState(false, 0, "Can't verify subscription — check your connection")
	}
	return false
}

// startLicenseWatcher performs the startup check and keeps re-verifying in the
// background. A valid cached token unlocks immediately so a subscribed (or
// trial) machine doesn't wait on the network at every launch.
func startLicenseWatcher() {
	if !licenseEnforced() {
		logf("license: this build does not enforce licensing")
		return
	}
	if pub := licensePubKey(); pub != nil {
		if fp, err := machineFingerprint(); err == nil {
			if c, ok := cachedLicense(pub, fp); ok {
				licenseTrialEnd.Store(c.trialEnd())
				licenseActive.Store(true)
			}
		}
	}

	safeGo("license-watcher", func() {
		for {
			reached := refreshLicense()
			wait := licenseWait(reached, time.Now())
			select {
			case <-time.After(wait):
			case <-licensePoke:
			}
		}
	})
}

// licenseWait is how long the watcher sleeps after a check.
func licenseWait(reached bool, now time.Time) time.Duration {
	switch {
	case !reached:
		return licenseErrorInterval
	case now.UnixNano() < licenseFastUntil.Load() && !subscribed():
		return licenseFastInterval
	case licenseActive.Load():
		// On the trial, check again right when it ends so the app locks on time.
		if end := licenseTrialEnd.Load(); end != 0 {
			untilEnd := time.Unix(end, 0).Sub(now) + time.Second
			return max(min(untilEnd, licenseCheckInterval), licenseFastInterval)
		}
		return licenseCheckInterval
	default:
		return licenseRetryInterval
	}
}

// recheckLicenseNow wakes the watcher for an immediate check.
func recheckLicenseNow() {
	select {
	case licensePoke <- struct{}{}:
	default:
	}
}

// openSubscribePage sends the user to Razorpay Checkout for this machine and
// polls quickly for a while so the app unlocks moments after they pay.
func openSubscribePage() {
	openLicensePage("checkout")
	licenseFastUntil.Store(time.Now().Add(licenseFastWindow).UnixNano())
	recheckLicenseNow()
}

// openManagePage opens the subscription page (status, cancel).
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
