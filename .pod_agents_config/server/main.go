package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	"time"
)

var (
	statsCache           []byte
	cacheMutex           sync.RWMutex
	authMutex            sync.Mutex
	loginRateMutex       sync.Mutex
	loginRateAttempts    = map[string]*loginRateState{}
	activityBusyCPUPct   = 1.0
	activityProbeTimeout = 900 * time.Millisecond
)

const (
	loginRateWindow      = time.Minute
	loginRateMaxAttempts = 10
)

func main() {
	// Background loop refreshes podman stats every few seconds
	go updateStatsLoop()

	root := filepath.Join(os.Getenv("HOME"), ".pod_agents_config")
	mux := http.NewServeMux()
	mux.Handle("/", noCache(http.FileServer(http.Dir("./static"))))

	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
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

	mux.HandleFunc("/api/auth/status", func(w http.ResponseWriter, r *http.Request) {
		auth := currentAuthContext(r, root)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"authenticated":  auth.Role == "operator",
			"role":           auth.Role,
			"writeProtected": true,
			"passkeys": map[string]any{
				"enabled":        false,
				"browserPackage": "@simplewebauthn/browser",
				"serverPackage":  "@simplewebauthn/server",
				"status":         "planned",
			},
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
		session, err := verifyBootstrapTokenAndCreateSession(root, token, r)
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

	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
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

		// All inputs already pass validIdent (or are blank for instance), so they're safe
		// to embed unquoted. Instance is wrapped in single quotes so a blank value still
		// occupies the positional slot (`pod start agent '' flavor volumes base`).
		shellCmd := fmt.Sprintf("source ~/.pod_agents && pod start %s '%s' %s %s %s",
			agent, instance, flavor, volumes, base)
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
	if cpuPct >= activityBusyCPUPct {
		return "running", fmt.Sprintf("cpu %.2f%%", cpuPct)
	}

	ctx, cancel := context.WithTimeout(context.Background(), activityProbeTimeout)
	defer cancel()

	script := `if command -v tmux >/dev/null 2>&1 && tmux has-session -t bot 2>/dev/null; then tmux display-message -p -t bot:0.0 "#{pane_current_command}"; printf '\n---POD_CAPTURE---\n'; tmux capture-pane -p -t bot:0.0 -S -30; else printf 'no-session\n'; fi`
	out, err := exec.CommandContext(ctx, "podman", "exec", container, "sh", "-lc", script).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "unknown", "activity probe timed out"
	}
	if err != nil {
		return "unknown", strings.TrimSpace(stripANSI(string(out)))
	}

	command, capture := splitActivityProbeOutput(string(out))
	return classifyLowCPUActivity(command, capture)
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
	command = strings.TrimSpace(command)
	switch command {
	case "", "no-session":
		return "idle", "no active agent tmux session"
	case "bash", "sh", "ash", "zsh", "fish", "tmux":
		return "idle", "agent pane is waiting at a shell"
	default:
		if captureLooksIdle(capture) {
			return "idle", "agent pane is waiting for input: " + command
		}
		return "running", "foreground command: " + command
	}
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
		"bash", "-lc", `if tmux has-session -t bot 2>/dev/null; then cmd=$(tmux display-message -p -t bot:0.0 "#{pane_current_command}" 2>/dev/null || true); case "$cmd" in bash|sh|ash|zsh|fish|tmux|"") tmux kill-session -t bot 2>/dev/null || true ;; *) exit 0 ;; esac; fi; tmux new-session -d -s bot "$POD_AGENT"`,
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

	cmd := exec.CommandContext(ctx,
		"podman", "exec",
		"-e", "POD_TERMINAL_INPUT="+input,
		agent+"-"+instance,
		"bash", "-lc", `if ! tmux has-session -t bot 2>/dev/null; then echo "agent session is not running"; exit 3; fi; cmd=$(tmux display-message -p -t bot:0.0 "#{pane_current_command}" 2>/dev/null || true); case "$cmd" in bash|sh|ash|zsh|fish|tmux|"") echo "agent session is not active; start the agent session first"; exit 4 ;; esac; tmux send-keys -t bot:0.0 -- "$POD_TERMINAL_INPUT" Enter`,
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("terminal input timed out")
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
		lower := strings.ToLower(line)
		if idlePathLineRe.MatchString(line) {
			return true
		}
		if strings.Contains(lower, "type a message") ||
			strings.Contains(lower, "send a message") ||
			strings.Contains(lower, "waiting for input") ||
			strings.Contains(lower, "press enter to continue") {
			return true
		}
		if strings.HasPrefix(line, ">") && len(line) <= 4 {
			return true
		}
	}
	return false
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

type authState struct {
	BootstrapTokenSHA256 string        `json:"bootstrap_token_sha256"`
	CreatedAt            string        `json:"created_at"`
	UpdatedAt            string        `json:"updated_at"`
	Sessions             []authSession `json:"sessions"`
}

type authSession struct {
	TokenSHA256 string `json:"token_sha256"`
	Role        string `json:"role"`
	CreatedAt   string `json:"created_at"`
	ExpiresAt   string `json:"expires_at"`
	RemoteAddr  string `json:"remote_addr,omitempty"`
	UserAgent   string `json:"user_agent,omitempty"`
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

func verifyBootstrapTokenAndCreateSession(root, token string, r *http.Request) (string, error) {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return "", err
	}
	if state.BootstrapTokenSHA256 == "" {
		return "", fmt.Errorf("bootstrap token not configured; run `pod server token rotate`")
	}
	if subtle.ConstantTimeCompare([]byte(state.BootstrapTokenSHA256), []byte(tokenHash(token))) != 1 {
		return "", fmt.Errorf("invalid bootstrap token")
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
		ExpiresAt:   now.Add(24 * time.Hour).Format(time.RFC3339),
		RemoteAddr:  clientIP(r),
		UserAgent:   r.UserAgent(),
	})
	state.UpdatedAt = now.Format(time.RFC3339)
	if err := saveAuthState(root, state); err != nil {
		return "", err
	}
	return session, nil
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
		MaxAge:   24 * 60 * 60,
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
	path := auditFile(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("audit mkdir failed: %v", err)
		return
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
