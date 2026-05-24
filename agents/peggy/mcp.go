package peggy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/erain/glue"
	toolsmcp "github.com/erain/glue/tools/mcp"
)

const defaultMCPTransport = "stdio"

// MCPEnabled reports whether any MCP server is explicitly enabled.
func MCPEnabled(settings MCPSettings) bool {
	for _, server := range settings.Servers {
		if server.Enabled {
			return true
		}
	}
	return false
}

// MCPTools initializes Peggy's configured MCP servers and returns their
// discovered glue tools plus the manager that owns the server lifecycles.
func MCPTools(ctx context.Context, settings MCPSettings) ([]glue.Tool, *toolsmcp.Manager, MCPSettings, error) {
	configs, normalized, err := MCPServerConfigs(settings)
	if err != nil {
		return nil, nil, normalized, err
	}
	if len(configs) == 0 {
		return nil, nil, normalized, nil
	}
	manager, err := toolsmcp.NewManager(ctx, configs, toolsmcp.Options{})
	if err != nil {
		return nil, nil, normalized, fmt.Errorf("peggy: mcp: %w", err)
	}
	return manager.Tools(), manager, normalized, nil
}

// MCPServerConfigs converts Peggy settings into tools/mcp server configs.
func MCPServerConfigs(settings MCPSettings) ([]toolsmcp.ServerConfig, MCPSettings, error) {
	normalized, err := expandMCPSettings(settings)
	if err != nil {
		return nil, normalized, err
	}
	if len(normalized.Servers) == 0 {
		return nil, normalized, nil
	}

	names := make([]string, 0, len(normalized.Servers))
	for name := range normalized.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	var configs []toolsmcp.ServerConfig
	for _, name := range names {
		server := normalized.Servers[name]
		transport := strings.ToLower(strings.TrimSpace(server.Transport))
		if transport == "" {
			transport = defaultMCPTransport
			server.Transport = transport
		}
		normalized.Servers[name] = server
		if !server.Enabled {
			continue
		}
		if transport != defaultMCPTransport {
			return nil, normalized, fmt.Errorf("peggy: mcp.servers.%s transport %q is not supported yet", name, server.Transport)
		}
		if strings.TrimSpace(server.Command) == "" {
			return nil, normalized, fmt.Errorf("peggy: mcp.servers.%s.command is required", name)
		}
		server.Command = strings.TrimSpace(server.Command)
		if server.TimeoutSeconds < 0 {
			return nil, normalized, fmt.Errorf("peggy: mcp.servers.%s.timeout_seconds must be >= 0", name)
		}
		normalized.Servers[name] = server

		cfg := toolsmcp.ServerConfig{
			Name:    name,
			Command: server.Command,
			Args:    append([]string(nil), server.Args...),
			Env:     append([]string(nil), server.Env...),
			WorkDir: server.WorkDir,
		}
		if server.TimeoutSeconds > 0 {
			cfg.Timeout = time.Duration(server.TimeoutSeconds) * time.Second
		}
		configs = append(configs, cfg)
	}
	return configs, normalized, nil
}
