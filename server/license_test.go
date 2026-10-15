package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func signTestToken(t *testing.T, priv ed25519.PrivateKey, c licenseClaims) string {
	t.Helper()
	raw, _ := json.Marshal(c)
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(body)))
}

func TestParseLicenseToken(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_800_000_000, 0)
	fp := strings.Repeat("a", 64)
	good := licenseClaims{Product: "server", Fingerprint: fp, Status: "active", Plan: "subscription", IssuedAt: now.Unix(), Expires: now.Unix() + 3600}

	if c, err := parseLicenseToken(signTestToken(t, priv, good), pub, fp, now); err != nil || c.Plan != "subscription" {
		t.Fatalf("valid token rejected: %v", err)
	}
	expired, desktop, foreign := good, good, good
	expired.Expires = now.Unix() - 1
	desktop.Product = ""
	foreign.Fingerprint = strings.Repeat("b", 64)
	bad := map[string]string{
		"expired":           signTestToken(t, priv, expired),
		"other product":     signTestToken(t, priv, desktop),
		"other machine":     signTestToken(t, priv, foreign),
		"other key":         signTestToken(t, other, good),
		"tampered":          "x" + signTestToken(t, priv, good),
		"no signature":      strings.SplitN(signTestToken(t, priv, good), ".", 2)[0],
		"garbage":           "!!!.!!!",
		"empty":             "",
		"signature swapped": strings.SplitN(signTestToken(t, priv, good), ".", 2)[0] + "." + strings.SplitN(signTestToken(t, priv, expired), ".", 2)[1],
	}
	for name, tok := range bad {
		if _, err := parseLicenseToken(tok, pub, fp, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A token signed by the real Worker code (worker/src/server-api.js signToken,
// run under Node) must verify here, so the two never drift apart.
func TestParseLicenseToken_WorkerFixture(t *testing.T) {
	const (
		pubB64 = "PwqQvMbLO3AxpAwZibnf7vZm91gtBvzLx4FLVQ/t6J8="
		token  = "eyJwcm9kdWN0Ijoic2VydmVyIiwiZnAiOiJjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjY2NjIiwic2VhdF9pZCI6InNlYXRfMDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3IiwibGljZW5zZV9pZCI6ImxpY18wMTIzNDU2Nzg5YWJjZGVmMDEyMzQ1NjciLCJzdGF0dXMiOiJhY3RpdmUiLCJwbGFuIjoic3Vic2NyaXB0aW9uIiwiaWF0IjoxNzkwMDAwMDAwLCJleHAiOjQxMDI0NDQ4MDAsImxhdGVzdF92ZXJzaW9uIjoiMS4yLjAifQ.qdczgd84IKT76H3syZU3nsj4p33eb9iW1TeXBXwU2FeP0h9nqyOzZ_KIjsSRJzm2QeLFX50lNd3QZES9xjJnAg"
	)
	raw, _ := base64.StdEncoding.DecodeString(pubB64)
	c, err := parseLicenseToken(token, ed25519.PublicKey(raw), strings.Repeat("c", 64), time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatalf("worker-signed token rejected: %v", err)
	}
	if c.SeatID != "seat_0123456789abcdef01234567" || c.Plan != "subscription" || c.LatestVersion != "1.2.0" {
		t.Errorf("claims: %+v", c)
	}
}

func TestFingerprintIsOpaqueAndStable(t *testing.T) {
	a := fingerprintFromMachineID("0123456789abcdef")
	if len(a) != 64 || a != fingerprintFromMachineID("0123456789abcdef") || a == fingerprintFromMachineID("other") {
		t.Fatal("fingerprint must be stable, per machine, 64 hex chars")
	}
	if strings.Contains(a, "0123456789abcdef") {
		t.Error("raw machine id leaked")
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.2.0", "1.1.9", true}, {"1.10.0", "1.9.9", true}, {"1.2.0", "1.2.0", false},
		{"1.2.0", "1.3.0", false}, {"server-v2.0.0", "1.0.0", true}, {"1.2.0", "dev", false},
		{"1.2.1", "1.2.0-ci.5", true}, {"bogus", "1.0.0", false},
	}
	for _, c := range cases {
		if got := isNewer(c.a, c.b); got != c.want {
			t.Errorf("isNewer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// fakeLicenseServer answers like worker/src/server-api.js.
type fakeLicenseServer struct {
	t        *testing.T
	priv     ed25519.PrivateKey
	mu       sync.Mutex
	plan     string            // what trial answers: "trial" or "trial_expired"
	seats    map[string]string // seat id → secret
	seatGone bool
	calls    []string
}

func startLicenseEnv(t *testing.T) *fakeLicenseServer {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	f := &fakeLicenseServer{t: t, priv: priv, plan: "trial", seats: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))

	origKey, origBase, origPath, origID := licensePublicKey, licenseAPIBase, licenseStatePath, readMachineID
	licensePublicKey = base64.StdEncoding.EncodeToString(pub)
	licenseAPIBase = srv.URL + "/api/server"
	dir := t.TempDir()
	licenseStatePath = func() string { return filepath.Join(dir, "license.json") }
	readMachineID = func() (string, error) { return "machine-1", nil }
	t.Cleanup(func() {
		srv.Close()
		licensePublicKey, licenseAPIBase, licenseStatePath, readMachineID = origKey, origBase, origPath, origID
	})
	return f
}

func (f *fakeLicenseServer) serve(w http.ResponseWriter, r *http.Request) {
	var b map[string]string
	json.NewDecoder(r.Body).Decode(&b)
	f.mu.Lock()
	defer f.mu.Unlock()
	ep := strings.TrimPrefix(r.URL.Path, "/api/server/")
	f.calls = append(f.calls, ep)
	now := time.Now().Unix()
	token := func(c licenseClaims) string {
		c.Product, c.Fingerprint, c.IssuedAt = "server", b["fingerprint"], now
		if c.Expires == 0 {
			c.Expires = now + 7*86400
		}
		return signTestToken(f.t, f.priv, c)
	}
	reply := func(v any, code int) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	switch ep {
	case "trial":
		st := map[bool]string{true: "active", false: "inactive"}[f.plan == "trial"]
		reply(map[string]string{"token": token(licenseClaims{Status: st, Plan: f.plan, TrialEnd: now + 3*86400, LatestVersion: "9.9.9"})}, 200)
	case "activate":
		if b["key"] != "HFXS-GOOD" {
			reply(map[string]string{"error": "invalid_key", "message": "Unknown activation key."}, 403)
			return
		}
		id := "seat_" + strings.Repeat("1", 24)
		f.seats[id] = strings.Repeat("e", 64)
		reply(map[string]string{"seat_id": id, "seat_secret": f.seats[id], "token": token(licenseClaims{Status: "active", Plan: "subscription", SeatID: id})}, 200)
	case "refresh":
		if f.seatGone || f.seats[b["seat_id"]] != b["seat_secret"] {
			reply(map[string]string{"error": "seat_gone"}, 410)
			return
		}
		reply(map[string]string{"token": token(licenseClaims{Status: "active", Plan: "subscription", SeatID: b["seat_id"]})}, 200)
	case "deactivate":
		delete(f.seats, b["seat_id"])
		reply(map[string]bool{"ok": true}, 200)
	}
}

func TestLicenseManager_TrialThenExpired(t *testing.T) {
	f := startLicenseEnv(t)
	origVersion := version
	version = "1.0.0"
	t.Cleanup(func() { version = origVersion })
	m := newLicenseManager(defaultConfig)
	if m.Licensed() {
		t.Fatal("no license yet: must not be licensed")
	}
	m.refreshOnce()
	if !m.Licensed() || m.Summary().Plan != "trial" {
		t.Fatalf("trial should license: %+v", m.Summary())
	}
	if st := loadLicenseState(); st.Notified != "9.9.9" {
		t.Error("update notice not recorded")
	}

	f.plan = "trial_expired"
	m.refreshOnce()
	if m.Licensed() {
		t.Fatal("an expired trial must not license")
	}
	if !strings.Contains(m.Summary().Note, "trial has ended") {
		t.Errorf("reason: %q", m.Summary().Note)
	}
}

func TestLicenseState_ActivateRefreshDeactivate(t *testing.T) {
	startLicenseEnv(t)
	pub := licensePubKey()
	cfg := defaultConfig()
	st := loadLicenseState()
	if _, _, err := st.identity(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.activate(cfg, "HFXS-BAD", pub); !isAPIError(err, "invalid_key") || err.Error() != "Unknown activation key." {
		t.Fatalf("bad key: %v", err)
	}
	c, err := st.activate(cfg, "HFXS-GOOD", pub)
	if err != nil || c.Plan != "subscription" || st.SeatID == "" || st.SeatSecret == "" {
		t.Fatalf("activate: %v %+v %+v", err, c, st)
	}
	if err := saveLicenseState(st); err != nil {
		t.Fatal(err)
	}

	m := newLicenseManager(defaultConfig)
	m.loadFromDisk()
	if !m.Licensed() {
		t.Fatal("the service should adopt the CLI's activation from disk")
	}
	m.refreshOnce()
	if !m.Licensed() || m.Summary().Plan != "subscription" {
		t.Fatalf("seat refresh: %+v", m.Summary())
	}

	if err := st.deactivate(cfg); err != nil || st.SeatID != "" {
		t.Fatalf("deactivate: %v", err)
	}
}

func TestLicenseManager_SeatGoneFallsBack(t *testing.T) {
	f := startLicenseEnv(t)
	st := loadLicenseState()
	st.identity()
	st.activate(defaultConfig(), "HFXS-GOOD", licensePubKey())
	saveLicenseState(st)

	f.seatGone = true
	m := newLicenseManager(defaultConfig)
	m.refreshOnce()
	if loadLicenseState().SeatID != "" {
		t.Error("a gone seat must be forgotten")
	}
	f.seatGone = false
	m.refreshOnce() // no seat: back to the trial
	if m.Summary().Plan != "trial" {
		t.Errorf("want trial after losing the seat, got %+v", m.Summary())
	}
}

func TestLicenseManager_AutoActivatesFromKeyFile(t *testing.T) {
	startLicenseEnv(t)
	t.Setenv("HOTFIXD_LICENSE_KEY", "HFXS-GOOD")
	m := newLicenseManager(defaultConfig)
	m.refreshOnce()
	if m.Summary().Plan != "subscription" || loadLicenseState().SeatID == "" {
		t.Fatalf("should activate from HOTFIXD_LICENSE_KEY: %+v", m.Summary())
	}
}

func TestIdentityChangeNeedsNewSeat(t *testing.T) {
	startLicenseEnv(t)
	st := loadLicenseState()
	st.identity()
	st.activate(defaultConfig(), "HFXS-GOOD", licensePubKey())
	before := st.InstanceID

	readMachineID = func() (string, error) { return "machine-2", nil } // cloned / reinstalled
	if _, changed, _ := st.identity(); !changed || st.InstanceID == before || st.SeatID != "" {
		t.Fatal("a new machine identity must drop the seat and mint a new instance id")
	}
}

func TestEngineOnlyReportsWhenUnlicensed(t *testing.T) {
	startLicenseEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := defaultConfig()
	cfg.Mode = ModeEnforce
	writeConfig(path, cfg)
	e, err := newEngine(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.effectiveConfig(); got.Mode != ModeMonitor || e.mon.monitorNote == "" {
		t.Fatalf("unlicensed enforce must only report: %+v", got)
	}
	e.lic.refreshOnce() // trial
	if got := e.effectiveConfig(); got.Mode != ModeEnforce {
		t.Fatal("licensed (trial) enforce should enforce")
	}
}

func TestDevBuildIsNotEnforced(t *testing.T) {
	orig := licensePublicKey
	t.Cleanup(func() { licensePublicKey = orig })
	licensePublicKey = ""
	if licenseEnforced() || !newLicenseManager(defaultConfig).Licensed() {
		t.Error("a build without a public key doesn't enforce licensing")
	}
}
