package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

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

type terminalClient struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.Reader
	errDone  chan []byte
	mu       sync.Mutex
	waitOnce sync.Once
	waitErr  error
}
