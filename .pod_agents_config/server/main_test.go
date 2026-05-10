package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSplitManagedPodNamePrefersLongestAgent(t *testing.T) {
	root := t.TempDir()
	agentsDir := filepath.Join(root, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"coder.sh", "little-coder.sh", "pi.sh"} {
		if err := os.WriteFile(filepath.Join(agentsDir, name), []byte("# test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	agent, instance, ok := splitManagedPodName(root, "little-coder-dev")
	if !ok {
		t.Fatal("expected managed pod name")
	}
	if agent != "little-coder" || instance != "dev" {
		t.Fatalf("got %q/%q, want little-coder/dev", agent, instance)
	}
}

func TestFirstPercent(t *testing.T) {
	row := map[string]any{"CPU": "1.23%"}
	if got := firstPercent(row, "CPU"); got != 1.23 {
		t.Fatalf("got %v, want 1.23", got)
	}
}

func TestPodInstructionPath(t *testing.T) {
	agent, instance, ok := parsePodInstructionPath("/api/pods/little-coder/dev/instructions")
	if !ok {
		t.Fatal("expected path to parse")
	}
	if agent != "little-coder" || instance != "dev" {
		t.Fatalf("got %q/%q, want little-coder/dev", agent, instance)
	}
}

func TestAppendAndReadInboxEntry(t *testing.T) {
	root := t.TempDir()
	_, err := appendInboxEntry(root, "instruction", "pi", "dev", "test", "check this", nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := readInboxEntries(root, "pi", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Body != "check this" || entries[0].Status != "pending" {
		t.Fatalf("unexpected entry: %#v", entries[0])
	}
}

func TestBootstrapTokenCreatesOperatorSession(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Format(time.RFC3339)
	if err := saveAuthState(root, authState{
		BootstrapTokenSHA256: tokenHash("secret-token"),
		CreatedAt:            now,
		UpdatedAt:            now,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/auth/login", nil)
	session, err := verifyBootstrapTokenAndCreateSession(root, "secret-token", req)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "pod_session", Value: session})
	if got := currentAuthContext(req, root).Role; got != "operator" {
		t.Fatalf("got role %q, want operator", got)
	}
}
