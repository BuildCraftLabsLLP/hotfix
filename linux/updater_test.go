//go:build linux

package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		remote, installed string
		want              bool
	}{
		{"1.0.1", "1.0.0", true},
		{"1.1.0", "1.0.9", true},
		{"2.0.0", "1.9.9", true},
		{"1.0.10", "1.0.9", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"0.9.9", "1.0.0", false},
		{"garbage", "1.0.0", false},
	}
	for _, c := range cases {
		if got := isNewer(c.remote, c.installed); got != c.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", c.remote, c.installed, got, c.want)
		}
	}
}

// A full release as CI attaches it: every platform's assets side by side.
var releaseAssets = []githubAsset{
	{Name: "Hotfix-v1.2.3-macOS.dmg", BrowserDownloadURL: "https://example/mac.dmg"},
	{Name: "Hotfix-v1.2.3-Windows.exe", BrowserDownloadURL: "https://example/win.exe"},
	{Name: "Hotfix-Setup-v1.2.3-Windows.exe", BrowserDownloadURL: "https://example/setup.exe"},
	{Name: "Hotfix-v1.2.3-Linux-x86_64.tar.gz", BrowserDownloadURL: "https://example/x64.tar.gz"},
	{Name: "Hotfix-v1.2.3-Linux-x86_64", BrowserDownloadURL: "https://example/x64"},
	{Name: "Hotfix-v1.2.3-Linux-arm64.tar.gz", BrowserDownloadURL: "https://example/arm64.tar.gz"},
	{Name: "Hotfix-v1.2.3-Linux-arm64", BrowserDownloadURL: "https://example/arm64"},
	{Name: "Hotfix-v1.2.3-Linux-x86_64.deb", BrowserDownloadURL: "https://example/x64.deb"},
	{Name: "Hotfix-v1.2.3-Linux-x86_64.rpm", BrowserDownloadURL: "https://example/x64.rpm"},
	{Name: "Hotfix-v1.2.3-Linux-arm64.deb", BrowserDownloadURL: "https://example/arm64.deb"},
	{Name: "Hotfix-v1.2.3-Linux-arm64.rpm", BrowserDownloadURL: "https://example/arm64.rpm"},
}

func TestPickRawBinaryURL_PicksRawBinaryForArch(t *testing.T) {
	if got := pickRawBinaryURL(releaseAssets, "amd64"); got != "https://example/x64" {
		t.Errorf("amd64: got %q", got)
	}
	if got := pickRawBinaryURL(releaseAssets, "arm64"); got != "https://example/arm64" {
		t.Errorf("arm64: got %q", got)
	}
}

func TestPickRawBinaryURL_NoMatch(t *testing.T) {
	if got := pickRawBinaryURL(releaseAssets, "riscv64"); got != "" {
		t.Errorf("unsupported arch should find nothing, got %q", got)
	}
	// A release cut before Linux support has no Linux assets at all.
	if got := pickRawBinaryURL(releaseAssets[:3], "amd64"); got != "" {
		t.Errorf("pre-Linux release should find nothing, got %q", got)
	}
	// Packages must never be installed as the binary.
	var packagesOnly []githubAsset
	for _, a := range releaseAssets {
		if strings.HasSuffix(a.Name, ".tar.gz") || strings.HasSuffix(a.Name, ".deb") || strings.HasSuffix(a.Name, ".rpm") {
			packagesOnly = append(packagesOnly, a)
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if got := pickRawBinaryURL(packagesOnly, arch); got != "" {
			t.Errorf("%s: a package must not be picked, got %q", arch, got)
		}
	}
}

func TestBuildIssueURL(t *testing.T) {
	isolateXDG(t)
	u, err := url.Parse(buildIssueURL("panic: boom\n\ngoroutine 1 [running]"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.String(), issueBaseURL+"?") {
		t.Errorf("unexpected base: %s", u)
	}
	q := u.Query()
	if !strings.Contains(q.Get("title"), currentVersion) || !strings.Contains(q.Get("title"), "Linux") {
		t.Errorf("title should name version and OS: %q", q.Get("title"))
	}
	if q.Get("labels") != "crash" {
		t.Errorf("labels: %q", q.Get("labels"))
	}
	if !strings.Contains(q.Get("body"), "panic: boom") {
		t.Error("body should contain the crash text")
	}
}

func TestTruncateAndTailLines(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate short: %q", got)
	}
	if got := truncate("0123456789abc", 10); !strings.HasPrefix(got, "0123456789") || !strings.Contains(got, "truncated") {
		t.Errorf("truncate long: %q", got)
	}
	if got := tailLines("a\nb\nc\nd\n", 2); got != "c\nd" {
		t.Errorf("tailLines: %q", got)
	}
}
