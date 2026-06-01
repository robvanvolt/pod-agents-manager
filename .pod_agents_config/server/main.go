package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	statsCache           []byte
	cacheMutex           sync.RWMutex
	authMutex            sync.Mutex
	auditMutex           sync.Mutex
	loginRateMutex       sync.Mutex
	loginRateAttempts    = map[string]*loginRateState{}
	activityProbeMutex   sync.Mutex
	activityProbeCapture = map[string]string{}
	activityBusyCPUPct   = 1.0
	activityProbeTimeout = 900 * time.Millisecond
	updateStatusMutex    sync.Mutex
	updateStatusCache    updateStatusCacheEntry
)

const (
	loginRateWindow         = time.Minute
	loginRateMaxAttempts    = 10
	operatorSessionTTL      = 365 * 24 * time.Hour
	defaultAuditMaxBytes    = int64(1024 * 1024)
	defaultAuditMaxArchives = 5
	updateStatusTTL         = 15 * time.Minute
	terminalAttachScript    = `
if [ "$POD_TERMINAL_COLS" -gt 0 ] 2>/dev/null && [ "$POD_TERMINAL_ROWS" -gt 0 ] 2>/dev/null; then
	stty cols "$POD_TERMINAL_COLS" rows "$POD_TERMINAL_ROWS" 2>/dev/null || true
fi
# Apply the dashboard's tmux status-bar style live, so existing pods get
# the new bar without needing a full image rebuild. The same options are
# baked into /etc/tmux.conf for fresh builds.
tmux set-option -g status on 2>/dev/null
tmux set-option -g status-position bottom 2>/dev/null
tmux set-option -g status-style 'bg=#1f2937,fg=#e5e7eb' 2>/dev/null
tmux set-option -g status-left ' #[bold]#S #[default]· #W ' 2>/dev/null
tmux set-option -g status-right '#{pane_current_command} · %H:%M ' 2>/dev/null
tmux set-option -g window-status-current-style 'bold,fg=#fbbf24' 2>/dev/null
cd /workspace 2>/dev/null || cd "$HOME" 2>/dev/null || true
tmux attach-session -t bot
status=$?
if [ "$status" -eq 0 ]; then
	printf '\r\n[detached from bot - pod shell in %s]\r\n' "$(pwd)"
	# Use bash explicitly when available — readline gives us reliable
	# Ctrl+D-at-empty-prompt → exit behavior. Fall back to sh otherwise.
	if command -v bash >/dev/null 2>&1; then
		exec bash -i
	else
		exec sh -i
	fi
fi
exit "$status"
`
)

func main() {
	// Background loop refreshes podman stats every few seconds
	go updateStatsLoop()

	root := filepath.Join(os.Getenv("HOME"), ".pod_agents_config")

	// Load env from .env file into the process environment
	envVars := readEnvFile(root)
	for k, v := range envVars {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
		// Map special POD_* variables to standard OpenAI names if they are not set
		if k == "POD_OPENAI_API_KEY" && os.Getenv("OPENAI_API_KEY") == "" {
			os.Setenv("OPENAI_API_KEY", v)
		}
		if k == "POD_OPENAI_BASE_URL" && os.Getenv("OPENAI_BASE_URL") == "" {
			os.Setenv("OPENAI_BASE_URL", v)
		}
		if k == "POD_DEFAULT_MODEL" && os.Getenv("DEFAULT_MODEL") == "" {
			os.Setenv("DEFAULT_MODEL", v)
		}
	}

	go startAgentManagerLoop(root)
	mux := http.NewServeMux()
	fileServer := http.FileServer(http.Dir("./static"))
	mux.Handle("/", noCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sw.js" {
			w.Header().Set("Content-Type", "application/javascript")
		} else if r.URL.Path == "/manifest.json" {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		fileServer.ServeHTTP(w, r)
	})))

	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		if !requireOperatorRead(w, r, root) {
			return
		}
		cacheMutex.RLock()
		defer cacheMutex.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		if len(statsCache) == 0 {
			w.Write([]byte("[]"))
			return
		}
		w.Write(statsCache)
	})

	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		if !requireOperatorRead(w, r, root) {
			return
		}
		hostname, _ := os.Hostname()
		resp := map[string]any{
			"hostname": hostname,
			"ips":      localIPs(),
			"time":     time.Now().Format(time.RFC3339),
			"version":  readPodVersion(root),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/api/update-status", func(w http.ResponseWriter, r *http.Request) {
		if !requireOperatorRead(w, r, root) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(readUpdateStatus(root, r.URL.Query().Get("refresh") == "1"))
	})

	mux.HandleFunc("/api/auth/status", func(w http.ResponseWriter, r *http.Request) {
		auth := currentAuthContext(r, root)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"authenticated":  auth.Role == "operator",
			"role":           auth.Role,
			"writeProtected": true,
			"passkeys":       passkeyStatus(root),
		})
	})

	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requireSameOriginWrite(w, r, root, "auth.login", "", "viewer") {
			return
		}
		if !reserveLoginAttempt(r) {
			appendAudit(root, auditEntryFromRequest(r, "auth.login", "", "rate_limited", "too many login attempts", "viewer"))
			http.Error(w, "too many login attempts", http.StatusTooManyRequests)
			return
		}
		token, err := readLoginToken(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		session, err := verifyDashboardTokenAndCreateSession(root, token, r)
		if err != nil {
			if delay := recordLoginFailure(r); delay > 0 {
				time.Sleep(delay)
			}
			appendAudit(root, auditEntryFromRequest(r, "auth.login", "", "denied", err.Error(), "viewer"))
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		recordLoginSuccess(r)
		setSessionCookie(w, r, session)
		appendAudit(root, auditEntryFromRequest(r, "auth.login", "", "ok", "", "operator"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "role": "operator"})
	})

	mux.HandleFunc("/api/auth/passkey/register/options", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth, ok := requireOperator(w, r, root, "auth.passkey.register.options", "")
		if !ok {
			return
		}
		options, err := passkeyRegistrationOptions(root, r)
		if err != nil {
			appendAudit(root, auditEntryFromRequest(r, "auth.passkey.register.options", "", "error", err.Error(), auth.Role))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		appendAudit(root, auditEntryFromRequest(r, "auth.passkey.register.options", "", "ok", "", auth.Role))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(options)
	})

	mux.HandleFunc("/api/auth/passkey/register/verify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth, ok := requireOperator(w, r, root, "auth.passkey.register.verify", "")
		if !ok {
			return
		}
		if err := verifyPasskeyRegistration(root, r); err != nil {
			appendAudit(root, auditEntryFromRequest(r, "auth.passkey.register.verify", "", "error", err.Error(), auth.Role))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		appendAudit(root, auditEntryFromRequest(r, "auth.passkey.register.verify", "", "ok", "", auth.Role))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})

	mux.HandleFunc("/api/auth/passkey/login/options", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requireSameOriginWrite(w, r, root, "auth.passkey.login.options", "", "viewer") {
			return
		}
		options, err := passkeyAuthenticationOptions(root, r)
		if err != nil {
			appendAudit(root, auditEntryFromRequest(r, "auth.passkey.login.options", "", "error", err.Error(), "viewer"))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		appendAudit(root, auditEntryFromRequest(r, "auth.passkey.login.options", "", "ok", "", "viewer"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(options)
	})

	mux.HandleFunc("/api/auth/passkey/login/verify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requireSameOriginWrite(w, r, root, "auth.passkey.login.verify", "", "viewer") {
			return
		}
		if !reserveLoginAttempt(r) {
			appendAudit(root, auditEntryFromRequest(r, "auth.passkey.login.verify", "", "rate_limited", "too many login attempts", "viewer"))
			http.Error(w, "too many login attempts", http.StatusTooManyRequests)
			return
		}
		session, err := verifyPasskeyAuthenticationAndCreateSession(root, r)
		if err != nil {
			if delay := recordLoginFailure(r); delay > 0 {
				time.Sleep(delay)
			}
			appendAudit(root, auditEntryFromRequest(r, "auth.passkey.login.verify", "", "denied", err.Error(), "viewer"))
			http.Error(w, "passkey rejected", http.StatusUnauthorized)
			return
		}
		recordLoginSuccess(r)
		setSessionCookie(w, r, session)
		appendAudit(root, auditEntryFromRequest(r, "auth.passkey.login.verify", "", "ok", "", "operator"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "role": "operator"})
	})

	mux.HandleFunc("/api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		role := currentAuthContext(r, root).Role
		if !requireSameOriginWrite(w, r, root, "auth.logout", "", role) {
			return
		}
		if session := sessionTokenFromRequest(r); session != "" {
			revokeSession(root, session)
		}
		clearSessionCookie(w, r)
		appendAudit(root, auditEntryFromRequest(r, "auth.logout", "", "ok", "", role))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})

	mux.HandleFunc("/api/auth/passkeys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth := currentAuthContext(r, root)
		if auth.Role != "operator" {
			http.Error(w, "operator role required", http.StatusUnauthorized)
			return
		}
		keys, err := listPasskeys(root)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"passkeys": keys})
	})

	mux.HandleFunc("/api/auth/passkeys/rename", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth, ok := requireOperator(w, r, root, "auth.passkey.rename", "")
		if !ok {
			return
		}
		id, label, err := readPasskeyManageRequest(r)
		if err != nil {
			appendAudit(root, auditEntryFromRequest(r, "auth.passkey.rename", "", "error", err.Error(), auth.Role))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := renamePasskey(root, id, label); err != nil {
			appendAudit(root, auditEntryFromRequest(r, "auth.passkey.rename", id, "error", err.Error(), auth.Role))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		appendAudit(root, auditEntryFromRequest(r, "auth.passkey.rename", id, "ok", "", auth.Role))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})

	mux.HandleFunc("/api/auth/passkeys/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth, ok := requireOperator(w, r, root, "auth.passkey.delete", "")
		if !ok {
			return
		}
		id, _, err := readPasskeyManageRequest(r)
		if err != nil {
			appendAudit(root, auditEntryFromRequest(r, "auth.passkey.delete", "", "error", err.Error(), auth.Role))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := deletePasskey(root, id); err != nil {
			appendAudit(root, auditEntryFromRequest(r, "auth.passkey.delete", id, "error", err.Error(), auth.Role))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		appendAudit(root, auditEntryFromRequest(r, "auth.passkey.delete", id, "ok", "", auth.Role))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})

	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		if !requireOperatorRead(w, r, root) {
			return
		}
		resp := map[string]any{
			"agents":      listByExt(filepath.Join(root, "agents"), ".sh"),
			"flavors":     append([]string{"all"}, listByExt(filepath.Join(root, "flavors"), ".containerfile")...),
			"volumes":     append([]string{"all", "none"}, listByExt(filepath.Join(root, "volumes"), ".volumes")...),
			"bases":       []string{"alpine", "trixie-slim"},
			"defaultBase": readDefaultBase(root),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/api/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth, ok := requireOperator(w, r, root, "create", "")
		if !ok {
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad form data: "+err.Error(), http.StatusBadRequest)
			return
		}
		agent := strings.TrimSpace(r.FormValue("agent"))
		instance := strings.TrimSpace(r.FormValue("instance"))
		flavor := strings.TrimSpace(r.FormValue("flavor"))
		volumes := strings.TrimSpace(r.FormValue("volumes"))
		base := strings.TrimSpace(r.FormValue("base"))

		memory := strings.TrimSpace(r.FormValue("memory"))
		cpu := strings.TrimSpace(r.FormValue("cpu"))

		if !validIdent(agent) {
			http.Error(w, "invalid agent", http.StatusBadRequest)
			return
		}
		// Instance is optional (blank = auto-name). Flavor/volumes/base default if blank.
		if instance != "" && !validIdent(instance) {
			http.Error(w, "invalid instance", http.StatusBadRequest)
			return
		}
		if flavor == "" {
			flavor = "all"
		}
		if volumes == "" {
			volumes = "all"
		}
		if base == "" {
			base = "alpine"
		}
		if !validIdent(flavor) || !validIdent(volumes) || !validIdent(base) {
			http.Error(w, "invalid flavor/volumes/base", http.StatusBadRequest)
			return
		}

		// Validate memory if set
		if memory != "" {
			matched, _ := regexp.MatchString("^[0-9]+[gGmMkK]?$", memory)
			if !matched {
				http.Error(w, "invalid memory limit (must be e.g. 512M or 2G)", http.StatusBadRequest)
				return
			}
		}

		// Validate cpu if set
		if cpu != "" {
			matched, _ := regexp.MatchString("^[0-9]+(\\.[0-9]+)?$", cpu)
			if !matched {
				http.Error(w, "invalid cpu limit (must be e.g. 1.5 or 2.0)", http.StatusBadRequest)
				return
			}
		}

		extraFlags := ""
		if memory != "" {
			extraFlags += fmt.Sprintf(" --memory %s", memory)
		}
		if cpu != "" {
			extraFlags += fmt.Sprintf(" --cpu %s", cpu)
		}

		// All inputs already pass validation, so they're safe
		// to embed unquoted. Instance is wrapped in single quotes so a blank value still
		// occupies the positional slot (`pod start agent '' flavor volumes base`).
		shellCmd := fmt.Sprintf("source ~/.pod_agents && pod start %s '%s' %s %s %s%s",
			agent, instance, flavor, volumes, base, extraFlags)
		cmd := exec.Command("bash", "-lc", shellCmd)
		output, err := cmd.CombinedOutput()
		w.Header().Set("Content-Type", "application/json")
		status := http.StatusOK
		result := "ok"
		errText := ""
		body := map[string]any{
			"agent": agent, "instance": instance,
			"flavor": flavor, "volumes": volumes, "base": base,
			"output": stripANSI(string(output)),
		}
		if err != nil {
			status = http.StatusInternalServerError
			result = "error"
			errText = err.Error()
			body["status"] = "error"
			body["error"] = err.Error()
		} else {
			body["status"] = "ok"
		}
		appendAudit(root, auditEntryFromRequest(r, "create", agent+"-"+instance, result, errText, auth.Role))
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	})

	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth, ok := requireOperator(w, r, root, "action", "")
		if !ok {
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad form data: "+err.Error(), http.StatusBadRequest)
			return
		}
		op := strings.TrimSpace(r.FormValue("op"))
		agent := strings.TrimSpace(r.FormValue("agent"))
		instance := strings.TrimSpace(r.FormValue("instance"))

		// Whitelist ops to keep this endpoint safe even though it's LAN-local.
		allowedOps := map[string]bool{
			"start": true, "stop": true, "restart": true, "delete": true, "remove": true,
		}
		if !allowedOps[op] {
			http.Error(w, "op not allowed", http.StatusBadRequest)
			return
		}
		// Strict identifier validation; pod() is invoked via a sourced shell, so only allow safe chars.
		if !validIdent(agent) || !validIdent(instance) {
			http.Error(w, "invalid agent/instance", http.StatusBadRequest)
			return
		}

		cmd := exec.Command("bash", "-lc",
			fmt.Sprintf("source ~/.pod_agents && pod %s %s %s", op, agent, instance))
		output, err := cmd.CombinedOutput()
		if err != nil {
			appendAudit(root, auditEntryFromRequest(r, "action."+op, agent+"-"+instance, "error", err.Error(), auth.Role))
			http.Error(w, fmt.Sprintf("Action failed: %s\n%s", err, string(output)), http.StatusInternalServerError)
			return
		}
		appendAudit(root, auditEntryFromRequest(r, "action."+op, agent+"-"+instance, "ok", "", auth.Role))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
			"op":     op, "agent": agent, "instance": instance,
			"output": stripANSI(string(output)),
		})
	})

	mux.HandleFunc("/api/terminal", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth := currentAuthContext(r, root)
		if auth.Role != "operator" {
			http.Error(w, "operator role required", http.StatusUnauthorized)
			return
		}
		agent := strings.TrimSpace(r.URL.Query().Get("agent"))
		instance := strings.TrimSpace(r.URL.Query().Get("instance"))
		if !validIdent(agent) || !validIdent(instance) {
			http.Error(w, "invalid agent/instance", http.StatusBadRequest)
			return
		}
		snapshot, err := captureTerminal(agent+"-"+instance, 180)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snapshot)
	})

	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth := currentAuthContext(r, root)
		if auth.Role != "operator" {
			http.Error(w, "operator role required", http.StatusUnauthorized)
			return
		}
		agent := strings.TrimSpace(r.URL.Query().Get("agent"))
		instance := strings.TrimSpace(r.URL.Query().Get("instance"))
		if !validIdent(agent) || !validIdent(instance) {
			http.Error(w, "invalid agent/instance", http.StatusBadRequest)
			return
		}
		snapshot, err := capturePodJournal(agent, instance, journalLineLimitFromRequest(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snapshot)
	})

	mux.HandleFunc("/api/batches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requireOperatorRead(w, r, root) {
			return
		}
		batches, err := listBatchSummaries(root)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"batches": batches})
	})

	mux.HandleFunc("/api/batches/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requireOperatorRead(w, r, root) {
			return
		}
		snapshot, err := readBatchLog(root, strings.TrimSpace(r.URL.Query().Get("batch")), strings.TrimSpace(r.URL.Query().Get("target")), batchLineLimitFromRequest(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snapshot)
	})

	mux.HandleFunc("/api/batches/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requireOperatorRead(w, r, root) {
			return
		}
		ids, err := batchExportIDsFromRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateBatchExport(root, ids); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, batchExportFilename(ids)))
		if err := writeBatchExport(w, root, ids); err != nil {
			log.Printf("batch export failed: %v", err)
		}
	})

	mux.HandleFunc("/api/batches/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth, ok := requireOperator(w, r, root, "batch.stop", "")
		if !ok {
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad form data: "+err.Error(), http.StatusBadRequest)
			return
		}
		id := strings.TrimSpace(r.FormValue("batch"))
		if err := stopBatch(root, id); err != nil {
			appendAudit(root, auditEntryFromRequest(r, "batch.stop", id, "error", err.Error(), auth.Role))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		appendAudit(root, auditEntryFromRequest(r, "batch.stop", id, "ok", "", auth.Role))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "batch": id})
	})

	mux.HandleFunc("/api/batches/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth, ok := requireOperator(w, r, root, "batch.delete", "")
		if !ok {
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad form data: "+err.Error(), http.StatusBadRequest)
			return
		}
		idsStr := strings.TrimSpace(r.FormValue("batches"))
		if idsStr == "" {
			http.Error(w, "missing batches parameter", http.StatusBadRequest)
			return
		}
		ids := strings.Split(idsStr, ",")
		var deleted []string
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if !validIdent(id) {
				appendAudit(root, auditEntryFromRequest(r, "batch.delete", id, "error", "invalid batch id", auth.Role))
				http.Error(w, "invalid batch id: "+id, http.StatusBadRequest)
				return
			}
			_ = stopBatch(root, id)
			dir := filepath.Join(batchRoot(root), id)
			if err := os.RemoveAll(dir); err != nil {
				appendAudit(root, auditEntryFromRequest(r, "batch.delete", id, "error", err.Error(), auth.Role))
				http.Error(w, "failed to delete "+id+": "+err.Error(), http.StatusInternalServerError)
				return
			}
			appendAudit(root, auditEntryFromRequest(r, "batch.delete", id, "ok", "", auth.Role))
			deleted = append(deleted, id)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "deleted": deleted})
	})

	mux.HandleFunc("/api/agent-manager/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if !requireOperatorRead(w, r, root) {
				return
			}
			cfg := loadAgentManagerConfig(root)
			envs := readEnvFile(root)

			key := envs["POD_OPENAI_API_KEY"]
			if key == "" {
				key = os.Getenv("OPENAI_API_KEY")
			}
			maskedKey := "Not Set"
			if key != "" {
				if len(key) > 8 {
					maskedKey = key[:4] + "..." + key[len(key)-4:]
				} else {
					maskedKey = "Configured"
				}
			}

			baseURL := envs["POD_OPENAI_BASE_URL"]
			if baseURL == "" {
				baseURL = os.Getenv("OPENAI_BASE_URL")
			}
			if baseURL == "" {
				baseURL = "https://api.openai.com/v1"
			}

			model := envs["POD_DEFAULT_MODEL"]
			if model == "" {
				model = os.Getenv("POD_DEFAULT_MODEL")
			}
			if model == "" {
				model = os.Getenv("DEFAULT_MODEL")
			}
			if model == "" {
				model = "gpt-4o"
			}

			response := map[string]any{
				"enabled":       cfg.Enabled,
				"system_prompt": cfg.SystemPrompt,
				"start_hour":    cfg.StartHour,
				"end_hour":      cfg.EndHour,
				"api_key":       maskedKey,
				"base_url":      baseURL,
				"model":         model,
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
			return
		}

		if r.Method == http.MethodPost {
			auth, ok := requireOperator(w, r, root, "agent-manager.config", "")
			if !ok {
				return
			}

			type AgentManagerConfigRequest struct {
				Enabled      bool   `json:"enabled"`
				SystemPrompt string `json:"system_prompt"`
				StartHour    int    `json:"start_hour"`
				EndHour      int    `json:"end_hour"`
				ApiKey       string `json:"api_key,omitempty"`
				BaseUrl      string `json:"base_url,omitempty"`
				Model        string `json:"model,omitempty"`
			}

			var req AgentManagerConfigRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
				return
			}
			if req.StartHour < 0 || req.StartHour > 23 || req.EndHour < 0 || req.EndHour > 23 {
				http.Error(w, "invalid hours", http.StatusBadRequest)
				return
			}

			cfg := AgentManagerConfig{
				Enabled:      req.Enabled,
				SystemPrompt: req.SystemPrompt,
				StartHour:    req.StartHour,
				EndHour:      req.EndHour,
			}
			if err := saveAgentManagerConfig(root, cfg); err != nil {
				appendAudit(root, auditEntryFromRequest(r, "agent-manager.config", "", "error", err.Error(), auth.Role))
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			// Update .env file if new values are provided and they are not masked
			updates := make(map[string]string)
			if req.BaseUrl != "" {
				updates["POD_OPENAI_BASE_URL"] = req.BaseUrl
			}
			if req.Model != "" {
				updates["POD_DEFAULT_MODEL"] = req.Model
			}
			if req.ApiKey != "" && req.ApiKey != "Not Set" && !strings.Contains(req.ApiKey, "...") && req.ApiKey != "Configured" {
				updates["POD_OPENAI_API_KEY"] = req.ApiKey
			}

			if len(updates) > 0 {
				if err := updateEnvFile(root, updates); err != nil {
					appendAudit(root, auditEntryFromRequest(r, "agent-manager.config.env", "", "error", err.Error(), auth.Role))
					http.Error(w, "failed to update .env: "+err.Error(), http.StatusInternalServerError)
					return
				}
				// Dynamically update the process environment variables immediately
				for k, v := range updates {
					os.Setenv(k, v)
					if k == "POD_OPENAI_API_KEY" {
						os.Setenv("OPENAI_API_KEY", v)
					}
					if k == "POD_OPENAI_BASE_URL" {
						os.Setenv("OPENAI_BASE_URL", v)
					}
					if k == "POD_DEFAULT_MODEL" {
						os.Setenv("DEFAULT_MODEL", v)
					}
				}
			}

			appendAudit(root, auditEntryFromRequest(r, "agent-manager.config", "", "ok", "", auth.Role))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	mux.HandleFunc("/api/agent-manager/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requireOperatorRead(w, r, root) {
			return
		}
		logs, err := getAgentManagerLogs(root)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"logs": logs})
	})

	mux.HandleFunc("/api/terminal/ws", func(w http.ResponseWriter, r *http.Request) {
		handleTerminalWebSocket(w, r, root)
	})

	mux.HandleFunc("/api/logs/ws", func(w http.ResponseWriter, r *http.Request) {
		handleLogWebSocket(w, r, root)
	})

	mux.HandleFunc("/api/ports/ping", func(w http.ResponseWriter, r *http.Request) {
		if !requireOperatorRead(w, r, root) {
			return
		}
		port := strings.TrimSpace(r.URL.Query().Get("port"))
		for _, char := range port {
			if char < '0' || char > '9' {
				http.Error(w, "invalid port", http.StatusBadRequest)
				return
			}
		}
		if port == "" {
			http.Error(w, "missing port", http.StatusBadRequest)
			return
		}
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
		reachable := false
		if err == nil {
			reachable = true
			conn.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"port":      port,
			"reachable": reachable,
		})
	})

	mux.HandleFunc("/api/terminal/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth, ok := requireOperator(w, r, root, "terminal.start", "")
		if !ok {
			return
		}
		agent, instance, err := readTerminalTarget(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := startTerminalSession(agent, instance); err != nil {
			appendAudit(root, auditEntryFromRequest(r, "terminal.start", agent+"-"+instance, "error", err.Error(), auth.Role))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		appendAudit(root, auditEntryFromRequest(r, "terminal.start", agent+"-"+instance, "ok", "", auth.Role))
		snapshot, err := captureTerminal(agent+"-"+instance, 180)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snapshot)
	})

	mux.HandleFunc("/api/terminal/input", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth, ok := requireOperator(w, r, root, "terminal.input", "")
		if !ok {
			return
		}
		agent, instance, input, err := readTerminalInput(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := sendTerminalInput(agent, instance, input); err != nil {
			appendAudit(root, auditEntryFromRequest(r, "terminal.input", agent+"-"+instance, "error", err.Error(), auth.Role))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		appendAudit(root, auditEntryFromRequest(r, "terminal.input", agent+"-"+instance, "ok", "", auth.Role))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/api/inbox", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if !requireOperatorRead(w, r, root) {
				return
			}
			agent := strings.TrimSpace(r.URL.Query().Get("agent"))
			instance := strings.TrimSpace(r.URL.Query().Get("instance"))
			entries, err := readInboxEntries(root, agent, instance)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"entries": entries})
		case http.MethodPost:
			handleInstruct(w, r, root, currentAuthContext(r, root))
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/instruct", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleInstruct(w, r, root, currentAuthContext(r, root))
	})

	mux.HandleFunc("/api/pods/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		agent, instance, ok := parsePodInstructionPath(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		handleInstructForTarget(w, r, root, agent, instance, currentAuthContext(r, root))
	})

	mux.HandleFunc("/v1/models", shamModels)
	mux.HandleFunc("/v1/chat/completions", shamChatCompletions)
	mux.HandleFunc("/v1/completions", shamTextCompletions)
	mux.HandleFunc("/v1/messages", shamAnthropicMessages)

	// --- Notification Center Endpoints ---

	mux.HandleFunc("/api/notifications", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if !requireOperatorRead(w, r, root) {
				return
			}
			list, err := readNotifications(root, 50)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(list)
			return
		}

		if r.Method == http.MethodDelete {
			_, ok := requireOperator(w, r, root, "notifications.dismiss", "")
			if !ok {
				return
			}
			// Optional body: {"id": "..."}
			var body struct {
				ID string `json:"id"`
			}
			// Read body if present
			decoder := json.NewDecoder(r.Body)
			if err := decoder.Decode(&body); err == nil && body.ID != "" {
				if err := dismissNotification(root, body.ID); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				appendAudit(root, auditEntryFromRequest(r, "notifications.dismiss", body.ID, "ok", "", "operator"))
			} else {
				if err := dismissAllNotifications(root); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				appendAudit(root, auditEntryFromRequest(r, "notifications.dismiss_all", "", "ok", "", "operator"))
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	mux.HandleFunc("/api/notifications/stream", func(w http.ResponseWriter, r *http.Request) {
		if !requireOperatorRead(w, r, root) {
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch := make(chan notification, 10)
		notifySubscribers.Store(ch, true)
		defer func() {
			notifySubscribers.Delete(ch)
		}()

		// Send an initial handshake comment to keep the connection alive
		fmt.Fprintf(w, ": ok\n\n")
		flusher.Flush()

		ctx := r.Context()
		for {
			select {
			case n, open := <-ch:
				if !open {
					return
				}
				data, err := json.Marshal(n)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			case <-ctx.Done():
				return
			}
		}
	})

	// --- Pod Metadata Endpoints ---

	mux.HandleFunc("/api/pods/meta", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if !requireOperatorRead(w, r, root) {
				return
			}
			agent := r.URL.Query().Get("agent")
			instance := r.URL.Query().Get("instance")
			if agent == "" || instance == "" {
				http.Error(w, "missing agent or instance", http.StatusBadRequest)
				return
			}
			m := loadPodMeta(agent, instance)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(m)
			return
		}

		if r.Method == http.MethodPost {
			_, ok := requireOperator(w, r, root, "pods.meta.save", "")
			if !ok {
				return
			}
			var body struct {
				Agent    string   `json:"agent"`
				Instance string   `json:"instance"`
				Notes    string   `json:"notes"`
				Tags     []string `json:"tags"`
				Favorite bool     `json:"favorite"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "Bad JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			if body.Agent == "" || body.Instance == "" {
				http.Error(w, "missing agent or instance", http.StatusBadRequest)
				return
			}
			m := podMeta{
				Notes:    body.Notes,
				Tags:     body.Tags,
				Favorite: body.Favorite,
			}
			if err := savePodMeta(body.Agent, body.Instance, m); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			appendAudit(root, auditEntryFromRequest(r, "pods.meta.save", body.Agent+"-"+body.Instance, "ok", "", "operator"))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	port := os.Getenv("POD_SERVER_PORT")
	if port == "" {
		port = "1337"
	}
	addr := "0.0.0.0:" + port

	fmt.Printf("Pod dashboard listening on %s\n", addr)
	for _, ip := range localIPs() {
		fmt.Printf("  http://%s:%s\n", ip, port)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func listByExt(dir, ext string) []string {
	out := []string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ext) {
			continue
		}
		out = append(out, strings.TrimSuffix(name, ext))
	}
	sort.Strings(out)
	return out
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*webSocketConn, error) {
	if !headerHasToken(r.Header, "Connection", "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, fmt.Errorf("websocket upgrade required")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, fmt.Errorf("missing websocket key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("websocket hijack unsupported")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	accept := webSocketAcceptKey(key)
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
	fmt.Fprintf(rw, "Upgrade: websocket\r\n")
	fmt.Fprintf(rw, "Connection: Upgrade\r\n")
	fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n\r\n", accept)
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &webSocketConn{conn: conn, br: rw.Reader}, nil
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func webSocketAcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func (ws *webSocketConn) Close() error {
	return ws.conn.Close()
}

func (ws *webSocketConn) WriteJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ws.WriteFrame(1, b)
}

func (ws *webSocketConn) WriteFrame(opcode byte, payload []byte) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	header := []byte{0x80 | opcode}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 0xffff:
		header = append(header, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		header = append(header, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(len(payload)))
		header = append(header, ext[:]...)
	}
	if _, err := ws.conn.Write(header); err != nil {
		return err
	}
	_, err := ws.conn.Write(payload)
	return err
}

func (ws *webSocketConn) ReadText() ([]byte, error) {
	for {
		opcode, payload, err := ws.ReadFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case 1:
			return payload, nil
		case 8:
			ws.WriteFrame(8, nil)
			return nil, io.EOF
		case 9:
			ws.WriteFrame(10, payload)
		}
	}
}

func (ws *webSocketConn) ReadFrame() (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(ws.br, head[:]); err != nil {
		return 0, nil, err
	}
	opcode := head[0] & 0x0f
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(ws.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(ws.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > 64*1024 {
		return 0, nil, fmt.Errorf("websocket frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(ws.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(ws.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

func readDefaultBase(root string) string {
	paths := []string{filepath.Join(root, ".env")}
	keys := []string{"POD_BASE_IMAGE=", "BASE_IMAGE="}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			for _, key := range keys {
				if !strings.HasPrefix(line, key) {
					continue
				}
				v := strings.TrimPrefix(line, key)
				v = strings.Trim(v, "\"' \t")
				if v != "" {
					return v
				}
			}
		}
	}
	return "alpine"
}

func readPodVersion(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "version.conf"))
	if err != nil {
		return "unknown"
	}
	return parsePodVersion(data)
}

func parsePodVersion(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "POD_AGENTS_VERSION=") {
			continue
		}
		v := strings.TrimPrefix(line, "POD_AGENTS_VERSION=")
		v = strings.Trim(v, "\"' \t")
		if v != "" {
			return v
		}
	}
	return "unknown"
}

type installSource struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
}

type updateRefStatus struct {
	Ref     string `json:"ref"`
	Version string `json:"version"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

type updateStatusResponse struct {
	Version   string            `json:"version"`
	Command   string            `json:"command"`
	Source    installSource     `json:"source"`
	Latest    []updateRefStatus `json:"latest"`
	CheckedAt string            `json:"checked_at"`
}

type updateStatusCacheEntry struct {
	Key       string
	ExpiresAt time.Time
	Response  updateStatusResponse
}

func readInstallSource(root string) installSource {
	source := installSource{Repo: "robvanvolt/pod-agents-manager", Ref: "main"}
	data, err := os.ReadFile(filepath.Join(root, "install-source.conf"))
	if err != nil {
		return source
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "repo":
			if validGitHubRepo(value) {
				source.Repo = value
			}
		case "ref":
			if validInstallRef(value) {
				source.Ref = value
			}
		}
	}
	return source
}

func readCommandName(root string) string {
	data, err := os.ReadFile(filepath.Join(root, ".cmd_name"))
	if err != nil {
		return "pod"
	}
	cmd := strings.TrimSpace(string(data))
	if !validIdent(cmd) {
		return "pod"
	}
	return cmd
}

var githubRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var installRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]{0,127}$`)

func validGitHubRepo(repo string) bool { return githubRepoRe.MatchString(repo) }

func validInstallRef(ref string) bool {
	return installRefRe.MatchString(ref) && !strings.Contains(ref, "..")
}

func readUpdateStatus(root string, refresh bool) updateStatusResponse {
	source := readInstallSource(root)
	localVersion := readPodVersion(root)
	command := readCommandName(root)
	key := strings.Join([]string{source.Repo, source.Ref, localVersion, command}, "|")
	now := time.Now()

	updateStatusMutex.Lock()
	defer updateStatusMutex.Unlock()
	if !refresh && updateStatusCache.Key == key && now.Before(updateStatusCache.ExpiresAt) {
		return updateStatusCache.Response
	}

	response := updateStatusResponse{
		Version:   localVersion,
		Command:   command,
		Source:    source,
		CheckedAt: now.Format(time.RFC3339),
	}
	for _, ref := range []string{"main", "dev"} {
		version, err := fetchRemotePodVersion(source.Repo, ref)
		item := updateRefStatus{Ref: ref, Version: version, State: updateVersionState(localVersion, version)}
		if err != nil {
			item.Version = "unknown"
			item.State = "unknown"
			item.Error = err.Error()
		}
		response.Latest = append(response.Latest, item)
	}
	updateStatusCache = updateStatusCacheEntry{
		Key:       key,
		ExpiresAt: now.Add(updateStatusTTL),
		Response:  response,
	}
	return response
}

func fetchRemotePodVersion(repo, ref string) (string, error) {
	if !validGitHubRepo(repo) || !validInstallRef(ref) {
		return "", fmt.Errorf("invalid repository source")
	}
	target := "https://raw.githubusercontent.com/" + repo + "/" + url.PathEscape(ref) + "/.pod_agents_config/version.conf"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("version lookup returned %s", res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 4096))
	if err != nil {
		return "", err
	}
	version := parsePodVersion(data)
	if version == "unknown" {
		return "", fmt.Errorf("version lookup did not include POD_AGENTS_VERSION")
	}
	return version, nil
}

func updateVersionState(localVersion, remoteVersion string) string {
	if localVersion == "unknown" || remoteVersion == "" || remoteVersion == "unknown" {
		return "unknown"
	}
	if localVersion == remoteVersion {
		return "current"
	}
	return "different"
}

// pod() in .pod_agents emits colored progress messages via `\033[…m` sequences.
// They make the JSON response unreadable in the dashboard's <pre>, so strip them.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

var idlePathLineRe = regexp.MustCompile(`^(~(/[A-Za-z0-9._-]+)*|/[A-Za-z0-9._~@%+=:,/-]+)$`)

func validIdent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func localIPs() []string {
	out := []string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			out = append(out, ip.String())
		}
	}
	return out
}

func updateStatsLoop() {
	for {
		cmd := exec.Command("podman", "stats", "--all", "--no-stream", "--format", "{{json .}}")
		output, err := cmd.Output()
		if err != nil {
			log.Printf("podman stats failed: %v", err)
			cacheMutex.Lock()
			statsCache = []byte("[]")
			cacheMutex.Unlock()
			time.Sleep(3 * time.Second)
			continue
		}

		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		containers := []map[string]any{}
		publishedPorts := readPublishedPorts()
		root := filepath.Join(os.Getenv("HOME"), ".pod_agents_config")
		if len(lines) > 0 && lines[0] != "" {
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				var row map[string]any
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					log.Printf("podman stats json parse failed: %v", err)
					continue
				}
				enrichActivity(row, root)
				enrichPublishedPorts(row, publishedPorts)

				// Fire state transition notifications
				name := firstString(row, "Name", "Container", "ContainerName")
				if state, ok := row["ActivityState"].(string); ok && name != "" {
					lastState, exists := lastPodStates.Load(name)
					if !exists {
						lastPodStates.Store(name, state)
					} else if lastState != state {
						lastPodStates.Store(name, state)
						if state == "idle" && lastState == "running" {
							fireNotification(root, "pod_idle", "Pod Became Idle", fmt.Sprintf("Pod %s is now idle and waiting for input.", name), name)
						}
					}
				}

				// Enrich with workspace metadata
				if agent, instance, ok := splitManagedPodName(root, name); ok {
					meta := loadPodMeta(agent, instance)
					row["Notes"] = meta.Notes
					row["Tags"] = meta.Tags
					row["Favorite"] = meta.Favorite
				} else {
					row["Notes"] = ""
					row["Tags"] = []string{}
					row["Favorite"] = false
				}

				containers = append(containers, row)
			}
		}

		payload, err := json.Marshal(containers)
		if err != nil {
			log.Printf("stats json encode failed: %v", err)
			payload = []byte("[]")
		}

		cacheMutex.Lock()
		statsCache = payload
		cacheMutex.Unlock()
		time.Sleep(3 * time.Second)
	}
}

type publishedPort struct {
	ContainerPort string `json:"containerPort"`
	Protocol      string `json:"protocol"`
	HostIP        string `json:"hostIP"`
	HostPort      string `json:"hostPort"`
}

func readPublishedPorts() map[string][]publishedPort {
	output, err := exec.Command("podman", "port", "--all").Output()
	if err != nil {
		log.Printf("podman port failed: %v", err)
		return map[string][]publishedPort{}
	}
	return parsePublishedPorts(string(output))
}

func parsePublishedPorts(output string) map[string][]publishedPort {
	out := map[string][]publishedPort{}
	currentID := ""
	for _, rawLine := range strings.Split(strings.TrimSpace(output), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		left, right, ok := strings.Cut(line, "->")
		if !ok {
			currentID = line
			continue
		}
		if currentID == "" {
			continue
		}
		containerPort, protocol, ok := strings.Cut(strings.TrimSpace(left), "/")
		if !ok || strings.TrimSpace(containerPort) == "" {
			continue
		}
		hostIP, hostPort := splitHostPortLoose(strings.TrimSpace(right))
		if strings.TrimSpace(hostPort) == "" {
			continue
		}
		out[currentID] = append(out[currentID], publishedPort{
			ContainerPort: strings.TrimSpace(containerPort),
			Protocol:      strings.ToLower(strings.TrimSpace(protocol)),
			HostIP:        strings.TrimSpace(hostIP),
			HostPort:      strings.TrimSpace(hostPort),
		})
	}
	return out
}

func enrichPublishedPorts(row map[string]any, portsByID map[string][]publishedPort) {
	id := firstString(row, "ID", "ContainerID")
	if id == "" {
		row["PublishedPorts"] = []publishedPort{}
		return
	}
	for portID, ports := range portsByID {
		if portID == id || strings.HasPrefix(portID, id) || strings.HasPrefix(id, portID) {
			row["PublishedPorts"] = ports
			return
		}
	}
	row["PublishedPorts"] = []publishedPort{}
}

func enrichActivity(row map[string]any, root string) {
	name := firstString(row, "Name", "Container", "ContainerName")
	if name == "" {
		row["ActivityState"] = "unknown"
		row["ActivityDetail"] = "missing container name"
		return
	}
	agent, instance, ok := splitManagedPodName(root, name)
	if !ok {
		row["ActivityState"] = "unmanaged"
		row["ActivityDetail"] = "not a pod-agents-manager container"
		return
	}
	row["PodAgent"] = agent
	row["PodInstance"] = instance

	cpu := firstPercent(row, "CPU", "CPUPerc")
	state, detail := detectActivity(name, cpu)
	row["ActivityState"] = state
	row["ActivityDetail"] = detail
}

func detectActivity(container string, cpuPct float64) (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), activityProbeTimeout)
	defer cancel()

	script := `if command -v tmux >/dev/null 2>&1 && tmux has-session -t bot 2>/dev/null; then tmux display-message -p -t bot:0.0 "#{pane_current_command}"; printf '\n---POD_CAPTURE---\n'; tmux capture-pane -p -t bot:0.0 -S -30; else printf 'no-session\n'; fi`
	out, err := exec.CommandContext(ctx, "podman", "exec", container, "sh", "-lc", script).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		if cpuPct >= activityBusyCPUPct {
			return "running", fmt.Sprintf("cpu %.2f%%; activity probe timed out", cpuPct)
		}
		return "unknown", "activity probe timed out"
	}
	if err != nil {
		if cpuPct >= activityBusyCPUPct {
			return "running", fmt.Sprintf("cpu %.2f%%; %s", cpuPct, strings.TrimSpace(stripANSI(string(out))))
		}
		return "unknown", strings.TrimSpace(stripANSI(string(out)))
	}

	command, capture := splitActivityProbeOutput(string(out))
	return classifyLowCPUActivityWithStability(command, capture, activityCaptureStable(container, command, capture))
}

func splitActivityProbeOutput(out string) (string, string) {
	parts := strings.SplitN(out, "\n---POD_CAPTURE---\n", 2)
	command := strings.TrimSpace(stripANSI(parts[0]))
	if len(parts) == 1 {
		return command, ""
	}
	return command, stripANSI(parts[1])
}

func classifyLowCPUActivity(command, capture string) (string, string) {
	return classifyLowCPUActivityWithStability(command, capture, true)
}

func classifyLowCPUActivityWithStability(command, capture string, captureStable bool) (string, string) {
	command = strings.TrimSpace(command)
	switch command {
	case "", "no-session":
		return "idle", "no active agent tmux session"
	case "bash", "sh", "ash", "zsh", "fish", "tmux":
		return "idle", "agent pane is waiting at a shell"
	default:
		if captureLooksIdle(capture) {
			if !captureStable {
				return "running", "terminal pane changed: " + command
			}
			return "idle", idleActivityDetail(command, capture)
		}
		return "running", "foreground command: " + command
	}
}

func activityCaptureStable(container, command, capture string) bool {
	fingerprint := tokenHash(strings.TrimSpace(command) + "\n" + strings.TrimSpace(stripANSI(capture)))

	activityProbeMutex.Lock()
	defer activityProbeMutex.Unlock()

	previous, ok := activityProbeCapture[container]
	activityProbeCapture[container] = fingerprint
	return ok && previous == fingerprint
}

func handleTerminalWebSocket(w http.ResponseWriter, r *http.Request, root string) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	auth := currentAuthContext(r, root)
	if auth.Role != "operator" {
		http.Error(w, "operator role required", http.StatusUnauthorized)
		return
	}
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	instance := strings.TrimSpace(r.URL.Query().Get("instance"))
	if !validIdent(agent) || !validIdent(instance) {
		http.Error(w, "invalid agent/instance", http.StatusBadRequest)
		return
	}
	cols, rows := terminalSizeFromRequest(r)

	ws, err := upgradeWebSocket(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer ws.Close()

	// Send a hello status frame immediately after the WS upgrade so the
	// browser-side 2.5s "Terminal is still connecting…" fallback never
	// fires while we run the (variable-latency) podman-exec calls below.
	// captureTerminal + attachTerminalClient each shell out to podman,
	// and on a busy host those can briefly take a couple of seconds —
	// long enough to trigger the fallback even on a healthy session.
	// This first frame clears the client's connectTimer the moment the
	// socket opens; the snapshot/output frames follow at their own pace.
	_ = ws.WriteJSON(terminalWSMessage{Type: "status", Data: "connected, capturing pane…"})

	appendAudit(root, auditEntryFromRequest(r, "terminal.ws", agent+"-"+instance, "ok", "", auth.Role))
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	snapshot, err := captureTerminal(agent+"-"+instance, 180)
	if err != nil {
		ws.WriteJSON(terminalWSMessage{Type: "error", Error: err.Error()})
		return
	}
	ws.WriteJSON(terminalWSMessage{Type: "snapshot", Snapshot: &snapshot})
	if snapshot.Status == "no-session" {
		ws.WriteJSON(terminalWSMessage{Type: "status", Data: "no active agent session"})
		return
	}

	streamDone := make(chan error, 1)
	terminal, err := attachTerminalClient(ctx, agent, instance, cols, rows)
	if err != nil {
		ws.WriteJSON(terminalWSMessage{Type: "error", Error: err.Error()})
		return
	}
	defer terminal.Close()
	go func() {
		streamDone <- terminal.Stream(ws)
	}()

	readDone := make(chan error, 1)
	go func() {
		readDone <- readTerminalSocket(ctx, ws, agent, instance, terminal)
	}()

	select {
	case err := <-readDone:
		cancel()
		if err != nil && err != io.EOF {
			ws.WriteJSON(terminalWSMessage{Type: "error", Error: err.Error()})
		}
	case err := <-streamDone:
		cancel()
		if err != nil && ctx.Err() == nil {
			ws.WriteJSON(terminalWSMessage{Type: "error", Error: err.Error()})
		}
	case <-ctx.Done():
	}
}

func handleLogWebSocket(w http.ResponseWriter, r *http.Request, root string) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	auth := currentAuthContext(r, root)
	if auth.Role != "operator" {
		http.Error(w, "operator role required", http.StatusUnauthorized)
		return
	}
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	instance := strings.TrimSpace(r.URL.Query().Get("instance"))
	if !validIdent(agent) || !validIdent(instance) {
		http.Error(w, "invalid agent/instance", http.StatusBadRequest)
		return
	}

	ws, err := upgradeWebSocket(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer ws.Close()

	_ = ws.WriteJSON(map[string]any{"type": "status", "data": "connected, streaming journal…"})

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	unit := podJournalUnit(agent, instance)
	cmd := exec.CommandContext(ctx, "journalctl", "--user", "-u", unit, "-f", "-n", "100")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		ws.WriteJSON(map[string]any{"type": "error", "error": err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		ws.WriteJSON(map[string]any{"type": "error", "error": err.Error()})
		return
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	go func() {
		for {
			_, err := ws.ReadText()
			if err != nil {
				cancel()
				break
			}
		}
	}()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		msg := map[string]any{
			"type": "line",
			"line": stripANSI(line),
		}
		if err := ws.WriteJSON(msg); err != nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
}

func readTerminalSocket(ctx context.Context, ws *webSocketConn, agent, instance string, terminal *terminalClient) error {
	for {
		payload, err := ws.ReadText()
		if err != nil {
			return err
		}
		var msg terminalWSMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			ws.WriteJSON(terminalWSMessage{Type: "error", Error: "invalid terminal message"})
			continue
		}
		switch msg.Type {
		case "input":
			if err := terminal.WriteString(msg.Data); err != nil {
				ws.WriteJSON(terminalWSMessage{Type: "error", Error: err.Error()})
			}
		case "resize":
			if err := resizeTerminalPane(agent, instance, msg.Cols, msg.Rows); err != nil {
				ws.WriteJSON(terminalWSMessage{Type: "error", Error: err.Error()})
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func attachTerminalClient(ctx context.Context, agent, instance string, cols, rows int) (*terminalClient, error) {
	if err := ensureTerminalAgentActive(ctx, agent, instance); err != nil {
		return nil, err
	}
	if cols > 0 && rows > 0 {
		if err := resizeTerminalPane(agent, instance, cols, rows); err != nil {
			return nil, err
		}
	}

	cmd := exec.CommandContext(ctx,
		"podman", "exec",
		"-i", "-t",
		"-e", "TERM=xterm-256color",
		"-e", "COLORTERM=truecolor",
		"-e", "POD_TERMINAL_COLS="+strconv.Itoa(cols),
		"-e", "POD_TERMINAL_ROWS="+strconv.Itoa(rows),
		agent+"-"+instance,
		"sh", "-lc", terminalAttachScript,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	client := &terminalClient{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
	}
	client.errDone = make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(stderr, 4096))
		client.errDone <- b
	}()
	return client, nil
}

func (c *terminalClient) Stream(ws *webSocketConn) error {
	buf := make([]byte, 4096)
	for {
		n, readErr := c.stdout.Read(buf)
		if n > 0 {
			if err := ws.WriteJSON(terminalWSMessage{Type: "output", Data: string(buf[:n])}); err != nil {
				c.Kill()
				_ = c.Wait()
				return err
			}
		}
		if readErr != nil {
			waitErr := c.Wait()
			if readErr != io.EOF {
				return readErr
			}
			if waitErr != nil {
				msg := strings.TrimSpace(stripANSI(string(<-c.errDone)))
				if msg == "" {
					msg = waitErr.Error()
				}
				return fmt.Errorf("%s", msg)
			}
			return nil
		}
	}
}

func (c *terminalClient) WriteString(data string) error {
	if data == "" {
		return nil
	}
	if len(data) > 4096 {
		return fmt.Errorf("terminal input is too large")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := io.WriteString(c.stdin, data)
	return err
}

func (c *terminalClient) Close() {
	_ = c.stdin.Close()
	c.Kill()
	_ = c.Wait()
}

func (c *terminalClient) Kill() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func (c *terminalClient) Wait() error {
	c.waitOnce.Do(func() {
		c.waitErr = c.cmd.Wait()
	})
	return c.waitErr
}

func captureTerminal(container string, lines int) (terminalSnapshot, error) {
	snapshot := terminalSnapshot{
		Status:     "ok",
		Container:  container,
		CapturedAt: time.Now().Format(time.RFC3339),
	}
	if agent, instance, ok := splitContainerName(container); ok {
		snapshot.Agent = agent
		snapshot.Instance = instance
	}
	if lines <= 0 || lines > 500 {
		lines = 180
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	script := fmt.Sprintf(`if command -v tmux >/dev/null 2>&1 && tmux has-session -t bot 2>/dev/null; then tmux display-message -p -t bot:0.0 "#{pane_current_command}|#{pane_height}|#{pane_width}"; printf '\n---POD_TERMINAL_CAPTURE---\n'; tmux capture-pane -e -p -t bot:0.0 -S -%d; else printf 'no-session||\n---POD_TERMINAL_CAPTURE---\n'; fi`, lines)
	out, err := exec.CommandContext(ctx, "podman", "exec", container, "sh", "-lc", script).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return snapshot, fmt.Errorf("terminal capture timed out")
	}
	if err != nil {
		return snapshot, fmt.Errorf("%s", strings.TrimSpace(stripANSI(string(out))))
	}

	header, capture := splitTerminalProbeOutput(string(out))
	fields := strings.SplitN(header, "|", 3)
	if len(fields) > 0 {
		snapshot.Command = strings.TrimSpace(fields[0])
	}
	if len(fields) > 1 {
		snapshot.Rows = strings.TrimSpace(fields[1])
	}
	if len(fields) > 2 {
		snapshot.Cols = strings.TrimSpace(fields[2])
	}
	if snapshot.Command == "" || snapshot.Command == "no-session" {
		snapshot.Status = "no-session"
	}
	snapshot.Output = trimTerminalOutput(capture)
	return snapshot, nil
}

func capturePodJournal(agent, instance string, lines int) (journalSnapshot, error) {
	if lines <= 0 || lines > 800 {
		lines = 240
	}
	snapshot := journalSnapshot{
		Agent:      agent,
		Instance:   instance,
		Unit:       podJournalUnit(agent, instance),
		Lines:      lines,
		CapturedAt: time.Now().Format(time.RFC3339),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx,
		"journalctl",
		"--user",
		"--unit", snapshot.Unit,
		"--lines", strconv.Itoa(lines),
		"--no-pager",
		"--output", "short-iso",
	).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return snapshot, fmt.Errorf("journal capture timed out")
	}
	if err != nil {
		msg := strings.TrimSpace(stripANSI(string(out)))
		if msg == "" {
			msg = err.Error()
		}
		return snapshot, fmt.Errorf("%s", msg)
	}
	snapshot.Output = strings.TrimSpace(stripANSI(string(out)))
	if snapshot.Output == "" {
		snapshot.Output = "(no journal entries)"
	}
	return snapshot, nil
}

func podJournalUnit(agent, instance string) string {
	return agent + "@" + instance + ".service"
}

func journalLineLimitFromRequest(r *http.Request) int {
	lines, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("lines")))
	if lines <= 0 || lines > 800 {
		return 240
	}
	return lines
}

func batchRoot(root string) string {
	return filepath.Join(root, "batch")
}

func listBatchSummaries(root string) ([]batchSummary, error) {
	entries, err := os.ReadDir(batchRoot(root))
	if err != nil {
		if os.IsNotExist(err) {
			return []batchSummary{}, nil
		}
		return nil, err
	}

	batches := []batchSummary{}
	for _, entry := range entries {
		if !entry.IsDir() || !validIdent(entry.Name()) {
			continue
		}
		summary, err := readBatchSummary(root, entry.Name())
		if err != nil {
			continue
		}
		batches = append(batches, summary)
	}
	sort.Slice(batches, func(i, j int) bool {
		if batches[i].StartedEpoch != batches[j].StartedEpoch {
			return batches[i].StartedEpoch > batches[j].StartedEpoch
		}
		return batches[i].ID > batches[j].ID
	})
	if len(batches) > 40 {
		batches = batches[:40]
	}
	return batches, nil
}

func readBatchSummary(root, id string) (batchSummary, error) {
	summary := batchSummary{ID: id, Status: "interrupted", Targets: []batchTargetSummary{}}
	if !validIdent(id) {
		return summary, fmt.Errorf("invalid batch")
	}
	dir := filepath.Join(batchRoot(root), id)
	info, err := os.Stat(dir)
	if err != nil {
		return summary, err
	}
	if !info.IsDir() {
		return summary, fmt.Errorf("batch not found")
	}

	meta := readBatchMeta(filepath.Join(dir, "meta.conf"))
	summary.StartedAt = meta["started"]
	summary.StartedEpoch = int64Value(meta["started_epoch"])
	if summary.StartedEpoch == 0 {
		if metaInfo, err := os.Stat(filepath.Join(dir, "meta.conf")); err == nil {
			summary.StartedEpoch = metaInfo.ModTime().Unix()
		}
	}
	summary.Concurrent = meta["concurrent"] == "1"
	summary.Source = meta["source"]
	summary.Version = meta["pod_manager_version"]

	targetNames := strings.Fields(meta["targets"])
	if len(targetNames) == 0 {
		matches, _ := filepath.Glob(filepath.Join(dir, "progress", "*.prog"))
		for _, path := range matches {
			targetNames = append(targetNames, strings.TrimSuffix(filepath.Base(path), ".prog"))
		}
	}
	sort.Strings(targetNames)

	stopped := fileExists(filepath.Join(dir, ".stop"))
	runningTargets := 0
	doneTargets := 0
	for _, target := range targetNames {
		if !validIdent(target) {
			continue
		}
		current, total := readBatchProgress(filepath.Join(dir, "progress", target+".prog"))
		if total == 0 {
			total = int(int64Value(meta["total"]))
		}
		status := batchTargetStatus(dir, target, stopped)
		if status == "running" {
			runningTargets++
		}
		if status == "done" {
			doneTargets++
		}
		targetSummary := batchTargetSummary{
			Target:  target,
			Current: current,
			Total:   total,
			Status:  status,
		}
		if agent, instance, ok := splitContainerName(target); ok {
			targetSummary.Agent = agent
			targetSummary.Instance = instance
		}
		if total > 0 {
			targetSummary.Percent = current * 100 / total
		}
		targetSummary.Results = readBatchResultSummary(filepath.Join(dir, "logs", target+".results.jsonl"))
		summary.Targets = append(summary.Targets, targetSummary)
		summary.Current += current
		summary.Total += total
		summary.Results.Processed += targetSummary.Results.Processed
		summary.Results.Failed += targetSummary.Results.Failed
		summary.Results.DurationSeconds += targetSummary.Results.DurationSeconds
	}
	summary.Results.setAverage()

	switch {
	case stopped:
		summary.Status = "stopped"
	case len(summary.Targets) > 0 && doneTargets == len(summary.Targets):
		summary.Status = "done"
	case runningTargets > 0:
		summary.Status = "running"
		summary.ETASeconds = batchETA(summary.StartedEpoch, summary.Current, summary.Total)
	}
	return summary, nil
}

func readBatchMeta(path string) map[string]string {
	meta := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return meta
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key == "" {
			continue
		}
		meta[key] = value
	}
	return meta
}

func readBatchProgress(path string) (int, int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	left, right, ok := strings.Cut(strings.TrimSpace(string(data)), "/")
	if !ok {
		return 0, 0
	}
	current, _ := strconv.Atoi(strings.TrimSpace(left))
	total, _ := strconv.Atoi(strings.TrimSpace(right))
	if current < 0 {
		current = 0
	}
	if total < 0 {
		total = 0
	}
	return current, total
}

func readBatchResultSummary(path string) batchResultSummary {
	file, err := os.Open(path)
	if err != nil {
		return batchResultSummary{}
	}
	defer file.Close()

	summary := batchResultSummary{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var result struct {
			ExitCode        int   `json:"exit_code"`
			DurationSeconds int64 `json:"duration_s"`
		}
		if err := json.Unmarshal([]byte(scanner.Text()), &result); err != nil {
			continue
		}
		summary.Processed++
		if result.ExitCode != 0 {
			summary.Failed++
		}
		if result.DurationSeconds > 0 {
			summary.DurationSeconds += result.DurationSeconds
		}
	}
	summary.setAverage()
	return summary
}

func (summary *batchResultSummary) setAverage() {
	if summary.Processed > 0 {
		summary.AverageSeconds = float64(summary.DurationSeconds) / float64(summary.Processed)
	}
}

func batchTargetStatus(dir, target string, stopped bool) string {
	if fileExists(filepath.Join(dir, "done."+target)) {
		return "done"
	}
	if stopped {
		return "stopped"
	}
	pid := readPID(filepath.Join(dir, "runner-"+target+".pid"))
	if pid > 0 && pidAlive(pid) {
		return "running"
	}
	return "interrupted"
}

func batchETA(startedEpoch int64, current, total int) int64 {
	if startedEpoch <= 0 || current <= 0 || total <= current {
		return 0
	}
	elapsed := time.Now().Unix() - startedEpoch
	if elapsed <= 0 {
		return 0
	}
	return int64(total-current) * elapsed / int64(current)
}

func batchLineLimitFromRequest(r *http.Request) int {
	lines, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("lines")))
	if lines <= 0 || lines > 1200 {
		return 320
	}
	return lines
}

func batchExportIDsFromRequest(r *http.Request) ([]string, error) {
	raw := append([]string{}, r.URL.Query()["batch"]...)
	if batches := strings.TrimSpace(r.URL.Query().Get("batches")); batches != "" {
		raw = append(raw, strings.Split(batches, ",")...)
	}
	seen := map[string]bool{}
	ids := []string{}
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if !validIdent(id) {
			return nil, fmt.Errorf("invalid batch")
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("select at least one batch")
	}
	if len(ids) > 40 {
		return nil, fmt.Errorf("too many batches selected")
	}
	return ids, nil
}

func batchExportFilename(ids []string) string {
	if len(ids) == 1 {
		return "pod-batch-" + ids[0] + ".zip"
	}
	return fmt.Sprintf("pod-batches-%d.zip", len(ids))
}

func validateBatchExport(root string, ids []string) error {
	for _, id := range ids {
		info, err := os.Stat(filepath.Join(batchRoot(root), id))
		if err != nil || !info.IsDir() {
			return fmt.Errorf("batch not found: %s", id)
		}
	}
	return nil
}

func writeBatchExport(w io.Writer, root string, ids []string) error {
	if err := validateBatchExport(root, ids); err != nil {
		return err
	}
	archive := zip.NewWriter(w)
	for _, id := range ids {
		dir := filepath.Join(batchRoot(root), id)
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !info.Mode().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(batchRoot(root), path)
			if err != nil {
				return err
			}
			header, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			header.Name = filepath.ToSlash(filepath.Join("batch", rel))
			header.Method = zip.Deflate
			entry, err := archive.CreateHeader(header)
			if err != nil {
				return err
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(entry, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		})
		if err != nil {
			_ = archive.Close()
			return err
		}
	}
	return archive.Close()
}

func readBatchLog(root, id, target string, lines int) (batchLogSnapshot, error) {
	snapshot := batchLogSnapshot{
		BatchID:    id,
		Target:     target,
		Lines:      lines,
		CapturedAt: time.Now().Format(time.RFC3339),
	}
	if !validIdent(id) || !validIdent(target) {
		return snapshot, fmt.Errorf("invalid batch log target")
	}
	dir := filepath.Join(batchRoot(root), id)
	if !fileExists(dir) {
		return snapshot, fmt.Errorf("batch not found")
	}
	path := filepath.Join(dir, "logs", target+".log")
	output, err := tailTextFile(path, lines, 512*1024)
	if err != nil {
		if os.IsNotExist(err) {
			return snapshot, fmt.Errorf("batch log not found")
		}
		return snapshot, err
	}
	snapshot.Output = output
	return snapshot, nil
}

func tailTextFile(path string, maxLines int, maxBytes int64) (string, error) {
	if maxLines <= 0 {
		maxLines = 320
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return "", err
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes))
	if err != nil {
		return "", err
	}
	text := strings.ReplaceAll(stripANSI(string(data)), "\r\n", "\n")
	if start > 0 {
		if newline := strings.Index(text, "\n"); newline >= 0 {
			text = text[newline+1:]
		}
	}
	raw := strings.Split(strings.TrimSpace(text), "\n")
	if len(raw) > maxLines {
		raw = raw[len(raw)-maxLines:]
	}
	output := strings.TrimSpace(strings.Join(raw, "\n"))
	if output == "" {
		output = "(no batch log output)"
	}
	return output, nil
}

func stopBatch(root, id string) error {
	if !validIdent(id) {
		return fmt.Errorf("invalid batch")
	}
	dir := filepath.Join(batchRoot(root), id)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("batch not found")
	}
	if err := os.WriteFile(filepath.Join(dir, ".stop"), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return err
	}

	summary, err := readBatchSummary(root, id)
	if err != nil {
		return err
	}
	for _, target := range summary.Targets {
		pid := readPID(filepath.Join(dir, "runner-"+target.Target+".pid"))
		if pid > 0 {
			killBatchRunner(pid)
		}
		_ = exec.Command("pkill", "-f", "podman exec.*"+regexp.QuoteMeta(target.Target)).Run()
	}
	return nil
}

func readPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

func pidAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func killBatchRunner(pid int) {
	_ = exec.Command("pkill", "-TERM", "-P", strconv.Itoa(pid)).Run()
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Signal(syscall.SIGTERM)
	}
}

func int64Value(value string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return n
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func startTerminalSession(agent, instance string) error {
	container := agent + "-" + instance
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx,
		"podman", "exec",
		"-e", "TERM=xterm-256color",
		"-e", "COLORTERM=truecolor",
		"-e", "POD_AGENT="+agent,
		"-e", "OPENAI_BASE_URL="+os.Getenv("OPENAI_BASE_URL"),
		"-e", "OPENAI_API_BASE="+os.Getenv("OPENAI_BASE_URL"),
		"-e", "OPENAI_API_KEY="+os.Getenv("OPENAI_API_KEY"),
		"-e", "DEFAULT_MODEL="+os.Getenv("DEFAULT_MODEL"),
		"-e", "POD_DEFAULT_MODEL="+os.Getenv("POD_DEFAULT_MODEL"),
		container,
		"bash", "-lc", `if tmux has-session -t bot 2>/dev/null; then exit 0; fi; tmux new-session -d -s bot "$POD_AGENT"`,
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("terminal start timed out")
	}
	if err != nil {
		msg := strings.TrimSpace(stripANSI(string(out)))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func sendTerminalInput(agent, instance, input string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := ensureTerminalAgentActive(ctx, agent, instance); err != nil {
		return err
	}
	if err := sendTmuxLiteral(ctx, agent+"-"+instance, input); err != nil {
		return err
	}
	return sendTmuxKey(ctx, agent+"-"+instance, "Enter")
}

func sendTerminalData(agent, instance, data string) error {
	if data == "" {
		return nil
	}
	if len(data) > 4096 {
		return fmt.Errorf("terminal input is too large")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := ensureTerminalAgentActive(ctx, agent, instance); err != nil {
		return err
	}
	container := agent + "-" + instance
	for _, event := range terminalKeyEvents(data) {
		var err error
		if event.Literal != "" {
			err = sendTmuxLiteral(ctx, container, event.Literal)
		} else {
			err = sendTmuxKey(ctx, container, event.Key)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func ensureTerminalAgentActive(ctx context.Context, agent, instance string) error {
	cmd := exec.CommandContext(ctx,
		"podman", "exec",
		agent+"-"+instance,
		"bash", "-lc", `if ! tmux has-session -t bot 2>/dev/null; then echo "agent session is not running"; exit 3; fi`,
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("terminal input timed out")
	}
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(stripANSI(string(out)))
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("%s", msg)
}

func sendTmuxLiteral(ctx context.Context, container, literal string) error {
	if literal == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "podman", "exec", container, "tmux", "send-keys", "-t", "bot:0.0", "-l", "--", literal)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("terminal input timed out")
	}
	if err != nil {
		return fmt.Errorf("%s", terminalCommandError(out, err))
	}
	return nil
}

func sendTmuxKey(ctx context.Context, container, key string) error {
	if key == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "podman", "exec", container, "tmux", "send-keys", "-t", "bot:0.0", key)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("terminal input timed out")
	}
	if err != nil {
		return fmt.Errorf("%s", terminalCommandError(out, err))
	}
	return nil
}

func resizeTerminalPane(agent, instance string, cols, rows int) error {
	if cols < 20 || cols > 300 || rows < 5 || rows > 120 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx,
		"podman", "exec",
		"-e", "POD_TERMINAL_COLS="+strconv.Itoa(cols),
		"-e", "POD_TERMINAL_ROWS="+strconv.Itoa(rows),
		agent+"-"+instance,
		"sh", "-lc", `tmux resize-window -t bot -x "$POD_TERMINAL_COLS" -y "$POD_TERMINAL_ROWS" 2>/dev/null || tmux resize-pane -t bot:0.0 -x "$POD_TERMINAL_COLS" -y "$POD_TERMINAL_ROWS"`,
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("terminal resize timed out")
	}
	if err != nil {
		return fmt.Errorf("%s", terminalCommandError(out, err))
	}
	return nil
}

func terminalSizeFromRequest(r *http.Request) (int, int) {
	cols, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("cols")))
	rows, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("rows")))
	if cols < 20 || cols > 300 || rows < 5 || rows > 120 {
		return 0, 0
	}
	return cols, rows
}

func terminalCommandError(out []byte, err error) string {
	msg := strings.TrimSpace(stripANSI(string(out)))
	if msg == "" {
		msg = err.Error()
	}
	return msg
}

func readTerminalTarget(r *http.Request) (string, string, error) {
	if err := r.ParseForm(); err != nil {
		return "", "", fmt.Errorf("bad form data")
	}
	agent := strings.TrimSpace(r.FormValue("agent"))
	instance := strings.TrimSpace(r.FormValue("instance"))
	if !validIdent(agent) || !validIdent(instance) {
		return "", "", fmt.Errorf("invalid agent/instance")
	}
	return agent, instance, nil
}

func readTerminalInput(r *http.Request) (string, string, string, error) {
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Agent    string `json:"agent"`
			Instance string `json:"instance"`
			Input    string `json:"input"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil {
			return "", "", "", fmt.Errorf("invalid JSON body")
		}
		body.Agent = strings.TrimSpace(body.Agent)
		body.Instance = strings.TrimSpace(body.Instance)
		if !validIdent(body.Agent) || !validIdent(body.Instance) {
			return "", "", "", fmt.Errorf("invalid agent/instance")
		}
		input := normalizeTerminalInput(body.Input)
		if input == "" {
			return "", "", "", fmt.Errorf("input is required")
		}
		return body.Agent, body.Instance, input, nil
	}
	agent, instance, err := readTerminalTarget(r)
	if err != nil {
		return "", "", "", err
	}
	input := normalizeTerminalInput(r.FormValue("input"))
	if input == "" {
		return "", "", "", fmt.Errorf("input is required")
	}
	return agent, instance, input, nil
}

func normalizeTerminalInput(input string) string {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.ReplaceAll(input, "\r", "\n")
	input = strings.Trim(input, "\n")
	if len(input) > 8000 {
		input = input[:8000]
	}
	return input
}

func terminalKeyEvents(data string) []terminalKeyEvent {
	var events []terminalKeyEvent
	var literal strings.Builder
	flushLiteral := func() {
		if literal.Len() == 0 {
			return
		}
		events = append(events, terminalKeyEvent{Literal: literal.String()})
		literal.Reset()
	}
	for i := 0; i < len(data); {
		switch {
		case strings.HasPrefix(data[i:], "\x1b[A"):
			flushLiteral()
			events = append(events, terminalKeyEvent{Key: "Up"})
			i += 3
		case strings.HasPrefix(data[i:], "\x1b[B"):
			flushLiteral()
			events = append(events, terminalKeyEvent{Key: "Down"})
			i += 3
		case strings.HasPrefix(data[i:], "\x1b[C"):
			flushLiteral()
			events = append(events, terminalKeyEvent{Key: "Right"})
			i += 3
		case strings.HasPrefix(data[i:], "\x1b[D"):
			flushLiteral()
			events = append(events, terminalKeyEvent{Key: "Left"})
			i += 3
		case strings.HasPrefix(data[i:], "\x1b[3~"):
			flushLiteral()
			events = append(events, terminalKeyEvent{Key: "Delete"})
			i += 4
		case data[i] == '\r' || data[i] == '\n':
			flushLiteral()
			events = append(events, terminalKeyEvent{Key: "Enter"})
			i++
		case data[i] == '\t':
			flushLiteral()
			events = append(events, terminalKeyEvent{Key: "Tab"})
			i++
		case data[i] == 0x7f || data[i] == '\b':
			flushLiteral()
			events = append(events, terminalKeyEvent{Key: "BSpace"})
			i++
		case data[i] == 0x03:
			flushLiteral()
			events = append(events, terminalKeyEvent{Key: "C-c"})
			i++
		case data[i] == 0x04:
			flushLiteral()
			events = append(events, terminalKeyEvent{Key: "C-d"})
			i++
		case data[i] == 0x1b:
			flushLiteral()
			events = append(events, terminalKeyEvent{Key: "Escape"})
			i++
		case data[i] < 0x20:
			i++
		default:
			literal.WriteByte(data[i])
			i++
		}
	}
	flushLiteral()
	return events
}

func splitTerminalProbeOutput(out string) (string, string) {
	parts := strings.SplitN(out, "\n---POD_TERMINAL_CAPTURE---\n", 2)
	header := strings.TrimSpace(stripANSI(parts[0]))
	if len(parts) == 1 {
		return header, ""
	}
	return header, stripANSI(parts[1])
}

func splitContainerName(name string) (string, string, bool) {
	dash := strings.LastIndex(name, "-")
	if dash <= 0 || dash >= len(name)-1 {
		return "", "", false
	}
	agent := name[:dash]
	instance := name[dash+1:]
	if !validIdent(agent) || !validIdent(instance) {
		return "", "", false
	}
	return agent, instance, true
}

func trimTerminalOutput(output string) string {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return "(no terminal output captured)"
	}
	return strings.Join(lines, "\n")
}

func captureLooksIdle(capture string) bool {
	lines := recentNonEmptyLines(capture, 8)
	for _, line := range lines {
		if lineLooksIdlePrompt(line) {
			return true
		}
	}
	return false
}

func lineLooksIdlePrompt(line string) bool {
	line = strings.TrimSpace(strings.Trim(line, "│┃┆┊ "))
	lower := strings.ToLower(line)
	if idlePathLineRe.MatchString(line) {
		return true
	}
	if strings.Contains(lower, "ask your question") ||
		strings.Contains(lower, "type a message") ||
		strings.Contains(lower, "send a message") ||
		strings.Contains(lower, "waiting for input") ||
		strings.Contains(lower, "press enter to continue") {
		return true
	}
	return strings.HasPrefix(line, ">") && len(line) <= 4
}

func idleActivityDetail(command, capture string) string {
	if excerpt := idleHoverExcerpt(capture); excerpt != "" {
		return "waiting for input: " + excerpt
	}
	return "agent pane is waiting for input: " + command
}

func idleHoverExcerpt(capture string) string {
	lines := strings.Split(strings.ReplaceAll(stripANSI(capture), "\r\n", "\n"), "\n")
	prompt := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if lineLooksIdlePrompt(lines[i]) {
			prompt = i
			break
		}
	}
	if prompt < 0 {
		return ""
	}

	excerpt := []string{}
	for i := prompt - 1; i >= 0; i-- {
		line := cleanHoverLine(lines[i])
		if line == "" || terminalDividerLine(line) {
			if len(excerpt) > 0 {
				break
			}
			continue
		}
		excerpt = append([]string{line}, excerpt...)
		if len(excerpt) == 4 {
			break
		}
	}
	text := strings.Join(strings.Fields(strings.Join(excerpt, " ")), " ")
	runes := []rune(text)
	if len(runes) > 320 {
		text = string(runes[:319]) + "..."
	}
	return text
}

func cleanHoverLine(line string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "│┃┆┊ "))
}

func terminalDividerLine(line string) bool {
	line = strings.TrimSpace(line)
	return len([]rune(line)) >= 8 && strings.Trim(line, "-_=─━═ ") == ""
}

func recentNonEmptyLines(s string, max int) []string {
	raw := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	lines := []string{}
	for i := len(raw) - 1; i >= 0 && len(lines) < max; i-- {
		line := strings.TrimSpace(raw[i])
		line = strings.Trim(line, "│┃┆┊ ")
		if line != "" {
			lines = append([]string{line}, lines...)
		}
	}
	return lines
}

func firstString(row map[string]any, keys ...string) string {
	for _, key := range keys {
		v, ok := row[key]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case string:
			if strings.TrimSpace(x) != "" {
				return strings.TrimSpace(x)
			}
		default:
			s := strings.TrimSpace(fmt.Sprint(x))
			if s != "" {
				return s
			}
		}
	}
	return ""
}

func firstPercent(row map[string]any, keys ...string) float64 {
	for _, key := range keys {
		v, ok := row[key]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case float64:
			return x
		case int:
			return float64(x)
		case string:
			s := strings.TrimSpace(strings.TrimSuffix(x, "%"))
			if n, err := strconv.ParseFloat(s, 64); err == nil {
				return n
			}
		default:
			s := strings.TrimSpace(strings.TrimSuffix(fmt.Sprint(x), "%"))
			if n, err := strconv.ParseFloat(s, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

func splitManagedPodName(root, name string) (string, string, bool) {
	agents := listByExt(filepath.Join(root, "agents"), ".sh")
	sort.SliceStable(agents, func(i, j int) bool {
		return len(agents[i]) > len(agents[j])
	})
	for _, agent := range agents {
		prefix := agent + "-"
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		instance := strings.TrimPrefix(name, prefix)
		if validIdent(instance) {
			return agent, instance, true
		}
	}
	return "", "", false
}

type authContext struct {
	Role string
}

type loginRateState struct {
	WindowStart time.Time
	Attempts    int
	Failures    int
	LastSeen    time.Time
}

type terminalSnapshot struct {
	Status     string `json:"status"`
	Agent      string `json:"agent"`
	Instance   string `json:"instance"`
	Container  string `json:"container"`
	Command    string `json:"command"`
	Rows       string `json:"rows,omitempty"`
	Cols       string `json:"cols,omitempty"`
	Output     string `json:"output"`
	CapturedAt string `json:"captured_at"`
}

type journalSnapshot struct {
	Agent      string `json:"agent"`
	Instance   string `json:"instance"`
	Unit       string `json:"unit"`
	Lines      int    `json:"lines"`
	Output     string `json:"output"`
	CapturedAt string `json:"captured_at"`
}

type batchSummary struct {
	ID           string               `json:"id"`
	StartedAt    string               `json:"started_at,omitempty"`
	StartedEpoch int64                `json:"started_epoch,omitempty"`
	Concurrent   bool                 `json:"concurrent"`
	Source       string               `json:"source,omitempty"`
	Version      string               `json:"pod_manager_version,omitempty"`
	Status       string               `json:"status"`
	Current      int                  `json:"current"`
	Total        int                  `json:"total"`
	ETASeconds   int64                `json:"eta_seconds,omitempty"`
	Results      batchResultSummary   `json:"results"`
	Targets      []batchTargetSummary `json:"targets"`
}

type batchTargetSummary struct {
	Target   string             `json:"target"`
	Agent    string             `json:"agent,omitempty"`
	Instance string             `json:"instance,omitempty"`
	Current  int                `json:"current"`
	Total    int                `json:"total"`
	Percent  int                `json:"percent"`
	Status   string             `json:"status"`
	Results  batchResultSummary `json:"results"`
}

type batchResultSummary struct {
	Processed       int     `json:"processed"`
	Failed          int     `json:"failed"`
	DurationSeconds int64   `json:"duration_seconds"`
	AverageSeconds  float64 `json:"average_seconds"`
}

type batchLogSnapshot struct {
	BatchID    string `json:"batch_id"`
	Target     string `json:"target"`
	Lines      int    `json:"lines"`
	Output     string `json:"output"`
	CapturedAt string `json:"captured_at"`
}

type terminalKeyEvent struct {
	Literal string
	Key     string
}

type terminalWSMessage struct {
	Type     string            `json:"type"`
	Data     string            `json:"data,omitempty"`
	Snapshot *terminalSnapshot `json:"snapshot,omitempty"`
	Error    string            `json:"error,omitempty"`
	Cols     int               `json:"cols,omitempty"`
	Rows     int               `json:"rows,omitempty"`
}

type webSocketConn struct {
	conn net.Conn
	br   *bufio.Reader
	mu   sync.Mutex
}

type terminalClient struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.Reader
	errDone  chan []byte
	mu       sync.Mutex
	waitOnce sync.Once
	waitErr  error
}

type authState struct {
	BootstrapTokenSHA256           string              `json:"bootstrap_token_sha256"`
	CreatedAt                      string              `json:"created_at"`
	UpdatedAt                      string              `json:"updated_at"`
	Sessions                       []authSession       `json:"sessions"`
	PasskeyUserID                  string              `json:"passkey_user_id,omitempty"`
	Passkeys                       []passkeyCredential `json:"passkeys,omitempty"`
	PasskeyRegistrationChallenge   passkeyChallenge    `json:"passkey_registration_challenge,omitempty"`
	PasskeyAuthenticationChallenge passkeyChallenge    `json:"passkey_authentication_challenge,omitempty"`
}

type authSession struct {
	TokenSHA256 string `json:"token_sha256"`
	Role        string `json:"role"`
	CreatedAt   string `json:"created_at"`
	ExpiresAt   string `json:"expires_at"`
	RemoteAddr  string `json:"remote_addr,omitempty"`
	UserAgent   string `json:"user_agent,omitempty"`
}

type passkeyCredential struct {
	ID         string   `json:"id"`
	PublicKey  string   `json:"public_key"`
	SignCount  uint32   `json:"sign_count"`
	Transports []string `json:"transports,omitempty"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	Label      string   `json:"label,omitempty"`
}

type passkeySummary struct {
	ID         string   `json:"id"`
	Label      string   `json:"label,omitempty"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	Transports []string `json:"transports,omitempty"`
}

type passkeyChallenge struct {
	Challenge string `json:"challenge,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type webAuthnCredentialResponse struct {
	ID       string `json:"id"`
	RawID    string `json:"rawId"`
	Type     string `json:"type"`
	Response struct {
		ClientDataJSON    string   `json:"clientDataJSON"`
		AttestationObject string   `json:"attestationObject"`
		AuthenticatorData string   `json:"authenticatorData"`
		Signature         string   `json:"signature"`
		Transports        []string `json:"transports"`
	} `json:"response"`
}

type webAuthnClientData struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Origin    string `json:"origin"`
}

type parsedRegistrationAuthData struct {
	CredentialID  []byte
	PublicKeyCOSE []byte
	SignCount     uint32
	Alg           int
}

type cborParser struct {
	data []byte
	pos  int
}

type auditEntry struct {
	Time       string `json:"time"`
	Action     string `json:"action"`
	Target     string `json:"target,omitempty"`
	Result     string `json:"result"`
	Error      string `json:"error,omitempty"`
	Role       string `json:"role"`
	RemoteAddr string `json:"remote_addr,omitempty"`
	UserAgent  string `json:"user_agent,omitempty"`
}

func authFile(root string) string { return filepath.Join(root, "server", "auth.json") }

func auditFile(root string) string { return filepath.Join(root, "server", "audit.jsonl") }

func auditArchivePattern(root string) string {
	return filepath.Join(root, "server", "audit.*.jsonl")
}

func currentAuthContext(r *http.Request, root string) authContext {
	role := "viewer"
	session := sessionTokenFromRequest(r)
	if session == "" {
		return authContext{Role: role}
	}
	state, err := loadAuthState(root)
	if err != nil {
		return authContext{Role: role}
	}
	now := time.Now()
	hash := tokenHash(session)
	for _, s := range state.Sessions {
		expires, err := time.Parse(time.RFC3339, s.ExpiresAt)
		if err != nil || now.After(expires) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(s.TokenSHA256), []byte(hash)) == 1 && s.Role == "operator" {
			role = "operator"
			break
		}
	}
	return authContext{Role: role}
}

func requireOperator(w http.ResponseWriter, r *http.Request, root, action, target string) (authContext, bool) {
	auth := currentAuthContext(r, root)
	if !requireSameOriginWrite(w, r, root, action, target, auth.Role) {
		return auth, false
	}
	if auth.Role == "operator" {
		return auth, true
	}
	appendAudit(root, auditEntryFromRequest(r, action, target, "denied", "operator role required", auth.Role))
	http.Error(w, "operator role required", http.StatusUnauthorized)
	return auth, false
}

func requireOperatorRead(w http.ResponseWriter, r *http.Request, root string) bool {
	if currentAuthContext(r, root).Role == "operator" {
		return true
	}
	http.Error(w, "operator role required", http.StatusUnauthorized)
	return false
}

func requireSameOriginWrite(w http.ResponseWriter, r *http.Request, root, action, target, role string) bool {
	if err := validateWriteOrigin(r); err != nil {
		appendAudit(root, auditEntryFromRequest(r, action, target, "csrf_blocked", err.Error(), role))
		http.Error(w, "cross-origin write blocked", http.StatusForbidden)
		return false
	}
	return true
}

func validateWriteOrigin(r *http.Request) error {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
		if fetchSite == "" || fetchSite == "same-origin" {
			return nil
		}
		return fmt.Errorf("missing Origin with Sec-Fetch-Site=%s", fetchSite)
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid Origin")
	}
	if originAllowedForRequest(u, r) {
		return nil
	}
	return fmt.Errorf("Origin %s does not match Host %s", u.Host, r.Host)
}

func originAllowedForRequest(origin *url.URL, r *http.Request) bool {
	if sameHostPort(origin.Host, r.Host) {
		return true
	}
	originHost, originPort := splitHostPortLoose(origin.Host)
	if originPort == "" {
		originPort = defaultPortForScheme(origin.Scheme)
	}
	_, requestPort := splitHostPortLoose(r.Host)
	if requestPort == "" {
		if r.TLS != nil {
			requestPort = "443"
		} else {
			requestPort = "80"
		}
	}
	if originPort != requestPort {
		return false
	}
	originHost = strings.Trim(strings.ToLower(originHost), "[]")
	for _, allowed := range append(localIPs(), "127.0.0.1", "localhost", "::1") {
		if originHost == strings.Trim(strings.ToLower(allowed), "[]") {
			return true
		}
	}
	if host, err := os.Hostname(); err == nil && host != "" && originHost == strings.ToLower(host) {
		return true
	}
	return false
}

func sameHostPort(a, b string) bool {
	aHost, aPort := splitHostPortLoose(a)
	bHost, bPort := splitHostPortLoose(b)
	return strings.EqualFold(strings.Trim(aHost, "[]"), strings.Trim(bHost, "[]")) && aPort == bPort
}

func splitHostPortLoose(hostport string) (string, string) {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return "", ""
	}
	if host, port, err := net.SplitHostPort(hostport); err == nil {
		return host, port
	}
	if strings.HasPrefix(hostport, "[") && strings.Contains(hostport, "]") {
		end := strings.Index(hostport, "]")
		host := hostport[1:end]
		rest := hostport[end+1:]
		if strings.HasPrefix(rest, ":") {
			return host, strings.TrimPrefix(rest, ":")
		}
		return host, ""
	}
	if strings.Count(hostport, ":") == 1 {
		parts := strings.SplitN(hostport, ":", 2)
		return parts[0], parts[1]
	}
	return hostport, ""
}

func defaultPortForScheme(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return "443"
	default:
		return "80"
	}
}

func reserveLoginAttempt(r *http.Request) bool {
	ip := rateLimitIP(r)
	now := time.Now()

	loginRateMutex.Lock()
	defer loginRateMutex.Unlock()

	state := loginRateAttempts[ip]
	if state == nil || now.Sub(state.WindowStart) >= loginRateWindow {
		state = &loginRateState{WindowStart: now}
		loginRateAttempts[ip] = state
	}
	state.LastSeen = now
	if state.Attempts >= loginRateMaxAttempts {
		return false
	}
	state.Attempts++
	return true
}

func recordLoginFailure(r *http.Request) time.Duration {
	ip := rateLimitIP(r)
	now := time.Now()

	loginRateMutex.Lock()
	defer loginRateMutex.Unlock()

	state := loginRateAttempts[ip]
	if state == nil {
		state = &loginRateState{WindowStart: now}
		loginRateAttempts[ip] = state
	}
	state.Failures++
	state.LastSeen = now
	if state.Failures < 5 {
		return 0
	}
	exp := state.Failures - 5
	if exp > 4 {
		exp = 4
	}
	return time.Duration(1<<exp) * time.Second
}

func recordLoginSuccess(r *http.Request) {
	ip := rateLimitIP(r)
	loginRateMutex.Lock()
	defer loginRateMutex.Unlock()
	delete(loginRateAttempts, ip)
}

func readLoginToken(r *http.Request) (string, error) {
	contentType := r.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			return "", fmt.Errorf("invalid JSON body")
		}
		body.Token = strings.TrimSpace(body.Token)
		if body.Token == "" {
			return "", fmt.Errorf("token is required")
		}
		return body.Token, nil
	}
	if err := r.ParseForm(); err != nil {
		return "", fmt.Errorf("bad form data")
	}
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		return "", fmt.Errorf("token is required")
	}
	return token, nil
}

func verifyDashboardTokenAndCreateSession(root, token string, r *http.Request) (string, error) {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadOrInitAuthState(root)
	if err != nil {
		return "", err
	}
	if err := verifyDashboardToken(state, token); err != nil {
		return "", err
	}
	session, err := randomToken(32)
	if err != nil {
		return "", err
	}
	now := time.Now()
	state.Sessions = compactSessions(state.Sessions, now)
	state.Sessions = append(state.Sessions, authSession{
		TokenSHA256: tokenHash(session),
		Role:        "operator",
		CreatedAt:   now.Format(time.RFC3339),
		ExpiresAt:   now.Add(operatorSessionTTL).Format(time.RFC3339),
		RemoteAddr:  clientIP(r),
		UserAgent:   r.UserAgent(),
	})
	state.UpdatedAt = now.Format(time.RFC3339)
	if err := saveAuthState(root, state); err != nil {
		return "", err
	}
	return session, nil
}

func verifyDashboardToken(state authState, token string) error {
	hash := tokenHash(token)
	configured := false
	if apiKey := strings.TrimSpace(os.Getenv("POD_SERVER_API_KEY")); apiKey != "" {
		configured = true
		if subtle.ConstantTimeCompare([]byte(tokenHash(apiKey)), []byte(hash)) == 1 {
			return nil
		}
	}
	if state.BootstrapTokenSHA256 != "" {
		configured = true
		if subtle.ConstantTimeCompare([]byte(state.BootstrapTokenSHA256), []byte(hash)) == 1 {
			return nil
		}
	}
	if !configured {
		return fmt.Errorf("dashboard API key not configured; set POD_SERVER_API_KEY or run `pod server token rotate`")
	}
	return fmt.Errorf("invalid dashboard token")
}

// Legacy bootstrap-token callers share the dashboard unlock path while rotated
// bootstrap tokens remain accepted for existing installs.
func verifyBootstrapTokenAndCreateSession(root, token string, r *http.Request) (string, error) {
	return verifyDashboardTokenAndCreateSession(root, token, r)
}

func revokeSession(root, session string) {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return
	}
	hash := tokenHash(session)
	kept := []authSession{}
	for _, s := range state.Sessions {
		if subtle.ConstantTimeCompare([]byte(s.TokenSHA256), []byte(hash)) == 1 {
			continue
		}
		kept = append(kept, s)
	}
	state.Sessions = compactSessions(kept, time.Now())
	state.UpdatedAt = time.Now().Format(time.RFC3339)
	_ = saveAuthState(root, state)
}

func compactSessions(sessions []authSession, now time.Time) []authSession {
	out := []authSession{}
	for _, s := range sessions {
		expires, err := time.Parse(time.RFC3339, s.ExpiresAt)
		if err == nil && now.Before(expires) {
			out = append(out, s)
		}
	}
	return out
}

func loadAuthState(root string) (authState, error) {
	var state authState
	data, err := os.ReadFile(authFile(root))
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	return state, nil
}

func loadOrInitAuthState(root string) (authState, error) {
	state, err := loadAuthState(root)
	if err == nil {
		return state, nil
	}
	if !os.IsNotExist(err) {
		return state, err
	}
	now := time.Now().Format(time.RFC3339)
	return authState{
		CreatedAt: now,
		UpdatedAt: now,
		Sessions:  []authSession{},
	}, nil
}

func saveAuthState(root string, state authState) error {
	path := authFile(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func passkeyStatus(root string) map[string]any {
	state, err := loadAuthState(root)
	count := 0
	if err == nil {
		count = len(state.Passkeys)
	}
	return map[string]any{
		"enabled":              count > 0,
		"registered":           count,
		"browserPackage":       "@simplewebauthn/browser",
		"browserVersion":       "13.3.0",
		"serverImplementation": "native-go-webauthn-compatible",
		"secureContext":        "required",
	}
}

func listPasskeys(root string) ([]passkeySummary, error) {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return nil, err
	}
	keys := make([]passkeySummary, 0, len(state.Passkeys))
	for _, key := range state.Passkeys {
		keys = append(keys, passkeySummary{
			ID:         key.ID,
			Label:      key.Label,
			CreatedAt:  key.CreatedAt,
			LastUsedAt: key.LastUsedAt,
			Transports: key.Transports,
		})
	}
	return keys, nil
}

func passkeyRegistrationOptions(root string, r *http.Request) (map[string]any, error) {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return nil, err
	}
	challenge, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	if state.PasskeyUserID == "" {
		state.PasskeyUserID, err = randomToken(16)
		if err != nil {
			return nil, err
		}
	}
	state.PasskeyRegistrationChallenge = passkeyChallenge{
		Challenge: challenge,
		ExpiresAt: time.Now().Add(5 * time.Minute).Format(time.RFC3339),
	}
	state.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := saveAuthState(root, state); err != nil {
		return nil, err
	}

	exclude := []map[string]any{}
	for _, key := range state.Passkeys {
		exclude = append(exclude, map[string]any{
			"id":         key.ID,
			"type":       "public-key",
			"transports": key.Transports,
		})
	}
	userName := "pod operator"
	return map[string]any{
		"challenge": challenge,
		"rp": map[string]any{
			"name": "Pod Agents Manager",
			"id":   webAuthnRPID(r),
		},
		"user": map[string]any{
			"id":          state.PasskeyUserID,
			"name":        userName,
			"displayName": userName,
		},
		"pubKeyCredParams":   []map[string]any{{"type": "public-key", "alg": -7}},
		"timeout":            60000,
		"attestation":        "none",
		"excludeCredentials": exclude,
		"authenticatorSelection": map[string]any{
			"residentKey":      "preferred",
			"userVerification": "preferred",
		},
	}, nil
}

func readPasskeyManageRequest(r *http.Request) (string, string, error) {
	var body struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(contentType, "application/json") {
		if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&body); err != nil {
			return "", "", fmt.Errorf("invalid JSON body")
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return "", "", fmt.Errorf("bad form data")
		}
		body.ID = r.FormValue("id")
		body.Label = r.FormValue("label")
	}

	body.ID = strings.TrimSpace(body.ID)
	body.Label = strings.TrimSpace(body.Label)
	if body.ID == "" {
		return "", "", fmt.Errorf("passkey id is required")
	}
	if len(body.Label) > 80 {
		return "", "", fmt.Errorf("passkey label is too long")
	}
	return body.ID, body.Label, nil
}

func renamePasskey(root, id, label string) error {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return err
	}
	for i := range state.Passkeys {
		if state.Passkeys[i].ID == id {
			state.Passkeys[i].Label = label
			state.UpdatedAt = time.Now().Format(time.RFC3339)
			return saveAuthState(root, state)
		}
	}
	return fmt.Errorf("passkey not found")
}

func deletePasskey(root, id string) error {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return err
	}
	next := state.Passkeys[:0]
	deleted := false
	for _, key := range state.Passkeys {
		if key.ID == id {
			deleted = true
			continue
		}
		next = append(next, key)
	}
	if !deleted {
		return fmt.Errorf("passkey not found")
	}
	state.Passkeys = next
	state.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveAuthState(root, state)
}

func verifyPasskeyRegistration(root string, r *http.Request) error {
	var credential webAuthnCredentialResponse
	if err := json.NewDecoder(io.LimitReader(r.Body, 128*1024)).Decode(&credential); err != nil {
		return fmt.Errorf("invalid JSON body")
	}
	if credential.Type != "public-key" {
		return fmt.Errorf("invalid credential type")
	}
	clientDataJSON, err := base64URLDecode(credential.Response.ClientDataJSON)
	if err != nil {
		return fmt.Errorf("invalid clientDataJSON")
	}
	var clientData webAuthnClientData
	if err := json.Unmarshal(clientDataJSON, &clientData); err != nil {
		return fmt.Errorf("invalid client data")
	}

	authMutex.Lock()
	defer authMutex.Unlock()
	state, err := loadAuthState(root)
	if err != nil {
		return err
	}
	if err := verifyPasskeyChallenge(state.PasskeyRegistrationChallenge, clientData, "webauthn.create", expectedOrigin(r)); err != nil {
		return err
	}

	attestationObject, err := base64URLDecode(credential.Response.AttestationObject)
	if err != nil {
		return fmt.Errorf("invalid attestationObject")
	}
	authData, err := authenticatorDataFromAttestation(attestationObject)
	if err != nil {
		return err
	}
	parsed, err := parseRegistrationAuthData(authData, webAuthnRPID(r))
	if err != nil {
		return err
	}
	if parsed.Alg != -7 {
		return fmt.Errorf("unsupported passkey public key algorithm")
	}
	id := base64.RawURLEncoding.EncodeToString(parsed.CredentialID)
	for _, existing := range state.Passkeys {
		if existing.ID == id {
			return fmt.Errorf("passkey already registered")
		}
	}
	now := time.Now().Format(time.RFC3339)
	transports := credential.Response.Transports
	state.Passkeys = append(state.Passkeys, passkeyCredential{
		ID:         id,
		PublicKey:  base64.RawURLEncoding.EncodeToString(parsed.PublicKeyCOSE),
		SignCount:  parsed.SignCount,
		Transports: transports,
		CreatedAt:  now,
		Label:      "Pod Dashboard passkey",
	})
	state.PasskeyRegistrationChallenge = passkeyChallenge{}
	state.UpdatedAt = now
	return saveAuthState(root, state)
}

func passkeyAuthenticationOptions(root string, r *http.Request) (map[string]any, error) {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return nil, err
	}
	if len(state.Passkeys) == 0 {
		return nil, fmt.Errorf("no passkeys registered")
	}
	challenge, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	state.PasskeyAuthenticationChallenge = passkeyChallenge{
		Challenge: challenge,
		ExpiresAt: time.Now().Add(5 * time.Minute).Format(time.RFC3339),
	}
	state.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := saveAuthState(root, state); err != nil {
		return nil, err
	}

	allow := []map[string]any{}
	for _, key := range state.Passkeys {
		allow = append(allow, map[string]any{
			"id":         key.ID,
			"type":       "public-key",
			"transports": key.Transports,
		})
	}
	return map[string]any{
		"challenge":        challenge,
		"timeout":          60000,
		"rpId":             webAuthnRPID(r),
		"allowCredentials": allow,
		"userVerification": "preferred",
	}, nil
}

func verifyPasskeyAuthenticationAndCreateSession(root string, r *http.Request) (string, error) {
	var credential webAuthnCredentialResponse
	if err := json.NewDecoder(io.LimitReader(r.Body, 128*1024)).Decode(&credential); err != nil {
		return "", fmt.Errorf("invalid JSON body")
	}
	if credential.Type != "public-key" {
		return "", fmt.Errorf("invalid credential type")
	}
	credentialID := credential.RawID
	if credentialID == "" {
		credentialID = credential.ID
	}
	clientDataJSON, err := base64URLDecode(credential.Response.ClientDataJSON)
	if err != nil {
		return "", fmt.Errorf("invalid clientDataJSON")
	}
	var clientData webAuthnClientData
	if err := json.Unmarshal(clientDataJSON, &clientData); err != nil {
		return "", fmt.Errorf("invalid client data")
	}
	authData, err := base64URLDecode(credential.Response.AuthenticatorData)
	if err != nil {
		return "", fmt.Errorf("invalid authenticatorData")
	}
	signature, err := base64URLDecode(credential.Response.Signature)
	if err != nil {
		return "", fmt.Errorf("invalid signature")
	}

	authMutex.Lock()
	defer authMutex.Unlock()
	state, err := loadAuthState(root)
	if err != nil {
		return "", err
	}
	if err := verifyPasskeyChallenge(state.PasskeyAuthenticationChallenge, clientData, "webauthn.get", expectedOrigin(r)); err != nil {
		return "", err
	}
	keyIndex := -1
	for i, key := range state.Passkeys {
		if key.ID == credentialID {
			keyIndex = i
			break
		}
	}
	if keyIndex < 0 {
		return "", fmt.Errorf("unknown passkey")
	}
	signCount, err := verifyAuthenticationData(authData, clientDataJSON, signature, state.Passkeys[keyIndex], webAuthnRPID(r))
	if err != nil {
		return "", err
	}
	if state.Passkeys[keyIndex].SignCount > 0 && signCount > 0 && signCount <= state.Passkeys[keyIndex].SignCount {
		return "", fmt.Errorf("passkey sign count did not increase")
	}

	session, err := randomToken(32)
	if err != nil {
		return "", err
	}
	now := time.Now()
	state.Passkeys[keyIndex].SignCount = signCount
	state.Passkeys[keyIndex].LastUsedAt = now.Format(time.RFC3339)
	state.PasskeyAuthenticationChallenge = passkeyChallenge{}
	state.Sessions = compactSessions(state.Sessions, now)
	state.Sessions = append(state.Sessions, authSession{
		TokenSHA256: tokenHash(session),
		Role:        "operator",
		CreatedAt:   now.Format(time.RFC3339),
		ExpiresAt:   now.Add(operatorSessionTTL).Format(time.RFC3339),
		RemoteAddr:  clientIP(r),
		UserAgent:   r.UserAgent(),
	})
	state.UpdatedAt = now.Format(time.RFC3339)
	if err := saveAuthState(root, state); err != nil {
		return "", err
	}
	return session, nil
}

func verifyPasskeyChallenge(challenge passkeyChallenge, clientData webAuthnClientData, expectedType, origin string) error {
	if challenge.Challenge == "" {
		return fmt.Errorf("passkey challenge not found")
	}
	expires, err := time.Parse(time.RFC3339, challenge.ExpiresAt)
	if err != nil || time.Now().After(expires) {
		return fmt.Errorf("passkey challenge expired")
	}
	if clientData.Type != expectedType {
		return fmt.Errorf("unexpected WebAuthn type")
	}
	if subtle.ConstantTimeCompare([]byte(clientData.Challenge), []byte(challenge.Challenge)) != 1 {
		return fmt.Errorf("passkey challenge mismatch")
	}
	if clientData.Origin != origin {
		return fmt.Errorf("passkey origin mismatch")
	}
	return nil
}

func base64URLDecode(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty base64url value")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err == nil {
		return data, nil
	}
	return base64.URLEncoding.DecodeString(raw)
}

func expectedOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func webAuthnRPID(r *http.Request) string {
	host, _ := splitHostPortLoose(r.Host)
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "" {
		return "localhost"
	}
	return host
}

func authenticatorDataFromAttestation(attestationObject []byte) ([]byte, error) {
	value, _, err := parseCBOR(attestationObject)
	if err != nil {
		return nil, fmt.Errorf("invalid attestation CBOR")
	}
	m, ok := value.(map[any]any)
	if !ok {
		return nil, fmt.Errorf("attestation object is not a map")
	}
	authData, ok := cborMapBytes(m, "authData")
	if !ok || len(authData) == 0 {
		return nil, fmt.Errorf("attestation missing authData")
	}
	return authData, nil
}

func parseRegistrationAuthData(authData []byte, rpID string) (parsedRegistrationAuthData, error) {
	var parsed parsedRegistrationAuthData
	if len(authData) < 55 {
		return parsed, fmt.Errorf("authenticator data is too short")
	}
	if err := verifyAuthDataPrefix(authData, rpID); err != nil {
		return parsed, err
	}
	flags := authData[32]
	if flags&0x01 == 0 {
		return parsed, fmt.Errorf("passkey user presence flag missing")
	}
	if flags&0x40 == 0 {
		return parsed, fmt.Errorf("passkey attested credential data missing")
	}
	parsed.SignCount = binary.BigEndian.Uint32(authData[33:37])

	offset := 37 + 16
	if len(authData) < offset+2 {
		return parsed, fmt.Errorf("attested credential data is truncated")
	}
	credentialIDLen := int(binary.BigEndian.Uint16(authData[offset : offset+2]))
	offset += 2
	if credentialIDLen <= 0 || len(authData) < offset+credentialIDLen {
		return parsed, fmt.Errorf("credential id is truncated")
	}
	parsed.CredentialID = append([]byte(nil), authData[offset:offset+credentialIDLen]...)
	offset += credentialIDLen
	if offset >= len(authData) {
		return parsed, fmt.Errorf("credential public key missing")
	}
	_, read, err := parseCBOR(authData[offset:])
	if err != nil {
		return parsed, fmt.Errorf("invalid credential public key CBOR")
	}
	parsed.PublicKeyCOSE = append([]byte(nil), authData[offset:offset+read]...)
	_, alg, err := parseCOSEPublicKey(parsed.PublicKeyCOSE)
	if err != nil {
		return parsed, err
	}
	parsed.Alg = alg
	return parsed, nil
}

func verifyAuthenticationData(authData, clientDataJSON, signature []byte, key passkeyCredential, rpID string) (uint32, error) {
	if len(authData) < 37 {
		return 0, fmt.Errorf("authenticator data is too short")
	}
	if err := verifyAuthDataPrefix(authData, rpID); err != nil {
		return 0, err
	}
	flags := authData[32]
	if flags&0x01 == 0 {
		return 0, fmt.Errorf("passkey user presence flag missing")
	}
	signCount := binary.BigEndian.Uint32(authData[33:37])

	coseKey, err := base64URLDecode(key.PublicKey)
	if err != nil {
		return 0, fmt.Errorf("stored passkey public key is invalid")
	}
	publicKey, alg, err := parseCOSEPublicKey(coseKey)
	if err != nil {
		return 0, err
	}
	if alg != -7 {
		return 0, fmt.Errorf("unsupported passkey public key algorithm")
	}
	clientHash := sha256.Sum256(clientDataJSON)
	signedData := make([]byte, 0, len(authData)+len(clientHash))
	signedData = append(signedData, authData...)
	signedData = append(signedData, clientHash[:]...)
	digest := sha256.Sum256(signedData)
	if !ecdsa.VerifyASN1(publicKey, digest[:], signature) {
		return 0, fmt.Errorf("passkey signature rejected")
	}
	return signCount, nil
}

func verifyAuthDataPrefix(authData []byte, rpID string) error {
	if len(authData) < 37 {
		return fmt.Errorf("authenticator data is too short")
	}
	rpHash := sha256.Sum256([]byte(rpID))
	if !bytes.Equal(authData[:32], rpHash[:]) {
		return fmt.Errorf("passkey rp id hash mismatch")
	}
	return nil
}

func parseCOSEPublicKey(raw []byte) (*ecdsa.PublicKey, int, error) {
	value, read, err := parseCBOR(raw)
	if err != nil || read != len(raw) {
		return nil, 0, fmt.Errorf("invalid COSE public key")
	}
	m, ok := value.(map[any]any)
	if !ok {
		return nil, 0, fmt.Errorf("COSE public key is not a map")
	}
	keyType, ok := cborMapInt(m, 1)
	if !ok || keyType != 2 {
		return nil, 0, fmt.Errorf("unsupported COSE key type")
	}
	alg, ok := cborMapInt(m, 3)
	if !ok {
		return nil, 0, fmt.Errorf("COSE public key missing algorithm")
	}
	curve, ok := cborMapInt(m, -1)
	if !ok || curve != 1 {
		return nil, 0, fmt.Errorf("unsupported COSE curve")
	}
	xBytes, ok := cborMapBytes(m, int64(-2))
	if !ok {
		return nil, 0, fmt.Errorf("COSE public key missing x coordinate")
	}
	yBytes, ok := cborMapBytes(m, int64(-3))
	if !ok {
		return nil, 0, fmt.Errorf("COSE public key missing y coordinate")
	}
	if len(xBytes) != 32 || len(yBytes) != 32 {
		return nil, 0, fmt.Errorf("invalid P-256 coordinate length")
	}
	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)
	curveImpl := elliptic.P256()
	if !curveImpl.IsOnCurve(x, y) {
		return nil, 0, fmt.Errorf("COSE public key point is not on P-256")
	}
	return &ecdsa.PublicKey{Curve: curveImpl, X: x, Y: y}, int(alg), nil
}

func parseCBOR(data []byte) (any, int, error) {
	parser := cborParser{data: data}
	value, err := parser.parse()
	if err != nil {
		return nil, parser.pos, err
	}
	return value, parser.pos, nil
}

func (p *cborParser) parse() (any, error) {
	if p.pos >= len(p.data) {
		return nil, fmt.Errorf("unexpected end of CBOR")
	}
	header := p.data[p.pos]
	p.pos++
	major := header >> 5
	additional := header & 0x1f
	length, err := p.readLen(additional)
	if err != nil {
		return nil, err
	}

	switch major {
	case 0:
		if length > 1<<63-1 {
			return nil, fmt.Errorf("CBOR integer overflow")
		}
		return int64(length), nil
	case 1:
		if length > 1<<63-1 {
			return nil, fmt.Errorf("CBOR integer overflow")
		}
		return -1 - int64(length), nil
	case 2:
		if length > uint64(len(p.data)-p.pos) {
			return nil, fmt.Errorf("CBOR byte string truncated")
		}
		out := append([]byte(nil), p.data[p.pos:p.pos+int(length)]...)
		p.pos += int(length)
		return out, nil
	case 3:
		if length > uint64(len(p.data)-p.pos) {
			return nil, fmt.Errorf("CBOR string truncated")
		}
		out := string(p.data[p.pos : p.pos+int(length)])
		p.pos += int(length)
		return out, nil
	case 4:
		out := make([]any, 0, length)
		for i := uint64(0); i < length; i++ {
			v, err := p.parse()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case 5:
		out := make(map[any]any, length)
		for i := uint64(0); i < length; i++ {
			key, err := p.parse()
			if err != nil {
				return nil, err
			}
			value, err := p.parse()
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
		return out, nil
	case 7:
		switch additional {
		case 20:
			return false, nil
		case 21:
			return true, nil
		case 22, 23:
			return nil, nil
		default:
			return nil, fmt.Errorf("unsupported CBOR simple value")
		}
	default:
		return nil, fmt.Errorf("unsupported CBOR major type %d", major)
	}
}

func (p *cborParser) readLen(additional byte) (uint64, error) {
	switch {
	case additional < 24:
		return uint64(additional), nil
	case additional == 24:
		if p.pos >= len(p.data) {
			return 0, fmt.Errorf("CBOR length truncated")
		}
		value := p.data[p.pos]
		p.pos++
		return uint64(value), nil
	case additional == 25:
		if p.pos+2 > len(p.data) {
			return 0, fmt.Errorf("CBOR length truncated")
		}
		value := binary.BigEndian.Uint16(p.data[p.pos : p.pos+2])
		p.pos += 2
		return uint64(value), nil
	case additional == 26:
		if p.pos+4 > len(p.data) {
			return 0, fmt.Errorf("CBOR length truncated")
		}
		value := binary.BigEndian.Uint32(p.data[p.pos : p.pos+4])
		p.pos += 4
		return uint64(value), nil
	case additional == 27:
		if p.pos+8 > len(p.data) {
			return 0, fmt.Errorf("CBOR length truncated")
		}
		value := binary.BigEndian.Uint64(p.data[p.pos : p.pos+8])
		p.pos += 8
		return value, nil
	default:
		return 0, fmt.Errorf("unsupported CBOR indefinite length")
	}
}

func cborMapBytes(m map[any]any, key any) ([]byte, bool) {
	value, ok := m[key]
	if !ok {
		return nil, false
	}
	data, ok := value.([]byte)
	return data, ok
}

func cborMapInt(m map[any]any, key int64) (int64, bool) {
	value, ok := m[key]
	if !ok {
		return 0, false
	}
	n, ok := value.(int64)
	return n, ok
}

func randomToken(bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func sessionTokenFromRequest(r *http.Request) string {
	if cookie, err := r.Cookie("pod_session"); err == nil {
		return cookie.Value
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "pod_session",
		Value:    token,
		Path:     "/",
		MaxAge:   int(operatorSessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "pod_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func secureCookie(r *http.Request) bool {
	return r.TLS != nil || os.Getenv("POD_SERVER_FORCE_SECURE_COOKIE") == "1"
}

func appendAudit(root string, entry auditEntry) {
	auditMutex.Lock()
	defer auditMutex.Unlock()

	path := auditFile(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("audit mkdir failed: %v", err)
		return
	}
	if err := rotateAuditIfNeeded(root); err != nil {
		log.Printf("audit rotate failed: %v", err)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		log.Printf("audit marshal failed: %v", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("audit open failed: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		log.Printf("audit write failed: %v", err)
	}
}

func rotateAuditIfNeeded(root string) error {
	maxBytes := auditMaxBytes()
	if maxBytes <= 0 {
		return nil
	}

	path := auditFile(root)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Size() < maxBytes {
		return nil
	}

	archivePath := filepath.Join(filepath.Dir(path), fmt.Sprintf("audit.%s.jsonl", time.Now().UTC().Format("20060102-150405.000000000")))
	if err := os.Rename(path, archivePath); err != nil {
		return err
	}
	_ = os.Chmod(archivePath, 0o600)
	return pruneAuditArchives(root)
}

func pruneAuditArchives(root string) error {
	maxArchives := auditMaxArchives()
	if maxArchives < 0 {
		return nil
	}

	archives, err := filepath.Glob(auditArchivePattern(root))
	if err != nil {
		return err
	}
	sort.Strings(archives)
	for len(archives) > maxArchives {
		if err := os.Remove(archives[0]); err != nil && !os.IsNotExist(err) {
			return err
		}
		archives = archives[1:]
	}
	return nil
}

func auditMaxBytes() int64 {
	return int64Env("POD_SERVER_AUDIT_MAX_BYTES", defaultAuditMaxBytes)
}

func auditMaxArchives() int {
	v := int64Env("POD_SERVER_AUDIT_MAX_ARCHIVES", int64(defaultAuditMaxArchives))
	if v > int64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(v)
}

func int64Env(name string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return value
}

func auditEntryFromRequest(r *http.Request, action, target, result, errText, role string) auditEntry {
	return auditEntry{
		Time:       time.Now().Format(time.RFC3339),
		Action:     action,
		Target:     target,
		Result:     result,
		Error:      errText,
		Role:       role,
		RemoteAddr: clientIP(r),
		UserAgent:  r.UserAgent(),
	}
}

func clientIP(r *http.Request) string {
	forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if forwarded != "" {
		return strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func rateLimitIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

type inboxEntry struct {
	ID        string   `json:"id"`
	CreatedAt string   `json:"created_at"`
	Type      string   `json:"type"`
	Agent     string   `json:"agent"`
	Instance  string   `json:"instance"`
	Source    string   `json:"source"`
	Status    string   `json:"status"`
	Body      string   `json:"body"`
	Options   []string `json:"options"`
}

func parsePodInstructionPath(path string) (string, string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[0] != "api" || parts[1] != "pods" || parts[4] != "instructions" {
		return "", "", false
	}
	if !validIdent(parts[2]) || !validIdent(parts[3]) {
		return "", "", false
	}
	return parts[2], parts[3], true
}

func handleInstruct(w http.ResponseWriter, r *http.Request, root string, auth authContext) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form data: "+err.Error(), http.StatusBadRequest)
		return
	}
	agent := strings.TrimSpace(r.FormValue("agent"))
	instance := strings.TrimSpace(r.FormValue("instance"))
	handleInstructForTarget(w, r, root, agent, instance, auth)
}

func handleInstructForTarget(w http.ResponseWriter, r *http.Request, root, agent, instance string, auth authContext) {
	if !requireSameOriginWrite(w, r, root, "instruct", agent+"-"+instance, auth.Role) {
		return
	}
	if auth.Role != "operator" {
		appendAudit(root, auditEntryFromRequest(r, "instruct", agent+"-"+instance, "denied", "operator role required", auth.Role))
		http.Error(w, "operator role required", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form data: "+err.Error(), http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.FormValue("message"))
	if body == "" {
		body = strings.TrimSpace(r.FormValue("body"))
	}
	if !validIdent(agent) || !validIdent(instance) {
		http.Error(w, "invalid agent/instance", http.StatusBadRequest)
		return
	}
	if body == "" || len(body) > 8000 {
		http.Error(w, "message is required and must be <= 8000 characters", http.StatusBadRequest)
		return
	}
	entry, err := appendInboxEntry(root, "instruction", agent, instance, "dashboard", body, nil)
	if err != nil {
		appendAudit(root, auditEntryFromRequest(r, "instruct", agent+"-"+instance, "error", err.Error(), auth.Role))
		http.Error(w, "failed to queue instruction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	appendAudit(root, auditEntryFromRequest(r, "instruct", agent+"-"+instance, "ok", "", auth.Role))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "entry": entry})
}

func appendInboxEntry(root, typ, agent, instance, source, body string, options []string) (inboxEntry, error) {
	entry := inboxEntry{
		ID:        fmt.Sprintf("%s-%d", time.Now().Format("20060102-150405"), os.Getpid()),
		CreatedAt: time.Now().Format(time.RFC3339),
		Type:      typ,
		Agent:     agent,
		Instance:  instance,
		Source:    source,
		Status:    "pending",
		Body:      body,
		Options:   options,
	}
	dir := filepath.Join(root, "inbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return entry, err
	}
	path := filepath.Join(dir, agent+"-"+instance+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return entry, err
	}
	defer f.Close()
	enc, err := json.Marshal(entry)
	if err != nil {
		return entry, err
	}
	if _, err := f.Write(append(enc, '\n')); err != nil {
		return entry, err
	}
	return entry, nil
}

func readInboxEntries(root, agent, instance string) ([]inboxEntry, error) {
	if agent != "" && !validIdent(agent) {
		return nil, fmt.Errorf("invalid agent")
	}
	if instance != "" && !validIdent(instance) {
		return nil, fmt.Errorf("invalid instance")
	}
	if agent != "" && instance == "" {
		return nil, fmt.Errorf("instance is required when agent is set")
	}

	dir := filepath.Join(root, "inbox")
	paths := []string{}
	if agent != "" {
		paths = append(paths, filepath.Join(dir, agent+"-"+instance+".jsonl"))
	} else {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		sort.Strings(matches)
		paths = matches
	}

	entries := []inboxEntry{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var entry inboxEntry
			if err := json.Unmarshal([]byte(line), &entry); err == nil {
				entries = append(entries, entry)
			}
		}
	}
	return entries, nil
}

// --- Autonomous Agent Manager Daemon ---

type AgentManagerConfig struct {
	Enabled      bool   `json:"enabled"`
	SystemPrompt string `json:"system_prompt"`
	StartHour    int    `json:"start_hour"`
	EndHour      int    `json:"end_hour"`
}

const DefaultAgentManagerSystemPrompt = `You are the Pod Agent Manager, an autonomous coordinator that monitors developer agent containers.
Your job is to read the tmux terminal capture of a container that is currently IDLE and WAITING FOR INPUT, and decide whether it is safe and appropriate to advance it.

Rules:
1. If the agent has paused and is waiting for instructions, and it is clear what the next command should be (e.g. "continue", "yes", "proceed", running a test, or following a prompt suggestion), output the exact single-line command/response to send to the terminal. Do NOT wrap it in quotes or markdown.
2. If the task has completed successfully, if it has failed with a terminal error, or if there is major ambiguity requiring human decision-making, output exactly "[WAIT]" (without quotes).
3. If you decide to send a command, output ONLY that command. Do not add any conversational text, explanations, or quotes.

Tmux Terminal Capture:`

var (
	agentManagerCooldown = make(map[string]time.Time)
	agentManagerMutex    sync.Mutex
)

func readEnvFile(root string) map[string]string {
	envPath := filepath.Join(root, ".env")
	envMap := make(map[string]string)
	content, err := os.ReadFile(envPath)
	if err != nil {
		return envMap
	}

	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			// Unquote if double or single quoted
			if (strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"")) ||
				(strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'")) {
				if len(val) >= 2 {
					val = val[1 : len(val)-1]
				}
			}
			envMap[key] = val
		}
	}
	return envMap
}

func updateEnvFile(root string, updates map[string]string) error {
	envPath := filepath.Join(root, ".env")
	content, err := os.ReadFile(envPath)
	if err != nil {
		// If it doesn't exist, we start fresh
		content = []byte{}
	}

	lines := strings.Split(string(content), "\n")
	replaced := make(map[string]bool)
	var newLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			newLines = append(newLines, line)
			continue
		}

		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			if newVal, exists := updates[key]; exists {
				newLines = append(newLines, fmt.Sprintf("%s=%q", key, newVal))
				replaced[key] = true
				continue
			}
		}
		newLines = append(newLines, line)
	}

	// Add any updates that were not in the original file
	for key, newVal := range updates {
		if !replaced[key] {
			newLines = append(newLines, fmt.Sprintf("%s=%q", key, newVal))
		}
	}

	newContent := strings.Join(newLines, "\n")
	if !strings.HasSuffix(newContent, "\n") {
		newContent += "\n"
	}

	return os.WriteFile(envPath, []byte(newContent), 0644)
}

func loadAgentManagerConfig(root string) AgentManagerConfig {
	path := filepath.Join(root, "agent_manager.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentManagerConfig{
			Enabled:      false,
			SystemPrompt: DefaultAgentManagerSystemPrompt,
			StartHour:    2,
			EndHour:      13,
		}
	}
	var cfg AgentManagerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return AgentManagerConfig{
			Enabled:      false,
			SystemPrompt: DefaultAgentManagerSystemPrompt,
			StartHour:    2,
			EndHour:      13,
		}
	}
	return cfg
}

func saveAgentManagerConfig(root string, cfg AgentManagerConfig) error {
	path := filepath.Join(root, "agent_manager.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func logAgentManagerIntervention(root string, message string) {
	path := filepath.Join(root, "agent_manager.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), message)
	_, _ = f.WriteString(line)
}

func getAgentManagerLogs(root string) (string, error) {
	path := filepath.Join(root, "agent_manager.log")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 1000 {
		lines = lines[len(lines)-1000:]
	}
	return strings.Join(lines, "\n"), nil
}

func queryLLM(apiKey, apiURL, model, systemPrompt, userContent string) (string, error) {
	type chatMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type chatRequest struct {
		Model       string        `json:"model"`
		Messages    []chatMessage `json:"messages"`
		Temperature float64       `json:"temperature"`
	}
	reqPayload := chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature: 0.1,
	}
	jsonData, err := json.Marshal(reqPayload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	type chatChoice struct {
		Message chatMessage `json:"message"`
	}
	type chatResponse struct {
		Choices []chatChoice `json:"choices"`
	}

	var respPayload chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&respPayload); err != nil {
		return "", err
	}

	if len(respPayload.Choices) == 0 {
		return "", fmt.Errorf("no completion choice returned")
	}

	return strings.TrimSpace(respPayload.Choices[0].Message.Content), nil
}

func startAgentManagerLoop(root string) {
	// Give statsCache a few seconds to initialize
	time.Sleep(5 * time.Second)

	for {
		time.Sleep(15 * time.Second)

		cfg := loadAgentManagerConfig(root)
		if !cfg.Enabled {
			continue
		}

		localHour := time.Now().Hour()
		if !isHourInWindow(localHour, cfg.StartHour, cfg.EndHour) {
			continue
		}

		cacheMutex.RLock()
		var containers []map[string]any
		_ = json.Unmarshal(statsCache, &containers)
		cacheMutex.RUnlock()

		for _, c := range containers {
			name := firstString(c, "Name", "Container", "ContainerName")
			agent, instance, ok := splitManagedPodName(root, name)
			if !ok {
				continue
			}

			state := firstString(c, "ActivityState")
			if state != "idle" {
				continue
			}

			key := agent + "-" + instance
			agentManagerMutex.Lock()
			cooldownUntil, hasCooldown := agentManagerCooldown[key]
			agentManagerMutex.Unlock()

			if hasCooldown && time.Now().Before(cooldownUntil) {
				continue
			}

			// Capture tmux output
			capture, err := getPodTmuxCapture(name)
			if err != nil {
				logAgentManagerIntervention(root, fmt.Sprintf("Error capturing tmux for %s: %v", key, err))
				continue
			}
			capture = strings.TrimSpace(capture)
			if capture == "" || capture == "no-session" {
				continue
			}

			// LLM details
			apiKey := os.Getenv("OPENAI_API_KEY")
			baseURL := os.Getenv("OPENAI_BASE_URL")
			model := os.Getenv("POD_DEFAULT_MODEL")
			if model == "" {
				model = os.Getenv("DEFAULT_MODEL")
			}
			if model == "" {
				model = "gpt-4o"
			}

			if apiKey == "" {
				logAgentManagerIntervention(root, fmt.Sprintf("Error auditing %s: OPENAI_API_KEY is not set in environment", key))
				fireNotification(root, "pod_failed", "Agent Manager Error", fmt.Sprintf("OPENAI_API_KEY is not set. Cannot run autonomous steering for %s.", key), key)
				agentManagerMutex.Lock()
				agentManagerCooldown[key] = time.Now().Add(5 * time.Minute)
				agentManagerMutex.Unlock()
				continue
			}

			if baseURL == "" {
				baseURL = "https://api.openai.com/v1"
			}
			apiURL := strings.TrimSuffix(baseURL, "/") + "/chat/completions"

			// Query
			response, err := queryLLM(apiKey, apiURL, model, cfg.SystemPrompt, capture)
			if err != nil {
				logAgentManagerIntervention(root, fmt.Sprintf("Error querying LLM for %s: %v", key, err))
				fireNotification(root, "pod_failed", "LLM Query Failed", fmt.Sprintf("Error querying LLM for %s: %v", key, err), key)
				agentManagerMutex.Lock()
				agentManagerCooldown[key] = time.Now().Add(1 * time.Minute)
				agentManagerMutex.Unlock()
				continue
			}

			response = strings.TrimSpace(response)
			if response == "" {
				continue
			}

			if strings.ToUpper(response) == "[WAIT]" {
				agentManagerMutex.Lock()
				agentManagerCooldown[key] = time.Now().Add(3 * time.Minute) // 3-minute cooldown on wait
				agentManagerMutex.Unlock()
				continue
			}

			// Intervene
			logAgentManagerIntervention(root, fmt.Sprintf("Intervening on %s: sending command: %q", key, response))
			if err := sendTerminalInput(agent, instance, response); err != nil {
				logAgentManagerIntervention(root, fmt.Sprintf("Error sending command to %s: %v", key, err))
				fireNotification(root, "pod_failed", "Intervention Failed", fmt.Sprintf("Failed to send command to %s: %v", key, err), key)
				agentManagerMutex.Lock()
				agentManagerCooldown[key] = time.Now().Add(30 * time.Second)
				agentManagerMutex.Unlock()
			} else {
				fireNotification(root, "agent_intervened", "Agent Manager Intervention", fmt.Sprintf("Sent command to %s: %s", key, response), key)
				agentManagerMutex.Lock()
				agentManagerCooldown[key] = time.Now().Add(45 * time.Second) // 45s to execute
				agentManagerMutex.Unlock()
			}
		}
	}
}

func isHourInWindow(hour, start, end int) bool {
	if start <= end {
		return hour >= start && hour <= end
	}
	return hour >= start || hour <= end
}

func getPodTmuxCapture(container string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	script := `if command -v tmux >/dev/null 2>&1 && tmux has-session -t bot 2>/dev/null; then tmux capture-pane -p -t bot:0.0 -S -50; else printf 'no-session\n'; fi`
	out, err := exec.CommandContext(ctx, "podman", "exec", container, "sh", "-lc", script).CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// --- Notification Center Helpers & Types ---

type notification struct {
	ID        string `json:"id"`
	Type      string `json:"type"` // pod_idle, batch_completed, agent_question, pod_failed, agent_intervened
	Title     string `json:"title"`
	Message   string `json:"message"`
	Pod       string `json:"pod,omitempty"`
	Timestamp string `json:"timestamp"`
	Read      bool   `json:"read"`
}

var (
	notificationMutex sync.Mutex
	notifySubscribers sync.Map // map[chan notification]bool
	lastPodStates     sync.Map // map[string]string (containerName -> state)
)

func notificationFile(root string) string {
	return filepath.Join(root, "server", "notifications.jsonl")
}

func appendNotification(root string, n notification) error {
	notificationMutex.Lock()
	defer notificationMutex.Unlock()

	file := notificationFile(root)
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		return err
	}

	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := json.Marshal(n)
	if err != nil {
		return err
	}

	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func readNotifications(root string, limit int) ([]notification, error) {
	notificationMutex.Lock()
	defer notificationMutex.Unlock()

	file := notificationFile(root)
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return []notification{}, nil
	}

	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var list []notification
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var n notification
		if err := json.Unmarshal(line, &n); err == nil {
			list = append(list, n)
		}
	}

	// Return newest first, capped to limit
	if len(list) == 0 {
		return []notification{}, nil
	}

	// Reverse
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}

	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}

	return list, nil
}

func dismissNotification(root string, id string) error {
	notificationMutex.Lock()
	defer notificationMutex.Unlock()

	file := notificationFile(root)
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return nil
	}

	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	var list []notification
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var n notification
		if err := json.Unmarshal(line, &n); err == nil {
			if n.ID == id {
				n.Read = true
			}
			list = append(list, n)
		}
	}

	// Re-write file
	tmpFile := file + ".tmp"
	tmpF, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer tmpF.Close()

	for _, n := range list {
		data, err := json.Marshal(n)
		if err != nil {
			return err
		}
		if _, err := tmpF.Write(append(data, '\n')); err != nil {
			return err
		}
	}

	tmpF.Close()
	return os.Rename(tmpFile, file)
}

func dismissAllNotifications(root string) error {
	notificationMutex.Lock()
	defer notificationMutex.Unlock()

	file := notificationFile(root)
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return nil
	}

	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	var list []notification
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var n notification
		if err := json.Unmarshal(line, &n); err == nil {
			n.Read = true
			list = append(list, n)
		}
	}

	// Re-write file
	tmpFile := file + ".tmp"
	tmpF, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer tmpF.Close()

	for _, n := range list {
		data, err := json.Marshal(n)
		if err != nil {
			return err
		}
		if _, err := tmpF.Write(append(data, '\n')); err != nil {
			return err
		}
	}

	tmpF.Close()
	return os.Rename(tmpFile, file)
}

func fireNotification(root string, ntype, title, message, pod string) {
	// Generate random hex ID
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		b = []byte(fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	id := hex.EncodeToString(b)

	n := notification{
		ID:        id,
		Type:      ntype,
		Title:     title,
		Message:   message,
		Pod:       pod,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Read:      false,
	}

	if err := appendNotification(root, n); err != nil {
		log.Printf("Error appending notification: %v", err)
	}

	broadcastNotification(n)
}

func broadcastNotification(n notification) {
	notifySubscribers.Range(func(key, val interface{}) bool {
		ch, ok := key.(chan notification)
		if ok {
			select {
			case ch <- n:
			default:
				// Channel blocked, skip
			}
		}
		return true
	})
}

// --- Pod Metadata Helpers & Types ---

type podMeta struct {
	Notes    string   `json:"notes"`
	Tags     []string `json:"tags"`
	Favorite bool     `json:"favorite"`
}

func podMetaPath(agent, instance string) string {
	return filepath.Join(os.Getenv("HOME"), "Developer", agent+"-pods", instance, ".pod_meta.json")
}

func loadPodMeta(agent, instance string) podMeta {
	m := podMeta{Tags: []string{}}
	path := podMetaPath(agent, instance)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return m
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	return m
}

func savePodMeta(agent, instance string, m podMeta) error {
	path := podMetaPath(agent, instance)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
