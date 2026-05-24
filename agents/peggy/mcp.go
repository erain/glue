package peggy

import (
	"context"
	"fmt"
	"os"
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

// MCPResources initializes Peggy's configured MCP servers and returns
// discovered resource metadata plus the manager that owns server lifecycles.
func MCPResources(ctx context.Context, settings MCPSettings) ([]toolsmcp.Resource, *toolsmcp.Manager, MCPSettings, error) {
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
	resources, err := manager.Resources(ctx)
	if err != nil {
		_ = manager.Close()
		return nil, nil, normalized, fmt.Errorf("peggy: mcp: %w", err)
	}
	return resources, manager, normalized, nil
}

// MCPReadResource initializes Peggy's configured MCP servers and reads one
// resource URI from the named server.
func MCPReadResource(ctx context.Context, settings MCPSettings, serverName, uri string) (toolsmcp.ResourceRead, *toolsmcp.Manager, MCPSettings, error) {
	configs, normalized, err := MCPServerConfigs(settings)
	if err != nil {
		return toolsmcp.ResourceRead{}, nil, normalized, err
	}
	manager, err := toolsmcp.NewManager(ctx, configs, toolsmcp.Options{})
	if err != nil {
		return toolsmcp.ResourceRead{}, nil, normalized, fmt.Errorf("peggy: mcp: %w", err)
	}
	read, err := manager.ReadResource(ctx, serverName, uri)
	if err != nil {
		_ = manager.Close()
		return toolsmcp.ResourceRead{}, nil, normalized, fmt.Errorf("peggy: mcp: %w", err)
	}
	return read, manager, normalized, nil
}

// MCPPrompts initializes Peggy's configured MCP servers and returns
// discovered prompt metadata plus the manager that owns server lifecycles.
func MCPPrompts(ctx context.Context, settings MCPSettings) ([]toolsmcp.Prompt, *toolsmcp.Manager, MCPSettings, error) {
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
	prompts, err := manager.Prompts(ctx)
	if err != nil {
		_ = manager.Close()
		return nil, nil, normalized, fmt.Errorf("peggy: mcp: %w", err)
	}
	return prompts, manager, normalized, nil
}

// MCPGetPrompt initializes Peggy's configured MCP servers and renders one
// prompt by name from the named server.
func MCPGetPrompt(ctx context.Context, settings MCPSettings, serverName, name string, args map[string]string) (toolsmcp.PromptGet, *toolsmcp.Manager, MCPSettings, error) {
	configs, normalized, err := MCPServerConfigs(settings)
	if err != nil {
		return toolsmcp.PromptGet{}, nil, normalized, err
	}
	manager, err := toolsmcp.NewManager(ctx, configs, toolsmcp.Options{})
	if err != nil {
		return toolsmcp.PromptGet{}, nil, normalized, fmt.Errorf("peggy: mcp: %w", err)
	}
	prompt, err := manager.GetPrompt(ctx, serverName, name, args)
	if err != nil {
		_ = manager.Close()
		return toolsmcp.PromptGet{}, nil, normalized, fmt.Errorf("peggy: mcp: %w", err)
	}
	return prompt, manager, normalized, nil
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
		}
		server.Transport = transport
		normalized.Servers[name] = server
		if !server.Enabled {
			continue
		}
		if server.TimeoutSeconds < 0 {
			return nil, normalized, fmt.Errorf("peggy: mcp.servers.%s.timeout_seconds must be >= 0", name)
		}
		normalized.Servers[name] = server

		cfg := toolsmcp.ServerConfig{
			Name:      name,
			Transport: transport,
		}
		if server.TimeoutSeconds > 0 {
			cfg.Timeout = time.Duration(server.TimeoutSeconds) * time.Second
		}
		switch transport {
		case toolsmcp.TransportStdio:
			if strings.TrimSpace(server.Command) == "" {
				return nil, normalized, fmt.Errorf("peggy: mcp.servers.%s.command is required", name)
			}
			server.Command = strings.TrimSpace(server.Command)
			normalized.Servers[name] = server
			cfg.Command = server.Command
			cfg.Args = append([]string(nil), server.Args...)
			cfg.Env = append([]string(nil), server.Env...)
			cfg.WorkDir = server.WorkDir
		case toolsmcp.TransportHTTP:
			if strings.TrimSpace(server.URL) == "" {
				return nil, normalized, fmt.Errorf("peggy: mcp.servers.%s.url is required", name)
			}
			cfg.URL = strings.TrimSpace(server.URL)
			headers, err := resolveMCPHeaders(name, server.HeadersEnv)
			if err != nil {
				return nil, normalized, err
			}
			cfg.Headers = headers
		default:
			return nil, normalized, fmt.Errorf("peggy: mcp.servers.%s transport %q is not supported", name, server.Transport)
		}
		configs = append(configs, cfg)
	}
	return configs, normalized, nil
}

func resolveMCPHeaders(serverName string, headersEnv map[string]string) (map[string]string, error) {
	if len(headersEnv) == 0 {
		return nil, nil
	}
	headers := make(map[string]string, len(headersEnv))
	for header, envName := range headersEnv {
		header = strings.TrimSpace(header)
		envName = strings.TrimSpace(envName)
		if header == "" {
			return nil, fmt.Errorf("peggy: mcp.servers.%s.headers_env contains an empty header name", serverName)
		}
		if envName == "" {
			return nil, fmt.Errorf("peggy: mcp.servers.%s.headers_env.%s is empty", serverName, header)
		}
		value, ok := os.LookupEnv(envName)
		if !ok || value == "" {
			return nil, fmt.Errorf("peggy: mcp.servers.%s.headers_env.%s env var %s is not set", serverName, header, envName)
		}
		headers[header] = value
	}
	return headers, nil
}
