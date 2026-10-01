//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	currentVersion  = "1.0.12"
	releasesAPIURL  = "https://api.github.com/repos/buildcraftlabs/hotfix/releases/latest"
	releasesPageURL = "https://github.com/buildcraftlabs/hotfix/releases/latest"
)

type githubRelease struct {
	TagName string        `json:"tag_name"`
	HTMLURL string        `json:"html_url"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// autoUpdateInterval is how often the silent background updater polls GitHub.
const autoUpdateInterval = 6 * time.Hour

// startAutoUpdater runs silent background update checks: one shortly after
// launch, then every autoUpdateInterval. A newer release is downloaded and
// installed and the app relaunches automatically (see downloadAndReplace).
func startAutoUpdater() {
	safeGo("auto-updater", func() {
		time.Sleep(30 * time.Second) // let startup settle before the first check
		checkForUpdates(true)
		t := time.NewTicker(autoUpdateInterval)
		defer t.Stop()
		for range t.C {
			checkForUpdates(true)
		}
	})
}

// checkForUpdates fetches the latest GitHub release. If a newer version is
// found it downloads the raw binary and swaps it in place.
// When auto is true the check is silent: an up-to-date result produces no tray
// status change (so the periodic poll doesn't flicker the menu label).
func checkForUpdates(auto bool) {
	logf("updater: checking for updates (current: %s)", currentVersion)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, releasesAPIURL, nil)
	if err != nil {
		logf("updater: build request error: %v", err)
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", fmt.Sprintf("Hotfix/%s", currentVersion))

	resp, err := client.Do(req)
	if err != nil {
		logf("updater: request error: %v", err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logf("updater: read error: %v", err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		logf("updater: GitHub API returned %s", resp.Status)
		return
	}

	var release githubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		logf("updater: JSON parse error: %v", err)
		return
	}

	tag := strings.TrimPrefix(release.TagName, "v")
	logf("updater: latest release is %s", tag)

	if !isNewer(tag, currentVersion) {
		logf("updater: already up to date")
		if !auto {
			flashTrayStatus("Up to date")
		}
		return
	}

	// Find the raw binary for this CPU to swap in place — never the tarball.
	binURL := pickRawBinaryURL(release.Assets, runtime.GOARCH)
	if binURL == "" {
		// Releases without a Linux asset (or for another CPU) are expected;
		// only bother the user if they asked.
		logf("updater: no Linux %s binary in release %s", runtime.GOARCH, tag)
		if !auto {
			openURL(releasesPageURL)
		}
		return
	}

	logf("updater: downloading %s from %s", tag, binURL)
	setTrayStatus("Downloading update…", false)
	downloadAndReplace(binURL, auto)
}

// archLabel maps GOARCH to the label used in release asset names.
func archLabel(goarch string) string {
	if goarch == "amd64" {
		return "x86_64"
	}
	return goarch
}

// pickRawBinaryURL returns the download URL of the raw Linux binary for goarch
// (Hotfix-v<version>-Linux-<arch>, no extension) — the one the in-place updater
// swaps onto disk. The .tar.gz bundle is skipped: it's only for first installs.
func pickRawBinaryURL(assets []githubAsset, goarch string) string {
	suffix := "-linux-" + archLabel(goarch)
	for _, a := range assets {
		if strings.HasSuffix(strings.ToLower(a.Name), suffix) {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

// elfMagic starts every Linux executable; checked so an HTML error page or a
// truncated download is never installed over the working binary.
var elfMagic = []byte{0x7f, 'E', 'L', 'F'}

// downloadAndReplace downloads the new binary next to the running one, renames
// it over the executable (atomic, and safe while running on Linux), and
// re-executes. Works without root because install.sh installs per-user under
// ~/.local/bin; if the install location isn't writable (e.g. a system package)
// the update is left to whatever installed it.
func downloadAndReplace(binURL string, auto bool) {
	fail := func(format string, args ...any) {
		logf("updater: "+format, args...)
		flashTrayStatus("Update failed")
	}

	selfPath, err := os.Executable()
	if err != nil {
		fail("could not determine self path: %v", err)
		return
	}
	if resolved, err := filepath.EvalSymlinks(selfPath); err == nil {
		selfPath = resolved
	}

	// Temp file in the same directory so the final rename stays on one filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(selfPath), ".hotfix-update-*")
	if err != nil {
		logf("updater: install location not writable (%v); skipping self-update", err)
		setTrayStatus(baseStatus(getConfig()), false)
		if !auto {
			openURL(releasesPageURL)
		}
		return
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		_ = os.Remove(tmpPath)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(binURL)
	if err != nil {
		cleanup()
		fail("download error: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cleanup()
		fail("download returned %s", resp.Status)
		return
	}

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		cleanup()
		fail("write temp file error: %v", err)
		return
	}

	head := make([]byte, len(elfMagic))
	if _, err := tmp.ReadAt(head, 0); err != nil || !bytes.Equal(head, elfMagic) {
		cleanup()
		fail("downloaded file is not a Linux executable")
		return
	}
	if err := tmp.Chmod(0755); err != nil {
		cleanup()
		fail("chmod error: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		fail("close temp file error: %v", err)
		return
	}

	if err := os.Rename(tmpPath, selfPath); err != nil {
		_ = os.Remove(tmpPath)
		fail("replace error: %v", err)
		return
	}

	logf("updater: update installed, relaunching")
	setTrayStatus("Installing…", false)
	stopMonitor()
	closeLog()

	// Replace this process image with the new binary. The tray icon and D-Bus
	// connections are dropped with the old image and re-created by the new one.
	if err := syscall.Exec(selfPath, os.Args, os.Environ()); err != nil {
		// Still running the old code; the new binary takes over on next launch.
		initLog()
		logf("updater: relaunch failed (%v); update applies on next start", err)
		applyRuntime(getConfig())
	}
}

// isNewer returns true when remote semver is greater than installed semver.
func isNewer(remote, installed string) bool {
	rv := parseSemver(remote)
	cv := parseSemver(installed)
	for i := 0; i < 3; i++ {
		if rv[i] > cv[i] {
			return true
		}
		if rv[i] < cv[i] {
			return false
		}
	}
	return false
}

func parseSemver(v string) [3]int {
	parts := strings.SplitN(v, ".", 3)
	var result [3]int
	for i, p := range parts {
		if i >= 3 {
			break
		}
		n, _ := strconv.Atoi(strings.TrimSpace(p))
		result[i] = n
	}
	return result
}

// openURL opens a URL (or a local file) with the desktop's default handler.
func openURL(target string) {
	cmd := exec.Command("xdg-open", target)
	if err := cmd.Start(); err != nil {
		logf("open: xdg-open %s: %v", target, err)
		return
	}
	go func() { _ = cmd.Wait() }() // reap; xdg-open exits once the handler is launched
}
