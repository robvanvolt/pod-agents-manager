package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

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
