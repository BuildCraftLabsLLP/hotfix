//go:build windows

package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Errorf("short string altered: %q", got)
	}
	long := strings.Repeat("X", 5000)
	got := truncate(long, 3500)
	if !strings.HasPrefix(got, strings.Repeat("X", 3500)) {
		t.Error("truncate dropped the leading content")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncate did not append the truncation marker")
	}
	if len([]rune(got)) <= 3500 {
		t.Errorf("expected marker appended, got %d runes", len([]rune(got)))
	}
}

func TestTailLines(t *testing.T) {
	in := "a\nb\nc\nd\ne"
	if got := tailLines(in, 2); got != "d\ne" {
		t.Errorf("tailLines(2) = %q, want %q", got, "d\ne")
	}
	if got := tailLines(in, 100); got != in {
		t.Errorf("tailLines(>len) = %q, want full input", got)
	}
}

func TestBuildIssueURL(t *testing.T) {
	crash := "panic in event-loop: nil pointer\n\ngoroutine 7 [running]:\nmain.onReady.func1()"
	u := buildIssueURL(crash)

	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("buildIssueURL produced an unparseable URL: %v", err)
	}
	if parsed.Host != "github.com" || parsed.Path != "/buildcraftlabs/hotfix/issues/new" {
		t.Errorf("unexpected target: %s%s", parsed.Host, parsed.Path)
	}

	q := parsed.Query()
	if q.Get("labels") != "crash" {
		t.Errorf("labels = %q, want crash", q.Get("labels"))
	}
	if !strings.Contains(q.Get("title"), currentVersion) {
		t.Errorf("title missing version: %q", q.Get("title"))
	}
	body := q.Get("body")
	if !strings.Contains(body, "event-loop") {
		t.Error("body does not contain the panic location")
	}
	if !strings.Contains(body, currentVersion) {
		t.Error("body does not contain the version")
	}
}
