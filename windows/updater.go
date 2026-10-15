//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	currentVersion  = "1.0.12"
	releasesAPIURL  = "https://api.github.com/repos/buildcraftlabs/hotfix/releases/latest"
	releasesPageURL = "https://github.com/buildcraftlabs/hotfix/releases/latest"
)

type githubRelease struct {
	TagName string          `json:"tag_name"`
	HTMLURL string          `json:"html_url"`
	Assets  []githubAsset   `json:"assets"`
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
// found it downloads the .exe in the background and self-replaces on exit.
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

	var release githubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		logf("updater: JSON parse error: %v", err)
		return
	}

	tag := release.TagName
	if strings.HasPrefix(tag, "v") {
		tag = tag[1:]
	}
	logf("updater: latest release is %s", tag)

	if !isNewer(tag, currentVersion) {
		logf("updater: already up to date")
		if !auto {
			setTrayStatus("Up to date", false)
			time.AfterFunc(3*time.Second, func() { setTrayStatus("Watching", false) })
		}
		return
	}

	// Find the raw app .exe to swap in place — never the Setup installer.
	exeURL := pickRawExeURL(release.Assets)

	if exeURL == "" {
		logf("updater: no .exe asset found, opening browser")
		openURL(releasesPageURL)
		return
	}

	logf("updater: downloading %s from %s", tag, exeURL)
	setTrayStatus("Downloading update…", false)
	go downloadAndReplace(exeURL, tag)
}

// pickRawExeURL returns the download URL of the raw Hotfix .exe asset — the one
// the in-place updater swaps onto disk. The Setup installer (Hotfix-Setup-*.exe)
// is skipped: it's only for first-time installs, not background updates.
func pickRawExeURL(assets []githubAsset) string {
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if strings.HasSuffix(name, ".exe") && !strings.Contains(name, "setup") {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

func downloadAndReplace(exeURL, version string) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(exeURL)
	if err != nil {
		logf("updater: download error: %v", err)
		setTrayStatus("Update failed", false)
		time.AfterFunc(3*time.Second, func() { setTrayStatus("Watching", false) })
		return
	}
	defer resp.Body.Close()

	tmpDir := os.TempDir()
	newExePath := filepath.Join(tmpDir, "Hotfix_new.exe")

	f, err := os.Create(newExePath)
	if err != nil {
		logf("updater: create temp file error: %v", err)
		return
	}
	if _, err = io.Copy(f, resp.Body); err != nil {
		f.Close()
		logf("updater: write temp file error: %v", err)
		return
	}
	f.Close()

	// Get the path of the currently running .exe
	selfPath, err := os.Executable()
	if err != nil {
		logf("updater: could not determine self path: %v", err)
		return
	}

	// PowerShell one-liner: wait for us to exit, swap exe, relaunch, clean up.
	// Single-quoting paths is safe on Windows (paths never contain single quotes).
	// Running via hiddenCmd + -WindowStyle Hidden means zero visible windows.
	psScript := fmt.Sprintf(
		`Start-Sleep -Seconds 2; `+
			`Copy-Item -Force -LiteralPath '%s' -Destination '%s'; `+
			`Start-Process -FilePath '%s'; `+
			`Remove-Item -Force -LiteralPath '%s'`,
		newExePath, selfPath, selfPath, newExePath,
	)

	logf("updater: update downloaded, launching installer and exiting")
	setTrayStatus("Installing…", false)

	cmd := hiddenCmd("powershell", "-NonInteractive", "-NoProfile",
		"-WindowStyle", "Hidden", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	if err := cmd.Start(); err != nil {
		logf("updater: launch installer error: %v", err)
		return
	}

	// Quit so PowerShell can replace our exe
	go func() {
		time.Sleep(500 * time.Millisecond)
		systrayQuit()
	}()
}

// systrayQuit is called on the main goroutine via a deferred call to avoid
// calling systray.Quit from within the event loop handler.
var systrayQuitCh = make(chan struct{}, 1)

func systrayQuit() {
	systrayQuitCh <- struct{}{}
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

// openURL opens a URL in the default Windows browser without a console flash.
func openURL(url string) {
	cmd := hiddenCmd("cmd", "/c", "start", url)
	if err := cmd.Start(); err != nil {
		logf("updater: open URL error: %v", err)
	}
}
