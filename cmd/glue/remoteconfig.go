// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// remoteConfigPath is where a repo describes how its remote jobs run.
const remoteConfigPath = ".glue/remote.yaml"

// remoteConfig is the content of .glue/remote.yaml. Every field is optional;
// command-line flags take precedence over it.
type remoteConfig struct {
	Image       string   `yaml:"image"`
	Setup       []string `yaml:"setup"`
	Verify      []string `yaml:"verify"`
	Deliver     string   `yaml:"deliver"`
	Policy      string   `yaml:"policy"`
	EgressHosts []string `yaml:"egress_hosts"`
	Resources   struct {
		CPU    string `yaml:"cpu"`
		Memory string `yaml:"memory"`
	} `yaml:"resources"`
	Budgets struct {
		WallClock     string `yaml:"wall_clock"`
		TokenBudget   int64  `yaml:"token_budget"`
		MaxIterations int    `yaml:"max_iterations"`
	} `yaml:"budgets"`
}

// loadRemoteConfig reads .glue/remote.yaml from the git checkout containing
// dir. A missing file is not an error. Unknown keys and wrong types are.
func loadRemoteConfig(dir string) (*remoteConfig, string, error) {
	root := dir
	if out, err := gitOutput(context.Background(), dir, "rev-parse", "--show-toplevel"); err == nil && out != "" {
		root = out
	}
	path := filepath.Join(root, remoteConfigPath)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, path, err
	}
	var cfg remoteConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) { // an empty file is an empty config
		return nil, path, fmt.Errorf("%s: %w", remoteConfigPath, err)
	}
	if cfg.Budgets.WallClock != "" {
		if _, err := time.ParseDuration(cfg.Budgets.WallClock); err != nil {
			return nil, path, fmt.Errorf("%s: budgets.wall_clock: %w", remoteConfigPath, err)
		}
	}
	for _, h := range cfg.EgressHosts {
		if h = strings.TrimSpace(h); h == "" || strings.ContainsAny(h, "/: ") {
			return nil, path, fmt.Errorf("%s: egress_hosts: %q is not a host name", remoteConfigPath, h)
		}
	}
	return &cfg, path, nil
}

// applyRemoteConfig copies config values into the submit options whose flags
// were not set on the command line, and describes what it applied.
func applyRemoteConfig(cfg *remoteConfig, set map[string]bool, t *jobTargetOpts, verify *repeatedStrings, deliver, policy *string, wallClock *time.Duration, budget *int64, maxIter *int) string {
	var used []string
	use := func(key string, ok bool) {
		if ok {
			used = append(used, key)
		}
	}
	if cfg.Image != "" && !set["image"] {
		t.image = cfg.Image
		use("image", true)
	}
	if len(cfg.Verify) > 0 && !set["verify"] {
		*verify = append(repeatedStrings(nil), cfg.Verify...)
		use("verify", true)
	}
	if cfg.Deliver != "" && !set["deliver"] && !set["delivery"] {
		*deliver = cfg.Deliver
		use("deliver", true)
	}
	if cfg.Policy != "" && !set["policy"] {
		*policy = cfg.Policy
		use("policy", true)
	}
	if len(cfg.EgressHosts) > 0 {
		t.egress = append(t.egress, cfg.EgressHosts...)
		use("egress_hosts", true)
	}
	if cfg.Resources.CPU != "" && !set["cpu"] {
		t.cpu = cfg.Resources.CPU
		use("resources.cpu", true)
	}
	if cfg.Resources.Memory != "" && !set["memory"] {
		t.memory = cfg.Resources.Memory
		use("resources.memory", true)
	}
	if cfg.Budgets.WallClock != "" && !set["wall-clock"] {
		*wallClock, _ = time.ParseDuration(cfg.Budgets.WallClock)
		use("budgets.wall_clock", true)
	}
	if cfg.Budgets.TokenBudget > 0 && !set["budget"] {
		*budget = cfg.Budgets.TokenBudget
		use("budgets.token_budget", true)
	}
	if cfg.Budgets.MaxIterations > 0 && !set["max-iterations"] {
		*maxIter = cfg.Budgets.MaxIterations
		use("budgets.max_iterations", true)
	}
	if len(cfg.Setup) > 0 {
		use("setup", true)
	}
	if len(used) == 0 {
		return remoteConfigPath + " (nothing applied)"
	}
	return remoteConfigPath + ": " + strings.Join(used, ", ")
}

// splitCommand splits a command line into argv the way a POSIX shell would
// for plain words: whitespace separates arguments, single quotes keep text
// literally, double quotes keep spaces and honor \" and \\, and a backslash
// outside quotes escapes the next character. It does not expand variables or
// globs; use sh -c '...' for that.
func splitCommand(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg, quote := false, rune(0)
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(runes) && (runes[i+1] == '"' || runes[i+1] == '\\'):
				i++
				cur.WriteRune(runes[i])
			default:
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inArg = c, true
		case c == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			inArg = true
		case c == ' ' || c == '\t' || c == '\n':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(c)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote in %q", quote, line)
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}
