package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

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

func appendAudit(root string, entry auditEntry) {
	auditMutex.Lock()
	defer auditMutex.Unlock()

	path := auditFile(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("audit mkdir failed: %v", err)
		return
	}
	if err := rotateAuditIfNeeded(root); err != nil {
		log.Printf("audit rotate failed: %v", err)
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

func rotateAuditIfNeeded(root string) error {
	maxBytes := auditMaxBytes()
	if maxBytes <= 0 {
		return nil
	}

	path := auditFile(root)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Size() < maxBytes {
		return nil
	}

	archivePath := filepath.Join(filepath.Dir(path), fmt.Sprintf("audit.%s.jsonl", time.Now().UTC().Format("20060102-150405.000000000")))
	if err := os.Rename(path, archivePath); err != nil {
		return err
	}
	_ = os.Chmod(archivePath, 0o600)
	return pruneAuditArchives(root)
}

func pruneAuditArchives(root string) error {
	maxArchives := auditMaxArchives()
	if maxArchives < 0 {
		return nil
	}

	archives, err := filepath.Glob(auditArchivePattern(root))
	if err != nil {
		return err
	}
	sort.Strings(archives)
	for len(archives) > maxArchives {
		if err := os.Remove(archives[0]); err != nil && !os.IsNotExist(err) {
			return err
		}
		archives = archives[1:]
	}
	return nil
}

func auditMaxBytes() int64 {
	return int64Env("POD_SERVER_AUDIT_MAX_BYTES", defaultAuditMaxBytes)
}

func auditMaxArchives() int {
	v := int64Env("POD_SERVER_AUDIT_MAX_ARCHIVES", int64(defaultAuditMaxArchives))
	if v > int64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(v)
}

func int64Env(name string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return value
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
