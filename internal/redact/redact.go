// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

// Package redact scrubs known credential values and common token shapes from
// strings and serialized payloads before they are persisted or returned.
package redact

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
)

const Replacement = "[REDACTED]"

var (
	tokenPattern  = regexp.MustCompile(`(?:ghp_[A-Za-z0-9_]{4,}|github_pat_[A-Za-z0-9_]{4,}|gho_[A-Za-z0-9_]{4,}|AIza[A-Za-z0-9_-]{4,}|sk-[A-Za-z0-9_-]{4,})`)
	envSecretKeys = []string{"GLUE_GITHUB_TOKEN"}
	mu            sync.RWMutex
	known         = map[string]struct{}{}
)

// Register records secret values for redaction in memory.
func Register(values ...string) {
	mu.Lock()
	defer mu.Unlock()
	for _, v := range values {
		if v = strings.TrimSpace(v); len(v) >= 4 {
			known[v] = struct{}{}
			if enc, err := json.Marshal(v); err == nil && len(enc) >= 6 {
				known[string(enc[1:len(enc)-1])] = struct{}{}
			}
		}
	}
}

// String replaces known secret values and common token shapes with [REDACTED].
func String(s string) string {
	if s == "" {
		return ""
	}
	mu.RLock()
	vals := make([]string, 0, len(known)+len(envSecretKeys))
	for v := range known {
		vals = append(vals, v)
	}
	mu.RUnlock()
	for _, k := range envSecretKeys {
		if v := strings.TrimSpace(os.Getenv(k)); len(v) >= 4 {
			vals = append(vals, v)
		}
	}
	slices.SortFunc(vals, func(a, b string) int { return len(b) - len(a) })
	for _, secret := range vals {
		s = strings.ReplaceAll(s, secret, Replacement)
	}
	return tokenPattern.ReplaceAllString(s, Replacement)
}

// Bytes replaces known secret values and common token shapes in b.
func Bytes(b []byte) []byte { return []byte(String(string(b))) }
