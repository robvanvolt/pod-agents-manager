package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidGitHubRepo(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"robvanvolt/pod-agents-manager", true},
		{"Owner_1/repo.name-2", true},
		{"owner", false},            // no slash
		{"owner/repo/extra", false}, // too many segments
		{"owner/", false},
		{"/repo", false},
		{"bad space/repo", false},
		{"", false},
	}
	for _, c := range cases {
		if got := validGitHubRepo(c.in); got != c.want {
			t.Errorf("validGitHubRepo(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestValidInstallRef(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"main", true},
		{"v1.2.3", true},
		{"feature/pwa-notifications", true},
		{"", false},         // empty
		{"..", false},       // pure traversal
		{"a..b", false},     // contains ..
		{"-leading", false}, // must start alnum
		{".hidden", false},  // must start alnum
		{"with space", false},
	}
	for _, c := range cases {
		if got := validInstallRef(c.in); got != c.want {
			t.Errorf("validInstallRef(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestStripANSI(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[1;36mbold cyan\x1b[0m text", "bold cyan text"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripANSI(c.in); got != c.want {
			t.Errorf("stripANSI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBatchETA(t *testing.T) {
	// Guard cases — all deterministically return 0.
	guards := []struct {
		name                    string
		started, current, total int64
	}{
		{"no start epoch", 0, 5, 10},
		{"current zero", time.Now().Unix() - 100, 0, 10},
		{"total <= current", time.Now().Unix() - 100, 10, 10},
		{"future start", time.Now().Unix() + 100, 5, 10},
	}
	for _, g := range guards {
		if got := batchETA(g.started, int(g.current), int(g.total)); got != 0 {
			t.Errorf("batchETA(%s) = %d, want 0", g.name, got)
		}
	}

	// Positive case: started 100s ago, 10 of 20 done → ~100s remaining.
	// Allow timing slack since batchETA reads time.Now() internally.
	got := batchETA(time.Now().Unix()-100, 10, 20)
	if got < 90 || got > 110 {
		t.Errorf("batchETA(half done after 100s) = %d, want ~100", got)
	}
}

func TestBatchLineLimitFromRequest(t *testing.T) {
	cases := []struct {
		query string
		want  int
	}{
		{"", 320},            // unset → default
		{"lines=0", 320},     // non-positive → default
		{"lines=-5", 320},    // negative → default
		{"lines=5000", 320},  // over cap → default
		{"lines=abc", 320},   // unparseable → default
		{"lines=50", 50},     // valid
		{"lines=1200", 1200}, // at cap
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/api/batch/log?"+c.query, nil)
		if got := batchLineLimitFromRequest(req); got != c.want {
			t.Errorf("batchLineLimitFromRequest(?%s) = %d, want %d", c.query, got, c.want)
		}
	}
}

func TestBatchExportFilename(t *testing.T) {
	if got := batchExportFilename([]string{"20260101-120000-1"}); got != "pod-batch-20260101-120000-1.zip" {
		t.Errorf("single-id filename = %q", got)
	}
	if got := batchExportFilename([]string{"a", "b", "c"}); got != "pod-batches-3.zip" {
		t.Errorf("multi-id filename = %q", got)
	}
}
