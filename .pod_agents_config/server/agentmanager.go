package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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
