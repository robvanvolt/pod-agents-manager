package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

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

type journalSnapshot struct {
	Agent      string `json:"agent"`
	Instance   string `json:"instance"`
	Unit       string `json:"unit"`
	Lines      int    `json:"lines"`
	Output     string `json:"output"`
	CapturedAt string `json:"captured_at"`
}
