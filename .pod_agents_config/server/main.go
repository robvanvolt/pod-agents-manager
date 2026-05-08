package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	activityBusyCPUPct   = 1.0
	activityProbeTimeout = 900 * time.Millisecond
)

func main() {
	// Background loop refreshes podman stats every few seconds
	go updateStatsLoop()

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
		root := os.Getenv("HOME") + "/.pod_agents_config"
		resp := map[string]any{
			"hostname": hostname,
			"ips":      localIPs(),
			"time":     time.Now().Format(time.RFC3339),
			"version":  readPodVersion(root),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		root := os.Getenv("HOME") + "/.pod_agents_config"
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
		body := map[string]any{
			"agent": agent, "instance": instance,
			"flavor": flavor, "volumes": volumes, "base": base,
			"output": stripANSI(string(output)),
		}
		if err != nil {
			status = http.StatusInternalServerError
			body["status"] = "error"
			body["error"] = err.Error()
		} else {
			body["status"] = "ok"
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	})

	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
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
			http.Error(w, fmt.Sprintf("Action failed: %s\n%s", err, string(output)), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
			"op":     op, "agent": agent, "instance": instance,
			"output": stripANSI(string(output)),
		})
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
