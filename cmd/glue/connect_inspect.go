// Read-only "glue connect" inspection surfaces: daemon status, diagnose,
// and the tool/skill/role/memory/permission/MCP catalogs with their
// text renderers.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/erain/glue/daemon"
	// Register the shipped providers so they resolve through the
	// providers registry by name (--provider). Importing for side
)

type daemonToolCatalog struct {
	Tools []daemonToolCatalogEntry `json:"tools"`
}

type daemonSkillCatalog struct {
	Skills []daemon.SkillCatalogEntry `json:"skills"`
}

type daemonRoleCatalog struct {
	Roles []daemon.RoleCatalogEntry `json:"roles"`
}

type daemonMCPResourceCatalog struct {
	Resources []daemon.MCPResourceCatalogEntry `json:"resources"`
}

type daemonMCPPromptCatalog struct {
	Prompts []daemon.MCPPromptCatalogEntry `json:"prompts"`
}

type daemonPermissionCatalog struct {
	Permissions []daemon.PermissionGrant `json:"permissions"`
}

type daemonToolCatalogEntry struct {
	Name                    string          `json:"name"`
	Description             string          `json:"description,omitempty"`
	Parameters              json.RawMessage `json:"parameters,omitempty"`
	RequiresPermission      bool            `json:"requires_permission"`
	PermissionAction        string          `json:"permission_action,omitempty"`
	PermissionTargetPreview string          `json:"permission_target_preview,omitempty"`
}

type daemonStatus struct {
	OK           bool     `json:"ok"`
	Version      int      `json:"version"`
	ActiveRuns   int      `json:"active_runs"`
	ToolsCount   int      `json:"tools_count"`
	Capabilities []string `json:"capabilities"`
}

type daemonDiagnostics = daemon.DiagnosticResponse

type daemonDiagnosis struct {
	OK            bool               `json:"ok"`
	State         string             `json:"state"`
	Summary       string             `json:"summary"`
	MetadataPath  string             `json:"metadata_path,omitempty"`
	MetadataFound bool               `json:"metadata_found"`
	MetadataError string             `json:"metadata_error,omitempty"`
	MetadataPID   int                `json:"metadata_pid,omitempty"`
	BaseURL       string             `json:"base_url,omitempty"`
	BaseURLSource string             `json:"base_url_source,omitempty"`
	TokenSource   string             `json:"token_source,omitempty"`
	HTTPStatus    int                `json:"http_status,omitempty"`
	Status        *daemonStatus      `json:"status,omitempty"`
	Diagnostics   *daemonDiagnostics `json:"diagnostics,omitempty"`
	Suggestions   []string           `json:"suggestions,omitempty"`
}

type daemonInspect struct {
	Status       daemonStatus                     `json:"status"`
	Tools        []daemonToolCatalogEntry         `json:"tools"`
	Skills       []daemon.SkillCatalogEntry       `json:"skills,omitempty"`
	Roles        []daemon.RoleCatalogEntry        `json:"roles,omitempty"`
	Memories     []daemon.MemoryEntry             `json:"memories,omitempty"`
	Permissions  []daemon.PermissionGrant         `json:"permissions,omitempty"`
	MCPResources []daemon.MCPResourceCatalogEntry `json:"mcp_resources,omitempty"`
	MCPPrompts   []daemon.MCPPromptCatalogEntry   `json:"mcp_prompts,omitempty"`
}

func runConnectTools(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	catalog, err := fetchDaemonTools(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(catalog)
	}
	writeDaemonToolCatalog(stdout, catalog.Tools)
	return nil
}

func runConnectSkills(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	catalog, err := fetchDaemonSkills(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(catalog)
	}
	writeDaemonSkillCatalog(stdout, catalog.Skills)
	return nil
}

func runConnectRoles(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	catalog, err := fetchDaemonRoles(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(catalog)
	}
	writeDaemonRoleCatalog(stdout, catalog.Roles)
	return nil
}

func runConnectMCPResources(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	catalog, err := fetchDaemonMCPResources(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(catalog)
	}
	writeDaemonMCPResourceCatalog(stdout, catalog.Resources)
	return nil
}

func runConnectMCPPrompts(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	catalog, err := fetchDaemonMCPPrompts(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(catalog)
	}
	writeDaemonMCPPromptCatalog(stdout, catalog.Prompts)
	return nil
}

func runConnectMCPRead(ctx context.Context, cfg connectConfig, server, uri string, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	server = strings.TrimSpace(server)
	uri = strings.TrimSpace(uri)
	if server == "" {
		return errors.New("--server is required for --mcp-read")
	}
	if uri == "" {
		return errors.New("--uri is required for --mcp-read")
	}
	read, err := requestDaemonMCPResourceRead(ctx, cfg, server, uri, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(read)
	}
	writeDaemonMCPResourceRead(stdout, read)
	return nil
}

func runConnectMCPPrompt(ctx context.Context, cfg connectConfig, server, name string, args map[string]string, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	server = strings.TrimSpace(server)
	name = strings.TrimSpace(name)
	if server == "" {
		return errors.New("--server is required for --mcp-prompt")
	}
	if name == "" {
		return errors.New("--name is required for --mcp-prompt")
	}
	rendered, err := requestDaemonMCPPrompt(ctx, cfg, server, name, args, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rendered)
	}
	writeDaemonMCPPrompt(stdout, rendered)
	return nil
}

func runConnectRecall(ctx context.Context, cfg connectConfig, query string, memoriesOnly bool, limit int, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	query = strings.TrimSpace(query)
	if query == "" {
		return errors.New("--recall query is required")
	}
	if limit < 0 {
		return errors.New("--recall-limit must be non-negative")
	}
	recall, err := requestDaemonRecall(ctx, cfg, daemon.RecallRequest{
		Query:        query,
		Limit:        limit,
		MemoriesOnly: memoriesOnly,
	}, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(recall)
	}
	writeDaemonRecallHits(stdout, recall.Hits)
	return nil
}

func runConnectMemories(ctx context.Context, cfg connectConfig, limit int, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	if limit < 0 {
		return errors.New("--memory-limit must be non-negative")
	}
	catalog, err := fetchDaemonMemories(ctx, cfg, limit, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(catalog)
	}
	writeDaemonMemories(stdout, catalog.Memories)
	return nil
}

func runConnectForgetMemory(ctx context.Context, cfg connectConfig, id string, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("--forget-memory is required")
	}
	forgotten, err := requestDaemonForgetMemory(ctx, cfg, id, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(forgotten)
	}
	writeDaemonMemory(stdout, forgotten.Memory)
	return nil
}

func runConnectPermissions(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	catalog, err := fetchDaemonPermissions(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(catalog)
	}
	writeDaemonPermissions(stdout, catalog.Permissions)
	return nil
}

func runConnectForgetPermission(ctx context.Context, cfg connectConfig, id string, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("--forget-permission is required")
	}
	forgotten, err := requestDaemonForgetPermission(ctx, cfg, id, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(forgotten)
	}
	writeDaemonPermission(stdout, forgotten.Permission)
	return nil
}

func runConnectStatus(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	status, err := fetchDaemonStatus(ctx, cfg, client)
	if err != nil {
		return err
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}
	writeDaemonStatus(stdout, status)
	return nil
}

func runConnectDiagnose(ctx context.Context, cfg connectConfig, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	diagnosis := diagnoseDaemon(ctx, cfg, client)
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(diagnosis)
	}
	writeDaemonDiagnosis(stdout, diagnosis)
	return nil
}

func runConnectInspect(ctx context.Context, cfg connectConfig, memoryLimit int, jsonOutput bool, stdout io.Writer, client httpDoer) error {
	if memoryLimit < 0 {
		return errors.New("--memory-limit must be non-negative")
	}
	status, err := fetchDaemonStatus(ctx, cfg, client)
	if err != nil {
		return err
	}
	catalog, err := fetchDaemonTools(ctx, cfg, client)
	if err != nil {
		return err
	}
	inspect := daemonInspect{Status: status, Tools: catalog.Tools}
	if daemonHasCapability(status, "skills") {
		skills, err := fetchDaemonSkills(ctx, cfg, client)
		if err != nil {
			return err
		}
		inspect.Skills = skills.Skills
	}
	if daemonHasCapability(status, "roles") {
		roles, err := fetchDaemonRoles(ctx, cfg, client)
		if err != nil {
			return err
		}
		inspect.Roles = roles.Roles
	}
	if daemonHasCapability(status, "memories") {
		memories, err := fetchDaemonMemories(ctx, cfg, memoryLimit, client)
		if err != nil {
			return err
		}
		inspect.Memories = memories.Memories
	}
	if daemonHasCapability(status, "permission_grants") {
		permissions, err := fetchDaemonPermissions(ctx, cfg, client)
		if err != nil {
			return err
		}
		inspect.Permissions = permissions.Permissions
	}
	if daemonHasCapability(status, "mcp_resources") {
		resources, err := fetchDaemonMCPResources(ctx, cfg, client)
		if err != nil {
			return err
		}
		inspect.MCPResources = resources.Resources
	}
	if daemonHasCapability(status, "mcp_prompts") {
		prompts, err := fetchDaemonMCPPrompts(ctx, cfg, client)
		if err != nil {
			return err
		}
		inspect.MCPPrompts = prompts.Prompts
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(inspect)
	}
	writeDaemonInspect(stdout, inspect)
	return nil
}

func fetchDaemonStatus(ctx context.Context, cfg connectConfig, client httpDoer) (daemonStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/status", nil)
	if err != nil {
		return daemonStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonStatus{}, fmt.Errorf("daemon status: %s", httpStatusError(resp))
	}
	var status daemonStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return daemonStatus{}, err
	}
	return status, nil
}

func diagnoseDaemon(ctx context.Context, cfg connectConfig, client httpDoer) daemonDiagnosis {
	diagnosis := daemonDiagnosis{
		State:        "unknown",
		MetadataPath: strings.TrimSpace(cfg.MetadataPath),
	}
	var meta daemonMetadata
	var metadataErr error
	if diagnosis.MetadataPath != "" {
		loaded, err := readDaemonMetadata(diagnosis.MetadataPath)
		if err != nil {
			metadataErr = err
			diagnosis.MetadataError = err.Error()
		} else {
			meta = loaded
			diagnosis.MetadataFound = true
			diagnosis.MetadataPID = meta.PID
		}
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	switch {
	case baseURL != "":
		diagnosis.BaseURLSource = "flag"
	case strings.TrimSpace(meta.BaseURL) != "":
		baseURL = strings.TrimRight(strings.TrimSpace(meta.BaseURL), "/")
		diagnosis.BaseURLSource = "metadata"
	}
	diagnosis.BaseURL = baseURL

	token := strings.TrimSpace(cfg.Token)
	switch {
	case token != "":
		diagnosis.TokenSource = "flag"
	case strings.TrimSpace(meta.Token) != "":
		token = strings.TrimSpace(meta.Token)
		diagnosis.TokenSource = "metadata"
	case strings.TrimSpace(os.Getenv("GLUE_DAEMON_TOKEN")) != "":
		token = strings.TrimSpace(os.Getenv("GLUE_DAEMON_TOKEN"))
		diagnosis.TokenSource = "GLUE_DAEMON_TOKEN"
	default:
		diagnosis.TokenSource = "missing"
	}

	if baseURL == "" {
		if metadataErr != nil {
			diagnosis.State = "no_metadata"
			diagnosis.Summary = "daemon metadata is missing or unreadable"
			diagnosis.Suggestions = []string{"Start Peggy with `peggy serve` or pass --base-url and --token explicitly."}
			return diagnosis
		}
		diagnosis.State = "missing_base_url"
		diagnosis.Summary = "daemon base URL is not configured"
		diagnosis.Suggestions = []string{"Start Peggy with `peggy serve` or pass --base-url."}
		return diagnosis
	}
	if token == "" {
		diagnosis.State = "missing_token"
		diagnosis.Summary = "daemon bearer token is not configured"
		diagnosis.Suggestions = []string{"Use daemon metadata, pass --token, or set GLUE_DAEMON_TOKEN."}
		return diagnosis
	}

	status, diagnostics, httpStatus, err := requestDaemonDiagnostics(ctx, baseURL, token, client)
	diagnosis.HTTPStatus = httpStatus
	if err != nil {
		diagnosis.Summary = err.Error()
		switch {
		case httpStatus == http.StatusUnauthorized:
			diagnosis.State = "auth_failed"
			diagnosis.Suggestions = []string{"Use the token from the active metadata file or restart the daemon to refresh metadata."}
		case httpStatus > 0:
			diagnosis.State = "unhealthy"
			diagnosis.Suggestions = []string{"Check daemon logs and restart `peggy serve` if the error persists."}
		case diagnosis.MetadataFound && strings.TrimSpace(cfg.BaseURL) == "":
			diagnosis.State = "stale_metadata"
			diagnosis.Suggestions = []string{"The metadata file points at an unreachable daemon; restart `peggy serve` to refresh it."}
		default:
			diagnosis.State = "unreachable"
			diagnosis.Suggestions = []string{"Check that the daemon is running and the --base-url value is correct."}
		}
		return diagnosis
	}
	diagnosis.OK = true
	diagnosis.State = "healthy"
	diagnosis.Summary = "daemon is reachable and authenticated"
	diagnosis.Status = &status
	diagnosis.Diagnostics = &diagnostics
	return diagnosis
}

func requestDaemonDiagnostics(ctx context.Context, baseURL, token string, client httpDoer) (daemonStatus, daemonDiagnostics, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/diagnostics", nil)
	if err != nil {
		return daemonStatus{}, daemonDiagnostics{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonStatus{}, daemonDiagnostics{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonStatus{}, daemonDiagnostics{}, resp.StatusCode, fmt.Errorf("daemon diagnostics: %s", httpStatusError(resp))
	}
	var diagnostics daemonDiagnostics
	if err := json.NewDecoder(resp.Body).Decode(&diagnostics); err != nil {
		return daemonStatus{}, daemonDiagnostics{}, resp.StatusCode, err
	}
	status := daemonStatus{
		OK:           diagnostics.OK,
		Version:      diagnostics.Version,
		ActiveRuns:   diagnostics.ActiveRuns,
		ToolsCount:   diagnostics.ToolsCount,
		Capabilities: diagnostics.Capabilities,
	}
	return status, diagnostics, resp.StatusCode, nil
}

func writeDaemonStatus(w io.Writer, status daemonStatus) {
	state := "error"
	if status.OK {
		state = "ok"
	}
	fmt.Fprintf(w, "status: %s\n", state)
	fmt.Fprintf(w, "version: %d\n", status.Version)
	fmt.Fprintf(w, "active_runs: %d\n", status.ActiveRuns)
	fmt.Fprintf(w, "tools_count: %d\n", status.ToolsCount)
	if len(status.Capabilities) > 0 {
		fmt.Fprintf(w, "capabilities: %s\n", strings.Join(status.Capabilities, ", "))
	}
}

func writeDaemonDiagnosis(w io.Writer, diagnosis daemonDiagnosis) {
	state := diagnosis.State
	if state == "" {
		state = "unknown"
	}
	fmt.Fprintf(w, "daemon: %s\n", state)
	if diagnosis.Summary != "" {
		fmt.Fprintf(w, "summary: %s\n", diagnosis.Summary)
	}
	if diagnosis.MetadataPath != "" {
		fmt.Fprintf(w, "metadata: %s", diagnosis.MetadataPath)
		if diagnosis.MetadataFound {
			fmt.Fprint(w, " (found)")
			if diagnosis.MetadataPID != 0 {
				fmt.Fprintf(w, " pid=%d", diagnosis.MetadataPID)
			}
		} else {
			fmt.Fprint(w, " (missing)")
		}
		fmt.Fprintln(w)
	}
	if diagnosis.MetadataError != "" {
		fmt.Fprintf(w, "metadata_error: %s\n", diagnosis.MetadataError)
	}
	if diagnosis.BaseURL != "" {
		fmt.Fprintf(w, "base_url: %s", diagnosis.BaseURL)
		if diagnosis.BaseURLSource != "" {
			fmt.Fprintf(w, " (%s)", diagnosis.BaseURLSource)
		}
		fmt.Fprintln(w)
	}
	if diagnosis.TokenSource != "" {
		fmt.Fprintf(w, "token: %s\n", diagnosis.TokenSource)
	}
	if diagnosis.HTTPStatus != 0 {
		fmt.Fprintf(w, "http_status: %d\n", diagnosis.HTTPStatus)
	}
	if diagnosis.Status != nil {
		fmt.Fprintf(w, "active_runs: %d\n", diagnosis.Status.ActiveRuns)
		fmt.Fprintf(w, "tools_count: %d\n", diagnosis.Status.ToolsCount)
	}
	if diagnosis.Diagnostics != nil {
		runtime := diagnosis.Diagnostics.Runtime
		if runtime.Provider != "" {
			fmt.Fprintf(w, "provider: %s\n", runtime.Provider)
		}
		if runtime.Model != "" {
			fmt.Fprintf(w, "model: %s\n", runtime.Model)
		}
		if runtime.StoreType != "" || runtime.StorePath != "" {
			fmt.Fprintf(w, "store: %s", runtime.StoreType)
			if runtime.StorePath != "" {
				fmt.Fprintf(w, " %s", runtime.StorePath)
			}
			fmt.Fprintln(w)
		}
		if runtime.ListenAddr != "" {
			fmt.Fprintf(w, "listen_addr: %s\n", runtime.ListenAddr)
		}
		if runtime.MetadataPath != "" {
			fmt.Fprintf(w, "runtime_metadata: %s\n", runtime.MetadataPath)
		}
		if runtime.TokenSource != "" {
			fmt.Fprintf(w, "runtime_token_source: %s\n", runtime.TokenSource)
		}
		if runtime.CodingEnabled {
			fmt.Fprintf(w, "coding: enabled")
			if runtime.CodingWorkDir != "" {
				fmt.Fprintf(w, " %s", runtime.CodingWorkDir)
			}
			fmt.Fprintln(w)
		}
		if len(diagnosis.Diagnostics.RecentErrors) > 0 {
			fmt.Fprintln(w, "recent_errors:")
			for _, entry := range diagnosis.Diagnostics.RecentErrors {
				timestamp := "unknown"
				if !entry.Time.IsZero() {
					timestamp = entry.Time.Format(time.RFC3339)
				}
				fmt.Fprintf(w, "  %s %s", timestamp, entry.Error)
				if entry.RunID != "" {
					fmt.Fprintf(w, " run=%s", entry.RunID)
				}
				if entry.SessionID != "" {
					fmt.Fprintf(w, " session=%s", entry.SessionID)
				}
				if entry.ClientID != "" {
					fmt.Fprintf(w, " client=%s", entry.ClientID)
				}
				fmt.Fprintln(w)
			}
		}
	}
	if len(diagnosis.Suggestions) > 0 {
		fmt.Fprintln(w, "suggestions:")
		for _, suggestion := range diagnosis.Suggestions {
			fmt.Fprintf(w, "  - %s\n", suggestion)
		}
	}
}

func writeDaemonInspect(w io.Writer, inspect daemonInspect) {
	writeDaemonStatus(w, inspect.Status)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "tools:")
	writeDaemonToolCatalogIndented(w, inspect.Tools, "  ")
	if daemonHasCapability(inspect.Status, "skills") {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "skills:")
		writeDaemonSkillCatalogIndented(w, inspect.Skills, "  ")
	}
	if daemonHasCapability(inspect.Status, "roles") {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "roles:")
		writeDaemonRoleCatalogIndented(w, inspect.Roles, "  ")
	}
	if daemonHasCapability(inspect.Status, "memories") {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "memories:")
		writeDaemonMemoriesIndented(w, inspect.Memories, "  ")
	}
	if daemonHasCapability(inspect.Status, "permission_grants") {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "permissions:")
		writeDaemonPermissionsIndented(w, inspect.Permissions, "  ")
	}
	if daemonHasCapability(inspect.Status, "mcp_resources") {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "mcp_resources:")
		writeDaemonMCPResourceCatalogIndented(w, inspect.MCPResources, "  ")
	}
	if daemonHasCapability(inspect.Status, "mcp_prompts") {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "mcp_prompts:")
		writeDaemonMCPPromptCatalogIndented(w, inspect.MCPPrompts, "  ")
	}
}

func fetchDaemonTools(ctx context.Context, cfg connectConfig, client httpDoer) (daemonToolCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/tools", nil)
	if err != nil {
		return daemonToolCatalog{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonToolCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonToolCatalog{}, fmt.Errorf("daemon tools: %s", httpStatusError(resp))
	}
	var catalog daemonToolCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return daemonToolCatalog{}, err
	}
	if catalog.Tools == nil {
		catalog.Tools = []daemonToolCatalogEntry{}
	}
	return catalog, nil
}

func fetchDaemonSkills(ctx context.Context, cfg connectConfig, client httpDoer) (daemonSkillCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/skills", nil)
	if err != nil {
		return daemonSkillCatalog{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonSkillCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonSkillCatalog{}, fmt.Errorf("daemon skills: %s", httpStatusError(resp))
	}
	var catalog daemonSkillCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return daemonSkillCatalog{}, err
	}
	if catalog.Skills == nil {
		catalog.Skills = []daemon.SkillCatalogEntry{}
	}
	return catalog, nil
}

func fetchDaemonRoles(ctx context.Context, cfg connectConfig, client httpDoer) (daemonRoleCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/roles", nil)
	if err != nil {
		return daemonRoleCatalog{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonRoleCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonRoleCatalog{}, fmt.Errorf("daemon roles: %s", httpStatusError(resp))
	}
	var catalog daemonRoleCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return daemonRoleCatalog{}, err
	}
	if catalog.Roles == nil {
		catalog.Roles = []daemon.RoleCatalogEntry{}
	}
	return catalog, nil
}

func fetchDaemonMemories(ctx context.Context, cfg connectConfig, limit int, client httpDoer) (daemon.MemoryCatalogResponse, error) {
	endpoint := cfg.BaseURL + "/v1/memories"
	if limit > 0 {
		values := url.Values{}
		values.Set("limit", fmt.Sprint(limit))
		endpoint += "?" + values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return daemon.MemoryCatalogResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemon.MemoryCatalogResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemon.MemoryCatalogResponse{}, fmt.Errorf("daemon memories: %s", httpStatusError(resp))
	}
	var catalog daemon.MemoryCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return daemon.MemoryCatalogResponse{}, err
	}
	if catalog.Memories == nil {
		catalog.Memories = []daemon.MemoryEntry{}
	}
	return catalog, nil
}

func requestDaemonForgetMemory(ctx context.Context, cfg connectConfig, id string, client httpDoer) (daemon.MemoryForgetResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, cfg.BaseURL+"/v1/memories/"+url.PathEscape(id), nil)
	if err != nil {
		return daemon.MemoryForgetResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemon.MemoryForgetResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemon.MemoryForgetResponse{}, fmt.Errorf("daemon memory delete: %s", httpStatusError(resp))
	}
	var forgotten daemon.MemoryForgetResponse
	if err := json.NewDecoder(resp.Body).Decode(&forgotten); err != nil {
		return daemon.MemoryForgetResponse{}, err
	}
	return forgotten, nil
}

func fetchDaemonPermissions(ctx context.Context, cfg connectConfig, client httpDoer) (daemonPermissionCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/permissions", nil)
	if err != nil {
		return daemonPermissionCatalog{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonPermissionCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonPermissionCatalog{}, fmt.Errorf("daemon permissions: %s", httpStatusError(resp))
	}
	var catalog daemonPermissionCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return daemonPermissionCatalog{}, err
	}
	if catalog.Permissions == nil {
		catalog.Permissions = []daemon.PermissionGrant{}
	}
	return catalog, nil
}

func requestDaemonForgetPermission(ctx context.Context, cfg connectConfig, id string, client httpDoer) (daemon.PermissionForgetResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, cfg.BaseURL+"/v1/permissions/"+url.PathEscape(id), nil)
	if err != nil {
		return daemon.PermissionForgetResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemon.PermissionForgetResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemon.PermissionForgetResponse{}, fmt.Errorf("daemon permission delete: %s", httpStatusError(resp))
	}
	var forgotten daemon.PermissionForgetResponse
	if err := json.NewDecoder(resp.Body).Decode(&forgotten); err != nil {
		return daemon.PermissionForgetResponse{}, err
	}
	return forgotten, nil
}

func fetchDaemonMCPResources(ctx context.Context, cfg connectConfig, client httpDoer) (daemonMCPResourceCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/mcp/resources", nil)
	if err != nil {
		return daemonMCPResourceCatalog{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonMCPResourceCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonMCPResourceCatalog{}, fmt.Errorf("daemon MCP resources: %s", httpStatusError(resp))
	}
	var catalog daemonMCPResourceCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return daemonMCPResourceCatalog{}, err
	}
	if catalog.Resources == nil {
		catalog.Resources = []daemon.MCPResourceCatalogEntry{}
	}
	return catalog, nil
}

func fetchDaemonMCPPrompts(ctx context.Context, cfg connectConfig, client httpDoer) (daemonMCPPromptCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/mcp/prompts", nil)
	if err != nil {
		return daemonMCPPromptCatalog{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		return daemonMCPPromptCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonMCPPromptCatalog{}, fmt.Errorf("daemon MCP prompts: %s", httpStatusError(resp))
	}
	var catalog daemonMCPPromptCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return daemonMCPPromptCatalog{}, err
	}
	if catalog.Prompts == nil {
		catalog.Prompts = []daemon.MCPPromptCatalogEntry{}
	}
	return catalog, nil
}

func requestDaemonMCPResourceRead(ctx context.Context, cfg connectConfig, server, uri string, client httpDoer) (daemon.MCPResourceReadResponse, error) {
	payload := daemon.MCPReadResourceRequest{Server: server, URI: uri}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return daemon.MCPResourceReadResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/v1/mcp/resources/read", &body)
	if err != nil {
		return daemon.MCPResourceReadResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return daemon.MCPResourceReadResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemon.MCPResourceReadResponse{}, fmt.Errorf("daemon MCP resource read: %s", httpStatusError(resp))
	}
	var read daemon.MCPResourceReadResponse
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil {
		return daemon.MCPResourceReadResponse{}, err
	}
	if read.Contents == nil {
		read.Contents = []daemon.MCPResourceContent{}
	}
	return read, nil
}

func requestDaemonMCPPrompt(ctx context.Context, cfg connectConfig, server, name string, args map[string]string, client httpDoer) (daemon.MCPPromptRenderResponse, error) {
	payload := daemon.MCPPromptRenderRequest{Server: server, Name: name, Arguments: args}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return daemon.MCPPromptRenderResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/v1/mcp/prompts/get", &body)
	if err != nil {
		return daemon.MCPPromptRenderResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return daemon.MCPPromptRenderResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemon.MCPPromptRenderResponse{}, fmt.Errorf("daemon MCP prompt: %s", httpStatusError(resp))
	}
	var rendered daemon.MCPPromptRenderResponse
	if err := json.NewDecoder(resp.Body).Decode(&rendered); err != nil {
		return daemon.MCPPromptRenderResponse{}, err
	}
	if rendered.Messages == nil {
		rendered.Messages = []daemon.MCPPromptMessage{}
	}
	return rendered, nil
}

func requestDaemonRecall(ctx context.Context, cfg connectConfig, request daemon.RecallRequest, client httpDoer) (daemon.RecallResponse, error) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(request); err != nil {
		return daemon.RecallResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/v1/recall", &body)
	if err != nil {
		return daemon.RecallResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return daemon.RecallResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemon.RecallResponse{}, fmt.Errorf("daemon recall: %s", httpStatusError(resp))
	}
	var recall daemon.RecallResponse
	if err := json.NewDecoder(resp.Body).Decode(&recall); err != nil {
		return daemon.RecallResponse{}, err
	}
	if recall.Hits == nil {
		recall.Hits = []daemon.RecallHit{}
	}
	return recall, nil
}

func writeDaemonToolCatalog(w io.Writer, tools []daemonToolCatalogEntry) {
	writeDaemonToolCatalogIndented(w, tools, "")
}

func writeDaemonToolCatalogIndented(w io.Writer, tools []daemonToolCatalogEntry, indent string) {
	if len(tools) == 0 {
		fmt.Fprintf(w, "%sNo daemon tools reported.\n", indent)
		return
	}
	for i, tool := range tools {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s%s\n", indent, tool.Name)
		if tool.Description != "" {
			fmt.Fprintf(w, "%s  description: %s\n", indent, oneLine(tool.Description))
		}
		if tool.RequiresPermission || tool.PermissionAction != "" || tool.PermissionTargetPreview != "" {
			fmt.Fprintf(w, "%s  permission: %s %s\n", indent, tool.PermissionAction, tool.PermissionTargetPreview)
		}
		if len(tool.Parameters) > 0 {
			fmt.Fprintf(w, "%s  parameters: %s\n", indent, compactJSON(tool.Parameters))
		}
	}
}

func writeDaemonSkillCatalog(w io.Writer, skills []daemon.SkillCatalogEntry) {
	writeDaemonSkillCatalogIndented(w, skills, "")
}

func writeDaemonSkillCatalogIndented(w io.Writer, skills []daemon.SkillCatalogEntry, indent string) {
	if len(skills) == 0 {
		fmt.Fprintf(w, "%sNo daemon skills reported.\n", indent)
		return
	}
	for i, skill := range skills {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s%s\n", indent, skill.Name)
		if skill.Description != "" {
			fmt.Fprintf(w, "%s  description: %s\n", indent, oneLine(skill.Description))
		}
	}
}

func writeDaemonRoleCatalog(w io.Writer, roles []daemon.RoleCatalogEntry) {
	writeDaemonRoleCatalogIndented(w, roles, "")
}

func writeDaemonRoleCatalogIndented(w io.Writer, roles []daemon.RoleCatalogEntry, indent string) {
	if len(roles) == 0 {
		fmt.Fprintf(w, "%sNo daemon roles reported.\n", indent)
		return
	}
	for i, role := range roles {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s%s\n", indent, role.Name)
		if role.Description != "" {
			fmt.Fprintf(w, "%s  description: %s\n", indent, oneLine(role.Description))
		}
		if role.Model != "" {
			fmt.Fprintf(w, "%s  model: %s\n", indent, role.Model)
		}
	}
}

func writeDaemonMemories(w io.Writer, memories []daemon.MemoryEntry) {
	writeDaemonMemoriesIndented(w, memories, "")
}

func writeDaemonMemoriesIndented(w io.Writer, memories []daemon.MemoryEntry, indent string) {
	if len(memories) == 0 {
		fmt.Fprintf(w, "%sNo daemon memories reported.\n", indent)
		return
	}
	for i, memory := range memories {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s%d. %s\n", indent, i+1, memory.ID)
		writeDaemonMemoryFields(w, memory, indent)
	}
}

func writeDaemonMemory(w io.Writer, memory daemon.MemoryEntry) {
	fmt.Fprintln(w, memory.ID)
	writeDaemonMemoryFields(w, memory, "")
}

func writeDaemonMemoryFields(w io.Writer, memory daemon.MemoryEntry, indent string) {
	if !memory.Timestamp.IsZero() {
		fmt.Fprintf(w, "%s  timestamp: %s\n", indent, memory.Timestamp.Format(time.RFC3339))
	}
	fmt.Fprintf(w, "%s  content: %s\n", indent, oneLine(memory.Content))
	if len(memory.Tags) > 0 {
		fmt.Fprintf(w, "%s  tags: %s\n", indent, strings.Join(memory.Tags, ", "))
	}
}

func writeDaemonPermissions(w io.Writer, permissions []daemon.PermissionGrant) {
	writeDaemonPermissionsIndented(w, permissions, "")
}

func writeDaemonPermissionsIndented(w io.Writer, permissions []daemon.PermissionGrant, indent string) {
	if len(permissions) == 0 {
		fmt.Fprintf(w, "%sNo daemon permissions remembered.\n", indent)
		return
	}
	for i, permission := range permissions {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s%d. %s\n", indent, i+1, permission.ID)
		writeDaemonPermissionFields(w, permission, indent)
	}
}

func writeDaemonPermission(w io.Writer, permission daemon.PermissionGrant) {
	fmt.Fprintln(w, permission.ID)
	writeDaemonPermissionFields(w, permission, "")
}

func writeDaemonPermissionFields(w io.Writer, permission daemon.PermissionGrant, indent string) {
	fmt.Fprintf(w, "%s  scope: %s\n", indent, permission.Scope)
	if permission.Owner != "" {
		fmt.Fprintf(w, "%s  owner: %s\n", indent, permission.Owner)
	}
	if permission.ClientID != "" {
		fmt.Fprintf(w, "%s  client_id: %s\n", indent, permission.ClientID)
	}
	if permission.SessionID != "" {
		fmt.Fprintf(w, "%s  session_id: %s\n", indent, permission.SessionID)
	}
	fmt.Fprintf(w, "%s  tool: %s\n", indent, permission.Tool)
	fmt.Fprintf(w, "%s  action: %s\n", indent, permission.Action)
	if permission.Target != "" {
		fmt.Fprintf(w, "%s  target: %s\n", indent, permission.Target)
	}
	if !permission.CreatedAt.IsZero() {
		fmt.Fprintf(w, "%s  created_at: %s\n", indent, permission.CreatedAt.Format(time.RFC3339))
	}
}

func writeDaemonMCPResourceCatalog(w io.Writer, resources []daemon.MCPResourceCatalogEntry) {
	writeDaemonMCPResourceCatalogIndented(w, resources, "")
}

func writeDaemonMCPResourceCatalogIndented(w io.Writer, resources []daemon.MCPResourceCatalogEntry, indent string) {
	if len(resources) == 0 {
		fmt.Fprintf(w, "%sNo daemon MCP resources reported.\n", indent)
		return
	}
	for i, resource := range resources {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s%s\n", indent, resource.URI)
		fmt.Fprintf(w, "%s  server: %s\n", indent, resource.Server)
		fmt.Fprintf(w, "%s  name: %s\n", indent, resource.Name)
		if resource.Title != "" {
			fmt.Fprintf(w, "%s  title: %s\n", indent, oneLine(resource.Title))
		}
		if resource.Description != "" {
			fmt.Fprintf(w, "%s  description: %s\n", indent, oneLine(resource.Description))
		}
		if resource.MIMEType != "" {
			fmt.Fprintf(w, "%s  mime_type: %s\n", indent, resource.MIMEType)
		}
		if resource.Size != nil {
			fmt.Fprintf(w, "%s  size: %d\n", indent, *resource.Size)
		}
		if len(resource.Annotations) > 0 {
			raw, err := json.Marshal(resource.Annotations)
			if err == nil {
				fmt.Fprintf(w, "%s  annotations: %s\n", indent, compactJSON(raw))
			}
		}
	}
}

func writeDaemonMCPPromptCatalog(w io.Writer, prompts []daemon.MCPPromptCatalogEntry) {
	writeDaemonMCPPromptCatalogIndented(w, prompts, "")
}

func writeDaemonMCPPromptCatalogIndented(w io.Writer, prompts []daemon.MCPPromptCatalogEntry, indent string) {
	if len(prompts) == 0 {
		fmt.Fprintf(w, "%sNo daemon MCP prompts reported.\n", indent)
		return
	}
	for i, prompt := range prompts {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s%s\n", indent, prompt.Name)
		fmt.Fprintf(w, "%s  server: %s\n", indent, prompt.Server)
		if prompt.Title != "" {
			fmt.Fprintf(w, "%s  title: %s\n", indent, oneLine(prompt.Title))
		}
		if prompt.Description != "" {
			fmt.Fprintf(w, "%s  description: %s\n", indent, oneLine(prompt.Description))
		}
		if len(prompt.Arguments) > 0 {
			fmt.Fprintf(w, "%s  arguments:\n", indent)
			for _, arg := range prompt.Arguments {
				required := ""
				if arg.Required {
					required = " required"
				}
				line := arg.Name + required
				if arg.Description != "" {
					line += " - " + oneLine(arg.Description)
				}
				fmt.Fprintf(w, "%s    %s\n", indent, line)
			}
		}
	}
}

func writeDaemonMCPResourceRead(w io.Writer, read daemon.MCPResourceReadResponse) {
	if len(read.Contents) == 0 {
		fmt.Fprintln(w, "No daemon MCP resource contents returned.")
		return
	}
	for i, item := range read.Contents {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, item.URI)
		fmt.Fprintf(w, "  server: %s\n", read.Server)
		fmt.Fprintf(w, "  requested_uri: %s\n", read.URI)
		if item.MIMEType != "" {
			fmt.Fprintf(w, "  mime_type: %s\n", item.MIMEType)
		}
		if len(item.Meta) > 0 {
			raw, err := json.Marshal(item.Meta)
			if err == nil {
				fmt.Fprintf(w, "  meta: %s\n", compactJSON(raw))
			}
		}
		switch {
		case item.Text != nil:
			fmt.Fprintln(w, "  text:")
			for _, line := range strings.Split(*item.Text, "\n") {
				fmt.Fprintf(w, "    %s\n", line)
			}
		case item.Blob != nil:
			fmt.Fprintf(w, "  blob: %s\n", *item.Blob)
		}
	}
}

func writeDaemonMCPPrompt(w io.Writer, rendered daemon.MCPPromptRenderResponse) {
	fmt.Fprintln(w, rendered.Name)
	fmt.Fprintf(w, "  server: %s\n", rendered.Server)
	if rendered.Description != "" {
		fmt.Fprintf(w, "  description: %s\n", oneLine(rendered.Description))
	}
	if len(rendered.Messages) == 0 {
		fmt.Fprintln(w, "  messages: []")
		return
	}
	fmt.Fprintln(w, "  messages:")
	for _, message := range rendered.Messages {
		fmt.Fprintf(w, "    - role: %s\n", message.Role)
		if text, ok := daemonPromptTextContent(message.Content); ok {
			fmt.Fprintln(w, "      text:")
			for _, line := range strings.Split(text, "\n") {
				fmt.Fprintf(w, "        %s\n", line)
			}
			continue
		}
		fmt.Fprintf(w, "      content: %s\n", compactJSON(message.Content))
	}
}

func writeDaemonRecallHits(w io.Writer, hits []daemon.RecallHit) {
	if len(hits) == 0 {
		fmt.Fprintln(w, "No recall hits.")
		return
	}
	for i, hit := range hits {
		if i > 0 {
			fmt.Fprintln(w)
		}
		timestamp := ""
		if !hit.Timestamp.IsZero() {
			timestamp = hit.Timestamp.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%d. %s#%d\n", i+1, hit.SessionID, hit.Index)
		if timestamp != "" {
			fmt.Fprintf(w, "  timestamp: %s\n", timestamp)
		}
		fmt.Fprintf(w, "  score: %.2f\n", hit.Score)
		fmt.Fprintf(w, "  snippet: %s\n", oneLine(hit.Snippet))
	}
}

func daemonPromptTextContent(raw json.RawMessage) (string, bool) {
	var content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &content); err != nil {
		return "", false
	}
	if content.Type != "text" {
		return "", false
	}
	return content.Text, true
}

func daemonHasCapability(status daemonStatus, capability string) bool {
	for _, existing := range status.Capabilities {
		if existing == capability {
			return true
		}
	}
	return false
}
