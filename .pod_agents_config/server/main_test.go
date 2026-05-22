package main

import (
	"archive/zip"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSplitManagedPodNamePrefersLongestAgent(t *testing.T) {
	root := t.TempDir()
	agentsDir := filepath.Join(root, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"coder.sh", "two-word.sh", "pi.sh"} {
		if err := os.WriteFile(filepath.Join(agentsDir, name), []byte("# test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	agent, instance, ok := splitManagedPodName(root, "two-word-dev")
	if !ok {
		t.Fatal("expected managed pod name")
	}
	if agent != "two-word" || instance != "dev" {
		t.Fatalf("got %q/%q, want two-word/dev", agent, instance)
	}
}

func TestFirstPercent(t *testing.T) {
	row := map[string]any{"CPU": "1.23%"}
	if got := firstPercent(row, "CPU"); got != 1.23 {
		t.Fatalf("got %v, want 1.23", got)
	}
}

func TestParsePublishedPorts(t *testing.T) {
	got := parsePublishedPorts(`
f99c7164ed6a4ae1f05aa2ad1f08a70bb8dcb883e663ab34c497a53ec4f704d8
3000/tcp -> 0.0.0.0:3000
5353/udp -> [::]:45353
`)
	ports := got["f99c7164ed6a4ae1f05aa2ad1f08a70bb8dcb883e663ab34c497a53ec4f704d8"]
	if len(ports) != 2 {
		t.Fatalf("got %d ports: %#v", len(ports), ports)
	}
	if ports[0].ContainerPort != "3000" || ports[0].Protocol != "tcp" || ports[0].HostIP != "0.0.0.0" || ports[0].HostPort != "3000" {
		t.Fatalf("unexpected tcp port: %#v", ports[0])
	}
	if ports[1].ContainerPort != "5353" || ports[1].Protocol != "udp" || ports[1].HostIP != "::" || ports[1].HostPort != "45353" {
		t.Fatalf("unexpected udp port: %#v", ports[1])
	}
}

func TestEnrichPublishedPortsMatchesShortStatsID(t *testing.T) {
	row := map[string]any{"ID": "f99c7164ed6a"}
	enrichPublishedPorts(row, map[string][]publishedPort{
		"f99c7164ed6a4ae1": {{ContainerPort: "5173", Protocol: "tcp", HostIP: "0.0.0.0", HostPort: "5173"}},
	})
	ports, ok := row["PublishedPorts"].([]publishedPort)
	if !ok || len(ports) != 1 || ports[0].HostPort != "5173" {
		t.Fatalf("unexpected published ports: %#v", row["PublishedPorts"])
	}
}

func TestClassifyLowCPUAgentPromptAsIdle(t *testing.T) {
	capture := `
The square root of 20 is approximately 4.472.

────────────────────────────────
/workspace
↑1.8k ↓65 1.4%/128k (auto) (rms) Qwen3.6-35B-A3B-8bit
`
	state, detail := classifyLowCPUActivity("pi", capture)
	if state != "idle" {
		t.Fatalf("got %q/%q, want idle", state, detail)
	}
}

func TestClassifyLowCPUForegroundCommandAsRunning(t *testing.T) {
	state, detail := classifyLowCPUActivity("pi", "thinking about the next tool call")
	if state != "running" {
		t.Fatalf("got %q/%q, want running", state, detail)
	}
}

func TestClassifyCommandCodeQuestionPromptAsIdleWithHoverExcerpt(t *testing.T) {
	capture := `
Done. The roadmap is at /workspace/ROADMAP.md.

The core narrative: Agent Skill Smith becomes the standard for Claude tools.
Every Claude developer already lives there.

----------------------------------------------------
Ask your question...
----------------------------------------------------
? for shortcuts
`
	state, detail := classifyLowCPUActivity("node", capture)
	if state != "idle" {
		t.Fatalf("got %q/%q, want idle", state, detail)
	}
	if !strings.Contains(detail, "waiting for input:") || !strings.Contains(detail, "The core narrative:") {
		t.Fatalf("hover detail does not include idle marker and excerpt: %q", detail)
	}
	if strings.Contains(detail, "Ask your question") {
		t.Fatalf("hover detail should use response text, got %q", detail)
	}
}

func TestMovingQuestionPromptRemainsRunning(t *testing.T) {
	capture := `
Still working on tool output...

Ask your question...
`
	state, detail := classifyLowCPUActivityWithStability("node", capture, false)
	if state != "running" || !strings.Contains(detail, "terminal pane changed") {
		t.Fatalf("got %q/%q, want moving terminal to stay running", state, detail)
	}
}

func TestActivityCaptureStableWaitsForSamePane(t *testing.T) {
	activityProbeMutex.Lock()
	activityProbeCapture = map[string]string{}
	activityProbeMutex.Unlock()

	if activityCaptureStable("command-code-main", "node", "first capture") {
		t.Fatal("first capture should not count as stable")
	}
	if activityCaptureStable("command-code-main", "node", "changed capture") {
		t.Fatal("changed capture should not count as stable")
	}
	if !activityCaptureStable("command-code-main", "node", "changed capture") {
		t.Fatal("same capture should count as stable")
	}
}

func TestSplitTerminalProbeOutput(t *testing.T) {
	header, capture := splitTerminalProbeOutput("pi|42|120\n---POD_TERMINAL_CAPTURE---\nhello\n")
	if header != "pi|42|120" || capture != "hello\n" {
		t.Fatalf("got header=%q capture=%q", header, capture)
	}
}

func TestTrimTerminalOutput(t *testing.T) {
	got := trimTerminalOutput("\n\nhello\n\n")
	if got != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
}

func TestNormalizeTerminalInput(t *testing.T) {
	got := normalizeTerminalInput("\r\nhello\r\n")
	if got != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
}

func TestTerminalKeyEvents(t *testing.T) {
	events := terminalKeyEvents("hi\x7f\r\x1b[A")
	if len(events) != 4 {
		t.Fatalf("got %d events: %#v", len(events), events)
	}
	if events[0].Literal != "hi" || events[1].Key != "BSpace" || events[2].Key != "Enter" || events[3].Key != "Up" {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestWebSocketAcceptKey(t *testing.T) {
	got := webSocketAcceptKey("dGhlIHNhbXBsZSBub25jZQ==")
	want := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTerminalSizeFromRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/ws?cols=120&rows=36", nil)
	cols, rows := terminalSizeFromRequest(req)
	if cols != 120 || rows != 36 {
		t.Fatalf("got %dx%d, want 120x36", cols, rows)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/terminal/ws?cols=999&rows=36", nil)
	cols, rows = terminalSizeFromRequest(req)
	if cols != 0 || rows != 0 {
		t.Fatalf("got %dx%d, want invalid size to be ignored", cols, rows)
	}
}

func TestPodJournalUnit(t *testing.T) {
	if got := podJournalUnit("command-code", "main"); got != "command-code@main.service" {
		t.Fatalf("got %q, want command-code@main.service", got)
	}
}

func TestJournalLineLimitFromRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/logs?lines=480", nil)
	if got := journalLineLimitFromRequest(req); got != 480 {
		t.Fatalf("got %d, want 480", got)
	}

	for _, target := range []string{"/api/logs", "/api/logs?lines=0", "/api/logs?lines=9999"} {
		req = httptest.NewRequest(http.MethodGet, target, nil)
		if got := journalLineLimitFromRequest(req); got != 240 {
			t.Fatalf("%s got %d, want default 240", target, got)
		}
	}
}

func TestReadBatchSummaryFromBatchFiles(t *testing.T) {
	root := t.TempDir()
	id := "20260522-121212-42"
	dir := filepath.Join(root, "batch", id)
	if err := os.MkdirAll(filepath.Join(dir, "progress"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := strings.Join([]string{
		"batch_id=" + id,
		"started=2026-05-22T12:12:12+02:00",
		"started_epoch=1780056732",
		"concurrent=1",
		"total=3",
		"targets=pi-dev codex-main",
		"source=/tmp/prompts.txt",
		"pod_manager_version=0.6.0",
	}, "\n")
	for path, data := range map[string]string{
		filepath.Join(dir, "meta.conf"):                   meta,
		filepath.Join(dir, "progress", "pi-dev.prog"):     "3/3\n",
		filepath.Join(dir, "progress", "codex-main.prog"): "1/3\n",
		filepath.Join(dir, "done.pi-dev"):                 "done\n",
		filepath.Join(dir, "logs", "pi-dev.results.jsonl"): strings.Join([]string{
			`{"exit_code":0,"duration_s":12}`,
			`{"exit_code":7,"duration_s":18}`,
			"not json",
		}, "\n"),
	} {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	summary, err := readBatchSummary(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ID != id || !summary.Concurrent || summary.Current != 4 || summary.Total != 6 {
		t.Fatalf("unexpected batch summary: %#v", summary)
	}
	if summary.Status != "interrupted" || len(summary.Targets) != 2 {
		t.Fatalf("unexpected batch status/targets: %#v", summary)
	}
	if summary.Targets[1].Target != "pi-dev" || summary.Targets[1].Status != "done" {
		t.Fatalf("unexpected sorted done target: %#v", summary.Targets)
	}
	if summary.Results.Processed != 2 || summary.Results.Failed != 1 || summary.Results.DurationSeconds != 30 || summary.Results.AverageSeconds != 15 {
		t.Fatalf("unexpected batch results: %#v", summary.Results)
	}
	if summary.Targets[1].Results.Processed != 2 || summary.Targets[0].Results.Processed != 0 {
		t.Fatalf("unexpected per-target results: %#v", summary.Targets)
	}
}

func TestTailTextFileLimitsLinesAndStripsANSI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.log")
	if err := os.WriteFile(path, []byte("one\n\x1b[32mtwo\x1b[0m\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tailTextFile(path, 2, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if got != "three\nfour" {
		t.Fatalf("got %q, want last two plain lines", got)
	}
}

func TestBatchExportIDsFromRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/batches/export?batch=first&batch=second&batches=second,third", nil)
	ids, err := batchExportIDsFromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids, ","); got != "first,second,third" {
		t.Fatalf("got %q, want unique ordered batch ids", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/batches/export?batch=../bad", nil)
	if _, err := batchExportIDsFromRequest(req); err == nil {
		t.Fatal("expected invalid batch export id to fail")
	}
}

func TestWriteBatchExportArchivesSelectedRunFiles(t *testing.T) {
	root := t.TempDir()
	for path, data := range map[string]string{
		filepath.Join(root, "batch", "batch-one", "meta.conf"):      "batch_id=batch-one\n",
		filepath.Join(root, "batch", "batch-one", "logs", "pi.log"): "hello\n",
		filepath.Join(root, "batch", "batch-two", "meta.conf"):      "batch_id=batch-two\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	if err := writeBatchExport(&out, root, []string{"batch-one"}); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, file := range archive.File {
		names[file.Name] = true
	}
	for _, name := range []string{"batch/batch-one/meta.conf", "batch/batch-one/logs/pi.log"} {
		if !names[name] {
			t.Fatalf("archive missing %q: %#v", name, names)
		}
	}
	if names["batch/batch-two/meta.conf"] {
		t.Fatalf("archive included unselected run: %#v", names)
	}
}

func TestPodInstructionPath(t *testing.T) {
	agent, instance, ok := parsePodInstructionPath("/api/pods/two-word/dev/instructions")
	if !ok {
		t.Fatal("expected path to parse")
	}
	if agent != "two-word" || instance != "dev" {
		t.Fatalf("got %q/%q, want two-word/dev", agent, instance)
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

func TestDashboardAPIKeyCreatesLongLivedOperatorSession(t *testing.T) {
	t.Setenv("POD_SERVER_API_KEY", "env-dashboard-key")

	root := t.TempDir()
	req := httptest.NewRequest("POST", "/api/auth/login", nil)
	session, err := verifyDashboardTokenAndCreateSession(root, "env-dashboard-key", req)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "pod_session", Value: session})
	if got := currentAuthContext(req, root).Role; got != "operator" {
		t.Fatalf("got role %q, want operator", got)
	}
	state, err := loadAuthState(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(state.Sessions))
	}
	expires, err := time.Parse(time.RFC3339, state.Sessions[0].ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if expires.Before(time.Now().Add(364 * 24 * time.Hour)) {
		t.Fatalf("session expires too soon: %s", state.Sessions[0].ExpiresAt)
	}
}

func TestLoginRejectsWrongToken(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Format(time.RFC3339)
	if err := saveAuthState(root, authState{
		BootstrapTokenSHA256: tokenHash("secret-token"),
		CreatedAt:            now,
		UpdatedAt:            now,
		Sessions:             []authSession{},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/auth/login", nil)
	if _, err := verifyBootstrapTokenAndCreateSession(root, "wrong-token", req); err == nil || !strings.Contains(err.Error(), "invalid dashboard token") {
		t.Fatalf("got err %v, want invalid dashboard token", err)
	}
	state, err := loadAuthState(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 0 {
		t.Fatalf("got %d sessions, want 0", len(state.Sessions))
	}
}

func TestExpiredSessionDoesNotGrantOperator(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	session := "expired-session"
	if err := saveAuthState(root, authState{
		BootstrapTokenSHA256: tokenHash("secret-token"),
		CreatedAt:            now.Format(time.RFC3339),
		UpdatedAt:            now.Format(time.RFC3339),
		Sessions: []authSession{{
			TokenSHA256: tokenHash(session),
			Role:        "operator",
			CreatedAt:   now.Add(-48 * time.Hour).Format(time.RFC3339),
			ExpiresAt:   now.Add(-24 * time.Hour).Format(time.RFC3339),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/auth/status", nil)
	req.AddCookie(&http.Cookie{Name: "pod_session", Value: session})
	if got := currentAuthContext(req, root).Role; got != "viewer" {
		t.Fatalf("got role %q, want viewer", got)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
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
	revokeSession(root, session)
	req = httptest.NewRequest("GET", "/api/auth/status", nil)
	req.AddCookie(&http.Cookie{Name: "pod_session", Value: session})
	if got := currentAuthContext(req, root).Role; got != "viewer" {
		t.Fatalf("got role %q, want viewer", got)
	}
}

func TestRequireOperatorWritesAuditEntryOnDeny(t *testing.T) {
	root := t.TempDir()
	req := httptest.NewRequest("POST", "/api/action", nil)
	rec := httptest.NewRecorder()
	if _, ok := requireOperator(rec, req, root, "action", "pi-dev"); ok {
		t.Fatal("expected unauthenticated write to be denied")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	data, err := os.ReadFile(auditFile(root))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, `"result":"denied"`) || !strings.Contains(got, `"role":"viewer"`) {
		t.Fatalf("audit entry missing denied viewer result: %s", got)
	}
}

func TestAppendAuditRotatesAndPrunesArchives(t *testing.T) {
	t.Setenv("POD_SERVER_AUDIT_MAX_BYTES", "1")
	t.Setenv("POD_SERVER_AUDIT_MAX_ARCHIVES", "2")

	root := t.TempDir()
	req := httptest.NewRequest("POST", "/api/action", nil)
	for i := 0; i < 4; i++ {
		appendAudit(root, auditEntryFromRequest(req, "action.start", "pi-dev", "ok", "", "operator"))
	}

	current, err := os.ReadFile(auditFile(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(current), `"action":"action.start"`) {
		t.Fatalf("current audit file missing latest entry: %s", string(current))
	}

	archives, err := filepath.Glob(auditArchivePattern(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 2 {
		t.Fatalf("got %d archives, want 2: %#v", len(archives), archives)
	}
	for _, archive := range archives {
		info, err := os.Stat(archive)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() == 0 {
			t.Fatalf("archive is empty: %s", archive)
		}
	}
}

func TestRequireOperatorBlocksCrossSiteOrigin(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	session := "valid-session"
	if err := saveAuthState(root, authState{
		BootstrapTokenSHA256: tokenHash("secret-token"),
		CreatedAt:            now.Format(time.RFC3339),
		UpdatedAt:            now.Format(time.RFC3339),
		Sessions: []authSession{{
			TokenSHA256: tokenHash(session),
			Role:        "operator",
			CreatedAt:   now.Format(time.RFC3339),
			ExpiresAt:   now.Add(time.Hour).Format(time.RFC3339),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "http://127.0.0.1:1337/api/action", nil)
	req.Header.Set("Origin", "http://evil.example:1337")
	req.AddCookie(&http.Cookie{Name: "pod_session", Value: session})
	rec := httptest.NewRecorder()
	if _, ok := requireOperator(rec, req, root, "action", "pi-dev"); ok {
		t.Fatal("expected cross-site write to be blocked")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusForbidden)
	}
	data, err := os.ReadFile(auditFile(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"result":"csrf_blocked"`) {
		t.Fatalf("audit entry missing csrf_blocked result: %s", string(data))
	}
}

func TestLoginRateLimitCapsPerIP(t *testing.T) {
	loginRateMutex.Lock()
	loginRateAttempts = map[string]*loginRateState{}
	loginRateMutex.Unlock()

	req := httptest.NewRequest("POST", "/api/auth/login", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	for i := 0; i < loginRateMaxAttempts; i++ {
		if !reserveLoginAttempt(req) {
			t.Fatalf("attempt %d was rejected before cap", i+1)
		}
	}
	if reserveLoginAttempt(req) {
		t.Fatal("expected attempt after cap to be rejected")
	}
	recordLoginSuccess(req)
	if !reserveLoginAttempt(req) {
		t.Fatal("expected success reset to clear rate limit")
	}
}

func TestSessionCookieSecureFlagUsesTLSOrEnv(t *testing.T) {
	req := httptest.NewRequest("POST", "http://127.0.0.1/api/auth/login", nil)
	rec := httptest.NewRecorder()
	setSessionCookie(rec, req, "session-token")
	cookie := rec.Result().Cookies()[0]
	if cookie.Secure {
		t.Fatal("plain HTTP cookie should not be Secure by default")
	}
	if cookie.MaxAge != int(operatorSessionTTL.Seconds()) {
		t.Fatalf("got MaxAge %d, want %d", cookie.MaxAge, int(operatorSessionTTL.Seconds()))
	}

	t.Setenv("POD_SERVER_FORCE_SECURE_COOKIE", "1")
	rec = httptest.NewRecorder()
	setSessionCookie(rec, req, "session-token")
	if !rec.Result().Cookies()[0].Secure {
		t.Fatal("expected forced secure cookie")
	}
}

func TestPasskeyManagementRenamesDeletesAndHidesPublicKey(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Format(time.RFC3339)
	if err := saveAuthState(root, authState{
		BootstrapTokenSHA256: tokenHash("secret-token"),
		CreatedAt:            now,
		UpdatedAt:            now,
		Passkeys: []passkeyCredential{{
			ID:        "cred-1",
			PublicKey: "secret-public-key",
			CreatedAt: now,
			Label:     "old label",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	keys, err := listPasskeys(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].ID != "cred-1" || keys[0].Label != "old label" {
		t.Fatalf("unexpected passkey summary: %#v", keys)
	}
	data, err := json.Marshal(keys[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-public-key") {
		t.Fatalf("public key leaked in summary: %s", string(data))
	}

	if err := renamePasskey(root, "cred-1", "new label"); err != nil {
		t.Fatal(err)
	}
	keys, err = listPasskeys(root)
	if err != nil {
		t.Fatal(err)
	}
	if keys[0].Label != "new label" {
		t.Fatalf("got label %q, want new label", keys[0].Label)
	}

	if err := deletePasskey(root, "cred-1"); err != nil {
		t.Fatal(err)
	}
	keys, err = listPasskeys(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("got %d passkeys after delete, want 0", len(keys))
	}
}

func TestParseRegistrationAuthDataES256(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cose := testCOSEES256(privateKey)
	rpHash := sha256.Sum256([]byte("127.0.0.1"))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, 0x41)
	authData = binary.BigEndian.AppendUint32(authData, 7)
	authData = append(authData, make([]byte, 16)...)
	authData = binary.BigEndian.AppendUint16(authData, 4)
	authData = append(authData, []byte("cred")...)
	authData = append(authData, cose...)

	parsed, err := parseRegistrationAuthData(authData, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Alg != -7 || parsed.SignCount != 7 || string(parsed.CredentialID) != "cred" {
		t.Fatalf("unexpected parsed auth data: %#v", parsed)
	}
}

func TestVerifyAuthenticationDataES256(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte("127.0.0.1"))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, 0x01)
	authData = binary.BigEndian.AppendUint32(authData, 8)
	clientDataJSON := []byte(`{"type":"webauthn.get","challenge":"abc","origin":"http://127.0.0.1"}`)
	clientHash := sha256.Sum256(clientDataJSON)
	signedData := append([]byte{}, authData...)
	signedData = append(signedData, clientHash[:]...)
	digest := sha256.Sum256(signedData)
	signature, err := ecdsa.SignASN1(rand.Reader, privateKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	key := passkeyCredential{
		PublicKey: base64.RawURLEncoding.EncodeToString(testCOSEES256(privateKey)),
	}
	signCount, err := verifyAuthenticationData(authData, clientDataJSON, signature, key, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if signCount != 8 {
		t.Fatalf("got sign count %d, want 8", signCount)
	}
}

func TestVerifyPasskeyChallengeRejectsOrigin(t *testing.T) {
	err := verifyPasskeyChallenge(passkeyChallenge{
		Challenge: "abc",
		ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339),
	}, webAuthnClientData{
		Type:      "webauthn.get",
		Challenge: "abc",
		Origin:    "http://evil.example",
	}, "webauthn.get", "http://127.0.0.1")
	if err == nil || !strings.Contains(err.Error(), "origin mismatch") {
		t.Fatalf("got err %v, want origin mismatch", err)
	}
}

func testCOSEES256(privateKey *ecdsa.PrivateKey) []byte {
	x := privateKey.PublicKey.X.FillBytes(make([]byte, 32))
	y := privateKey.PublicKey.Y.FillBytes(make([]byte, 32))
	out := []byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20}
	out = append(out, x...)
	out = append(out, 0x22, 0x58, 0x20)
	out = append(out, y...)
	return out
}
