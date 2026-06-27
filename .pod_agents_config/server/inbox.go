package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

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
