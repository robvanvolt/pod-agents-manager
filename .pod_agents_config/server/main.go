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
	activityBusyCPUPct   = 1.0
	activityProbeTimeout = 900 * time.Millisecond
)

func main() {
	// Background loop refreshes podman stats every few seconds
	go updateStatsLoop()

	root := filepath.Join(os.Getenv("HOME"), ".pod_agents_config")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("./static")))

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
		token, err := readLoginToken(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		session, err := verifyBootstrapTokenAndCreateSession(root, token, r)
		if err != nil {
			appendAudit(root, auditEntryFromRequest(r, "auth.login", "", "denied", err.Error(), "viewer"))
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		setSessionCookie(w, session)
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
		if session := sessionTokenFromRequest(r); session != "" {
			revokeSession(root, session)
		}
		clearSessionCookie(w)
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

	script := `if command -v tmux >/dev/null 2>&1 && tmux has-session -t bot 2>/dev/null; then tmux display-message -p -t bot:0.0 "#{pane_current_command}"; else printf 'no-session\n'; fi`
	out, err := exec.CommandContext(ctx, "podman", "exec", container, "sh", "-lc", script).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "unknown", "activity probe timed out"
	}
	if err != nil {
		return "unknown", strings.TrimSpace(stripANSI(string(out)))
	}

	command := strings.TrimSpace(string(out))
	switch command {
	case "", "no-session":
		return "idle", "no active agent tmux session"
	case "bash", "sh", "ash", "zsh", "fish", "tmux":
		return "idle", "agent pane is waiting at a shell"
	default:
		return "running", "foreground command: " + command
	}
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
	if auth.Role == "operator" {
		return auth, true
	}
	appendAudit(root, auditEntryFromRequest(r, action, target, "denied", "operator role required", auth.Role))
	http.Error(w, "operator role required", http.StatusUnauthorized)
	return auth, false
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

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "pod_session",
		Value:    token,
		Path:     "/",
		MaxAge:   24 * 60 * 60,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "pod_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
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
