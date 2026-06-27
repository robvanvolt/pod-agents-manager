package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
