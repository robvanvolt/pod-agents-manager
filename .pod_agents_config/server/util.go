package main

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
)

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

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

// pod() in .pod_agents emits colored progress messages via `\033[…m` sequences.
// They make the JSON response unreadable in the dashboard's <pre>, so strip them.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

var idlePathLineRe = regexp.MustCompile(`^(~(/[A-Za-z0-9._-]+)*|/[A-Za-z0-9._~@%+=:,/-]+)$`)

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
