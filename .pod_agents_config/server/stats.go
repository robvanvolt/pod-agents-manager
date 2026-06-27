package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

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
