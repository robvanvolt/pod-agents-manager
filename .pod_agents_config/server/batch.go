package main

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func batchRoot(root string) string {
	return filepath.Join(root, "batch")
}

func listBatchSummaries(root string) ([]batchSummary, error) {
	entries, err := os.ReadDir(batchRoot(root))
	if err != nil {
		if os.IsNotExist(err) {
			return []batchSummary{}, nil
		}
		return nil, err
	}

	batches := []batchSummary{}
	for _, entry := range entries {
		if !entry.IsDir() || !validIdent(entry.Name()) {
			continue
		}
		summary, err := readBatchSummary(root, entry.Name())
		if err != nil {
			continue
		}
		batches = append(batches, summary)
	}
	sort.Slice(batches, func(i, j int) bool {
		if batches[i].StartedEpoch != batches[j].StartedEpoch {
			return batches[i].StartedEpoch > batches[j].StartedEpoch
		}
		return batches[i].ID > batches[j].ID
	})
	if len(batches) > 40 {
		batches = batches[:40]
	}
	return batches, nil
}

func readBatchSummary(root, id string) (batchSummary, error) {
	summary := batchSummary{ID: id, Status: "interrupted", Targets: []batchTargetSummary{}}
	if !validIdent(id) {
		return summary, fmt.Errorf("invalid batch")
	}
	dir := filepath.Join(batchRoot(root), id)
	info, err := os.Stat(dir)
	if err != nil {
		return summary, err
	}
	if !info.IsDir() {
		return summary, fmt.Errorf("batch not found")
	}

	meta := readBatchMeta(filepath.Join(dir, "meta.conf"))
	summary.StartedAt = meta["started"]
	summary.StartedEpoch = int64Value(meta["started_epoch"])
	if summary.StartedEpoch == 0 {
		if metaInfo, err := os.Stat(filepath.Join(dir, "meta.conf")); err == nil {
			summary.StartedEpoch = metaInfo.ModTime().Unix()
		}
	}
	summary.Concurrent = meta["concurrent"] == "1"
	summary.Source = meta["source"]
	summary.Version = meta["pod_manager_version"]

	targetNames := strings.Fields(meta["targets"])
	if len(targetNames) == 0 {
		matches, _ := filepath.Glob(filepath.Join(dir, "progress", "*.prog"))
		for _, path := range matches {
			targetNames = append(targetNames, strings.TrimSuffix(filepath.Base(path), ".prog"))
		}
	}
	sort.Strings(targetNames)

	stopped := fileExists(filepath.Join(dir, ".stop"))
	runningTargets := 0
	doneTargets := 0
	for _, target := range targetNames {
		if !validIdent(target) {
			continue
		}
		current, total := readBatchProgress(filepath.Join(dir, "progress", target+".prog"))
		if total == 0 {
			total = int(int64Value(meta["total"]))
		}
		status := batchTargetStatus(dir, target, stopped)
		if status == "running" {
			runningTargets++
		}
		if status == "done" {
			doneTargets++
		}
		targetSummary := batchTargetSummary{
			Target:  target,
			Current: current,
			Total:   total,
			Status:  status,
		}
		if agent, instance, ok := splitContainerName(target); ok {
			targetSummary.Agent = agent
			targetSummary.Instance = instance
		}
		if total > 0 {
			targetSummary.Percent = current * 100 / total
		}
		targetSummary.Results = readBatchResultSummary(filepath.Join(dir, "logs", target+".results.jsonl"))
		summary.Targets = append(summary.Targets, targetSummary)
		summary.Current += current
		summary.Total += total
		summary.Results.Processed += targetSummary.Results.Processed
		summary.Results.Failed += targetSummary.Results.Failed
		summary.Results.DurationSeconds += targetSummary.Results.DurationSeconds
	}
	summary.Results.setAverage()

	switch {
	case stopped:
		summary.Status = "stopped"
	case len(summary.Targets) > 0 && doneTargets == len(summary.Targets):
		summary.Status = "done"
	case runningTargets > 0:
		summary.Status = "running"
		summary.ETASeconds = batchETA(summary.StartedEpoch, summary.Current, summary.Total)
	}
	return summary, nil
}

func readBatchMeta(path string) map[string]string {
	meta := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return meta
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key == "" {
			continue
		}
		meta[key] = value
	}
	return meta
}

func readBatchProgress(path string) (int, int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	left, right, ok := strings.Cut(strings.TrimSpace(string(data)), "/")
	if !ok {
		return 0, 0
	}
	current, _ := strconv.Atoi(strings.TrimSpace(left))
	total, _ := strconv.Atoi(strings.TrimSpace(right))
	if current < 0 {
		current = 0
	}
	if total < 0 {
		total = 0
	}
	return current, total
}

func readBatchResultSummary(path string) batchResultSummary {
	file, err := os.Open(path)
	if err != nil {
		return batchResultSummary{}
	}
	defer file.Close()

	summary := batchResultSummary{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var result struct {
			ExitCode        int   `json:"exit_code"`
			DurationSeconds int64 `json:"duration_s"`
		}
		if err := json.Unmarshal([]byte(scanner.Text()), &result); err != nil {
			continue
		}
		summary.Processed++
		if result.ExitCode != 0 {
			summary.Failed++
		}
		if result.DurationSeconds > 0 {
			summary.DurationSeconds += result.DurationSeconds
		}
	}
	summary.setAverage()
	return summary
}

func (summary *batchResultSummary) setAverage() {
	if summary.Processed > 0 {
		summary.AverageSeconds = float64(summary.DurationSeconds) / float64(summary.Processed)
	}
}

func batchTargetStatus(dir, target string, stopped bool) string {
	if fileExists(filepath.Join(dir, "done."+target)) {
		return "done"
	}
	if stopped {
		return "stopped"
	}
	pid := readPID(filepath.Join(dir, "runner-"+target+".pid"))
	if pid > 0 && pidAlive(pid) {
		return "running"
	}
	return "interrupted"
}

func batchETA(startedEpoch int64, current, total int) int64 {
	if startedEpoch <= 0 || current <= 0 || total <= current {
		return 0
	}
	elapsed := time.Now().Unix() - startedEpoch
	if elapsed <= 0 {
		return 0
	}
	return int64(total-current) * elapsed / int64(current)
}

func batchLineLimitFromRequest(r *http.Request) int {
	lines, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("lines")))
	if lines <= 0 || lines > 1200 {
		return 320
	}
	return lines
}

func batchExportIDsFromRequest(r *http.Request) ([]string, error) {
	raw := append([]string{}, r.URL.Query()["batch"]...)
	if batches := strings.TrimSpace(r.URL.Query().Get("batches")); batches != "" {
		raw = append(raw, strings.Split(batches, ",")...)
	}
	seen := map[string]bool{}
	ids := []string{}
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if !validIdent(id) {
			return nil, fmt.Errorf("invalid batch")
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("select at least one batch")
	}
	if len(ids) > 40 {
		return nil, fmt.Errorf("too many batches selected")
	}
	return ids, nil
}

func batchExportFilename(ids []string) string {
	if len(ids) == 1 {
		return "pod-batch-" + ids[0] + ".zip"
	}
	return fmt.Sprintf("pod-batches-%d.zip", len(ids))
}

func validateBatchExport(root string, ids []string) error {
	for _, id := range ids {
		info, err := os.Stat(filepath.Join(batchRoot(root), id))
		if err != nil || !info.IsDir() {
			return fmt.Errorf("batch not found: %s", id)
		}
	}
	return nil
}

func writeBatchExport(w io.Writer, root string, ids []string) error {
	if err := validateBatchExport(root, ids); err != nil {
		return err
	}
	archive := zip.NewWriter(w)
	for _, id := range ids {
		dir := filepath.Join(batchRoot(root), id)
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !info.Mode().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(batchRoot(root), path)
			if err != nil {
				return err
			}
			header, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			header.Name = filepath.ToSlash(filepath.Join("batch", rel))
			header.Method = zip.Deflate
			entry, err := archive.CreateHeader(header)
			if err != nil {
				return err
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(entry, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		})
		if err != nil {
			_ = archive.Close()
			return err
		}
	}
	return archive.Close()
}

func readBatchLog(root, id, target string, lines int) (batchLogSnapshot, error) {
	snapshot := batchLogSnapshot{
		BatchID:    id,
		Target:     target,
		Lines:      lines,
		CapturedAt: time.Now().Format(time.RFC3339),
	}
	if !validIdent(id) || !validIdent(target) {
		return snapshot, fmt.Errorf("invalid batch log target")
	}
	dir := filepath.Join(batchRoot(root), id)
	if !fileExists(dir) {
		return snapshot, fmt.Errorf("batch not found")
	}
	path := filepath.Join(dir, "logs", target+".log")
	output, err := tailTextFile(path, lines, 512*1024)
	if err != nil {
		if os.IsNotExist(err) {
			return snapshot, fmt.Errorf("batch log not found")
		}
		return snapshot, err
	}
	snapshot.Output = output
	return snapshot, nil
}

func tailTextFile(path string, maxLines int, maxBytes int64) (string, error) {
	if maxLines <= 0 {
		maxLines = 320
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return "", err
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes))
	if err != nil {
		return "", err
	}
	text := strings.ReplaceAll(stripANSI(string(data)), "\r\n", "\n")
	if start > 0 {
		if newline := strings.Index(text, "\n"); newline >= 0 {
			text = text[newline+1:]
		}
	}
	raw := strings.Split(strings.TrimSpace(text), "\n")
	if len(raw) > maxLines {
		raw = raw[len(raw)-maxLines:]
	}
	output := strings.TrimSpace(strings.Join(raw, "\n"))
	if output == "" {
		output = "(no batch log output)"
	}
	return output, nil
}

func stopBatch(root, id string) error {
	if !validIdent(id) {
		return fmt.Errorf("invalid batch")
	}
	dir := filepath.Join(batchRoot(root), id)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("batch not found")
	}
	if err := os.WriteFile(filepath.Join(dir, ".stop"), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return err
	}

	summary, err := readBatchSummary(root, id)
	if err != nil {
		return err
	}
	for _, target := range summary.Targets {
		pid := readPID(filepath.Join(dir, "runner-"+target.Target+".pid"))
		if pid > 0 {
			killBatchRunner(pid)
		}
		_ = exec.Command("pkill", "-f", "podman exec.*"+regexp.QuoteMeta(target.Target)).Run()
	}
	return nil
}

func readPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

func pidAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func killBatchRunner(pid int) {
	_ = exec.Command("pkill", "-TERM", "-P", strconv.Itoa(pid)).Run()
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Signal(syscall.SIGTERM)
	}
}

func int64Value(value string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return n
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type batchSummary struct {
	ID           string               `json:"id"`
	StartedAt    string               `json:"started_at,omitempty"`
	StartedEpoch int64                `json:"started_epoch,omitempty"`
	Concurrent   bool                 `json:"concurrent"`
	Source       string               `json:"source,omitempty"`
	Version      string               `json:"pod_manager_version,omitempty"`
	Status       string               `json:"status"`
	Current      int                  `json:"current"`
	Total        int                  `json:"total"`
	ETASeconds   int64                `json:"eta_seconds,omitempty"`
	Results      batchResultSummary   `json:"results"`
	Targets      []batchTargetSummary `json:"targets"`
}

type batchTargetSummary struct {
	Target   string             `json:"target"`
	Agent    string             `json:"agent,omitempty"`
	Instance string             `json:"instance,omitempty"`
	Current  int                `json:"current"`
	Total    int                `json:"total"`
	Percent  int                `json:"percent"`
	Status   string             `json:"status"`
	Results  batchResultSummary `json:"results"`
}

type batchResultSummary struct {
	Processed       int     `json:"processed"`
	Failed          int     `json:"failed"`
	DurationSeconds int64   `json:"duration_seconds"`
	AverageSeconds  float64 `json:"average_seconds"`
}

type batchLogSnapshot struct {
	BatchID    string `json:"batch_id"`
	Target     string `json:"target"`
	Lines      int    `json:"lines"`
	Output     string `json:"output"`
	CapturedAt string `json:"captured_at"`
}
