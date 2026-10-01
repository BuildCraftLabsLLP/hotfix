//go:build linux

package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// signTestToken builds a token exactly as worker/src/license.js does.
func signTestToken(t *testing.T, priv ed25519.PrivateKey, c licenseClaims) string {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(body)))
}

func testKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// useMachineID points the fingerprint at a fake machine-id file.
func useMachineID(t *testing.T, id string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "machine-id")
	if err := os.WriteFile(p, []byte(id+"\n"), 0444); err != nil {
		t.Fatal(err)
	}
	orig := machineIDPaths
	machineIDPaths = []string{p}
	t.Cleanup(func() { machineIDPaths = orig })
	fp, err := machineFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestFingerprint_StableOpaqueAndPerMachine(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	fp := fingerprintFromMachineID(id)

	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fp) {
		t.Fatalf("fingerprint must be 64 lowercase hex chars, got %q", fp)
	}
	if fp != fingerprintFromMachineID(id) {
		t.Error("fingerprint must be stable for the same machine")
	}
	if fp == fingerprintFromMachineID("fedcba9876543210fedcba9876543210") {
		t.Error("different machines must get different fingerprints")
	}
	if strings.Contains(fp, id) {
		t.Error("fingerprint must not expose the raw machine ID")
	}
}

func TestMachineFingerprint_ReadsFirstAvailableID(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope")
	empty := filepath.Join(dir, "empty")
	good := filepath.Join(dir, "machine-id")
	_ = os.WriteFile(empty, []byte("\n"), 0644)
	_ = os.WriteFile(good, []byte("abc123\n"), 0644)

	orig := machineIDPaths
	t.Cleanup(func() { machineIDPaths = orig })

	machineIDPaths = []string{missing, empty, good}
	fp, err := machineFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if fp != fingerprintFromMachineID("abc123") {
		t.Error("should fall through to the first non-empty machine ID, trimmed")
	}

	machineIDPaths = []string{missing, empty}
	if _, err := machineFingerprint(); err == nil {
		t.Error("expected an error when no machine ID exists")
	}
}

func TestParseLicenseToken(t *testing.T) {
	pub, priv := testKeys(t)
	_, otherPriv := testKeys(t)
	now := time.Unix(1_800_000_000, 0)
	const fp = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	good := licenseClaims{Fingerprint: fp, Status: "active", IssuedAt: now.Unix(), Expires: now.Unix() + 3600}

	c, err := parseLicenseToken(signTestToken(t, priv, good), pub, fp, now)
	if err != nil || c.Status != "active" || c.Fingerprint != fp {
		t.Fatalf("valid token rejected: %v (%+v)", err, c)
	}

	expired := good
	expired.Expires = now.Unix() - 1
	tampered := signTestToken(t, priv, good)
	inactive := good
	inactive.Status = "inactive"
	// Flip the signed claims to "active" while keeping the "inactive" signature.
	forged := strings.SplitN(signTestToken(t, priv, inactive), ".", 2)
	forgedToken := strings.SplitN(signTestToken(t, otherPriv, good), ".", 2)[0] + "." + forged[1]

	bad := map[string]string{
		"other machine's token": signTestToken(t, priv, licenseClaims{Fingerprint: strings.Repeat("b", 64), Status: "active", Expires: good.Expires}),
		"expired":               signTestToken(t, priv, expired),
		"signed by another key": signTestToken(t, otherPriv, good),
		"claims swapped":        forgedToken,
		"tampered body":         "x" + tampered,
		"no separator":          "abcdef",
		"empty":                 "",
		"garbage signature":     strings.SplitN(tampered, ".", 2)[0] + ".!!!",
		"truncated signature":   tampered[:len(tampered)-4],
		"unsigned (empty sig)":  strings.SplitN(tampered, ".", 2)[0] + ".",
	}
	for name, token := range bad {
		if _, err := parseLicenseToken(token, pub, fp, now); err == nil {
			t.Errorf("%s: token must be rejected", name)
		}
	}

	if _, err := parseLicenseToken(signTestToken(t, priv, good), nil, fp, now); err == nil {
		t.Error("a missing public key must reject every token")
	}
}

// A token produced by the real Worker code (worker/src/license.js signToken,
// run under Node with a throwaway key) must verify here: guards the wire
// format against drifting between the two implementations.
func TestParseLicenseToken_WorkerFixture(t *testing.T) {
	const (
		pubB64 = "u/tXE9dmg17LmjZg7cWR+hHJ2aysCT7ED37aexTXPbc="
		token  = "eyJmcCI6ImNjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2MiLCJzdGF0dXMiOiJhY3RpdmUiLCJpYXQiOjE3OTAwMDAwMDAsImV4cCI6NDEwMjQ0NDgwMCwicGVyaW9kX2VuZCI6MTc5MjAwMDAwMH0.kM6HitimFBRMlMi97ickV5PAS3IRV0RYWsOs1jBChtxA6_sL_8Q-x8usOvGGf56QJKUrF4n68vFMmrM17odKCw"
	)
	raw, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		t.Fatal(err)
	}
	fp := strings.Repeat("c", 64)
	c, err := parseLicenseToken(token, ed25519.PublicKey(raw), fp, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatalf("worker-signed token rejected: %v", err)
	}
	if c.Status != "active" || c.PeriodEnd != 1_792_000_000 || c.IssuedAt != 1_790_000_000 {
		t.Errorf("unexpected claims: %+v", c)
	}
}

func TestLicensed_DevBuildIsNotEnforced(t *testing.T) {
	origKey := licensePublicKey
	t.Cleanup(func() { licensePublicKey = origKey; licenseActive.Store(false) })

	licensePublicKey = ""
	licenseActive.Store(false)
	if licenseEnforced() || !licensed() {
		t.Error("a build without a public key must not enforce licensing")
	}

	licensePublicKey = "not-a-real-key"
	if !licenseEnforced() || licensed() {
		t.Error("a build with a key must be locked until a license is verified")
	}
	if licensePubKey() != nil {
		t.Error("an undecodable key must not yield a usable public key")
	}
}

// licenseServer fakes the licensing API. status is what /verify answers for
// the requesting fingerprint; "down" makes it fail.
type licenseServer struct {
	status atomic.Value // string
	hits   atomic.Int32
}

func startLicenseEnv(t *testing.T) (*licenseServer, string) {
	t.Helper()
	isolateXDG(t)
	fp := useMachineID(t, "feedfacefeedfacefeedfacefeedface")
	pub, priv := testKeys(t)

	ls := &licenseServer{}
	ls.status.Store("inactive")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.hits.Add(1)
		status := ls.status.Load().(string)
		if r.URL.Path != "/license/verify" || status == "down" {
			http.Error(w, "nope", http.StatusBadGateway)
			return
		}
		now := time.Now().Unix()
		reqFP := r.URL.Query().Get("fp")
		var token string
		switch status {
		case "wrong-machine":
			token = signTestToken(t, priv, licenseClaims{Fingerprint: strings.Repeat("9", 64), Status: "active", IssuedAt: now, Expires: now + 3600})
		case "forged":
			_, evil := testKeys(t)
			token = signTestToken(t, evil, licenseClaims{Fingerprint: reqFP, Status: "active", IssuedAt: now, Expires: now + 3600})
		default:
			token = signTestToken(t, priv, licenseClaims{Fingerprint: reqFP, Status: status, IssuedAt: now, Expires: now + 3600})
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
	}))

	origBase, origKey := licenseAPIBase, licensePublicKey
	licenseAPIBase = srv.URL + "/license"
	licensePublicKey = base64.StdEncoding.EncodeToString(pub)
	licenseActive.Store(false)
	t.Cleanup(func() {
		srv.Close()
		licenseAPIBase, licensePublicKey = origBase, origKey
		licenseActive.Store(false)
		licenseMu.Lock()
		licenseReason = ""
		licenseMu.Unlock()
	})
	return ls, fp
}

func TestRefreshLicense_LocksWithoutSubscription(t *testing.T) {
	ls, _ := startLicenseEnv(t)

	if reached := refreshLicense(); !reached {
		t.Error("server answered; should count as reached")
	}
	if licensed() {
		t.Fatal("no subscription ⇒ the app must stay locked")
	}
	if got := licenseStatusLabel(); got != "Subscription required" {
		t.Errorf("status label: %q", got)
	}
	if loadCachedToken() != "" {
		t.Error("an inactive answer must not be cached")
	}
	if ls.hits.Load() != 1 {
		t.Errorf("expected one verify call, got %d", ls.hits.Load())
	}
}

func TestRefreshLicense_UnlocksAndCachesWhenActive(t *testing.T) {
	ls, fp := startLicenseEnv(t)
	ls.status.Store("active")

	refreshLicense()
	if !licensed() {
		t.Fatal("active subscription ⇒ unlocked")
	}
	if !cachedLicenseActive(licensePubKey(), fp) {
		t.Error("the active token should be cached for offline use")
	}
	if fi, err := os.Stat(licenseCachePath()); err != nil || fi.Mode().Perm() != 0600 {
		t.Errorf("license cache should be a private file: %v", err)
	}
}

func TestRefreshLicense_OfflineGraceUsesCachedToken(t *testing.T) {
	ls, _ := startLicenseEnv(t)
	ls.status.Store("active")
	refreshLicense()

	ls.status.Store("down")
	licenseActive.Store(false) // as after a restart
	if reached := refreshLicense(); reached {
		t.Error("a failing server must report unreachable so the watcher retries soon")
	}
	if !licensed() {
		t.Error("a cached active token must keep the app unlocked while offline")
	}
}

func TestRefreshLicense_OfflineWithoutCacheStaysLocked(t *testing.T) {
	ls, _ := startLicenseEnv(t)
	ls.status.Store("down")
	refreshLicense()
	if licensed() {
		t.Error("offline with no cached license must stay locked")
	}
}

func TestRefreshLicense_CancellationLocksAndClearsCache(t *testing.T) {
	ls, _ := startLicenseEnv(t)
	ls.status.Store("active")
	refreshLicense()

	ls.status.Store("inactive")
	refreshLicense()
	if licensed() {
		t.Error("a cancelled subscription must lock the app on the next check")
	}
	if loadCachedToken() != "" {
		t.Error("the cached token must be dropped once the server says inactive")
	}
	// …so going offline afterwards can't resurrect it.
	ls.status.Store("down")
	refreshLicense()
	if licensed() {
		t.Error("must stay locked offline after a cancellation")
	}
}

func TestRefreshLicense_RejectsForgedAndForeignTokens(t *testing.T) {
	for _, mode := range []string{"forged", "wrong-machine"} {
		t.Run(mode, func(t *testing.T) {
			ls, _ := startLicenseEnv(t)
			ls.status.Store(mode)
			refreshLicense()
			if licensed() {
				t.Errorf("%s token must not unlock the app", mode)
			}
			if loadCachedToken() != "" {
				t.Errorf("%s token must not be cached", mode)
			}
		})
	}
}

// A license cached on one machine is useless when copied to another.
func TestCachedToken_NotTransferableToAnotherMachine(t *testing.T) {
	ls, _ := startLicenseEnv(t)
	ls.status.Store("active")
	refreshLicense()
	if !licensed() {
		t.Fatal("precondition: machine A is licensed")
	}

	// Same state dir (the copied cache file), different machine, no server.
	otherFP := useMachineID(t, "0ther0ther0ther0ther0ther0ther00")
	ls.status.Store("down")
	licenseActive.Store(false)
	refreshLicense()
	if licensed() {
		t.Error("a token copied from another machine must not unlock this one")
	}
	if cachedLicenseActive(licensePubKey(), otherFP) {
		t.Error("cached token must not validate for a different fingerprint")
	}
}

func TestRefreshLicense_NoMachineIDLocks(t *testing.T) {
	startLicenseEnv(t)
	machineIDPaths = []string{filepath.Join(t.TempDir(), "missing")}
	refreshLicense()
	if licensed() {
		t.Error("without a machine ID the license can't be verified ⇒ locked")
	}
}

// With licensing enforced and no subscription, the monitor must not run even
// though the config says enabled.
func TestCheckProcesses_NoOpWhenUnlicensed(t *testing.T) {
	startLicenseEnv(t) // enforced, locked
	resetSamples()
	t.Cleanup(resetSamples)

	configMu.Lock()
	saved := current
	current = defaultConfig()
	configMu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		current = saved
		configMu.Unlock()
	})

	checkProcesses()
	sampleMu.Lock()
	sampled := prevSamples != nil
	sampleMu.Unlock()
	if sampled {
		t.Error("an unlicensed app must not even sample processes")
	}
}
