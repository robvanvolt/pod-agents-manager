package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

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
	return parsePodVersion(data)
}

func parsePodVersion(data []byte) string {
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

type installSource struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
}

type updateRefStatus struct {
	Ref     string `json:"ref"`
	Version string `json:"version"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

type updateStatusResponse struct {
	Version   string            `json:"version"`
	Command   string            `json:"command"`
	Source    installSource     `json:"source"`
	Latest    []updateRefStatus `json:"latest"`
	CheckedAt string            `json:"checked_at"`
}

type updateStatusCacheEntry struct {
	Key       string
	ExpiresAt time.Time
	Response  updateStatusResponse
}

func readInstallSource(root string) installSource {
	source := installSource{Repo: "robvanvolt/pod-agents-manager", Ref: "main"}
	data, err := os.ReadFile(filepath.Join(root, "install-source.conf"))
	if err != nil {
		return source
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "repo":
			if validGitHubRepo(value) {
				source.Repo = value
			}
		case "ref":
			if validInstallRef(value) {
				source.Ref = value
			}
		}
	}
	return source
}

func readCommandName(root string) string {
	data, err := os.ReadFile(filepath.Join(root, ".cmd_name"))
	if err != nil {
		return "pod"
	}
	cmd := strings.TrimSpace(string(data))
	if !validIdent(cmd) {
		return "pod"
	}
	return cmd
}

var githubRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var installRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]{0,127}$`)

func validGitHubRepo(repo string) bool { return githubRepoRe.MatchString(repo) }

func validInstallRef(ref string) bool {
	return installRefRe.MatchString(ref) && !strings.Contains(ref, "..")
}

func readUpdateStatus(root string, refresh bool) updateStatusResponse {
	source := readInstallSource(root)
	localVersion := readPodVersion(root)
	command := readCommandName(root)
	key := strings.Join([]string{source.Repo, source.Ref, localVersion, command}, "|")
	now := time.Now()

	updateStatusMutex.Lock()
	defer updateStatusMutex.Unlock()
	if !refresh && updateStatusCache.Key == key && now.Before(updateStatusCache.ExpiresAt) {
		return updateStatusCache.Response
	}

	response := updateStatusResponse{
		Version:   localVersion,
		Command:   command,
		Source:    source,
		CheckedAt: now.Format(time.RFC3339),
	}
	for _, ref := range []string{"main", "dev"} {
		version, err := fetchRemotePodVersion(source.Repo, ref)
		item := updateRefStatus{Ref: ref, Version: version, State: updateVersionState(localVersion, version)}
		if err != nil {
			item.Version = "unknown"
			item.State = "unknown"
			item.Error = err.Error()
		}
		response.Latest = append(response.Latest, item)
	}
	updateStatusCache = updateStatusCacheEntry{
		Key:       key,
		ExpiresAt: now.Add(updateStatusTTL),
		Response:  response,
	}
	return response
}

func fetchRemotePodVersion(repo, ref string) (string, error) {
	if !validGitHubRepo(repo) || !validInstallRef(ref) {
		return "", fmt.Errorf("invalid repository source")
	}
	target := "https://raw.githubusercontent.com/" + repo + "/" + url.PathEscape(ref) + "/.pod_agents_config/version.conf"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("version lookup returned %s", res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 4096))
	if err != nil {
		return "", err
	}
	version := parsePodVersion(data)
	if version == "unknown" {
		return "", fmt.Errorf("version lookup did not include POD_AGENTS_VERSION")
	}
	return version, nil
}

func updateVersionState(localVersion, remoteVersion string) string {
	if localVersion == "unknown" || remoteVersion == "" || remoteVersion == "unknown" {
		return "unknown"
	}
	if localVersion == remoteVersion {
		return "current"
	}
	return "different"
}
