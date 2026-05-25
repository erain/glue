// Package daemon serves local Glue sessions over the ADR-0010 HTTP+SSE
// protocol.
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erain/glue"
)

const protocolVersion = 1

const defaultPermissionTimeout = 10 * time.Minute

// Host supplies sessions to a daemon Server.
type Host interface {
	Session(ctx context.Context, id string, options ...glue.SessionOption) (*glue.Session, error)
}

// ToolCatalogHost is optionally implemented by hosts that can expose the
// provider-visible tool surface without starting a run.
type ToolCatalogHost interface {
	ToolCatalog() []glue.ToolSpec
}

// SkillCatalogHost is optionally implemented by hosts that can expose
// reusable skills without starting a run.
type SkillCatalogHost interface {
	SkillCatalog(context.Context) ([]SkillCatalogEntry, error)
}

// RoleCatalogHost is optionally implemented by hosts that can expose
// reusable roles without starting a run.
type RoleCatalogHost interface {
	RoleCatalog(context.Context) ([]RoleCatalogEntry, error)
}

// MCPResourceCatalogHost is optionally implemented by hosts that can expose
// MCP resource metadata without starting a run.
type MCPResourceCatalogHost interface {
	MCPResourceCatalog(context.Context) ([]MCPResourceCatalogEntry, error)
}

// MCPPromptCatalogHost is optionally implemented by hosts that can expose MCP
// prompt metadata without starting a run.
type MCPPromptCatalogHost interface {
	MCPPromptCatalog(context.Context) ([]MCPPromptCatalogEntry, error)
}

// MCPResourceReaderHost is optionally implemented by hosts that can read one
// MCP resource without starting a run.
type MCPResourceReaderHost interface {
	MCPReadResource(context.Context, MCPReadResourceRequest) (MCPResourceReadResponse, error)
}

// MCPPromptRendererHost is optionally implemented by hosts that can render one
// MCP prompt without starting a run.
type MCPPromptRendererHost interface {
	MCPRenderPrompt(context.Context, MCPPromptRenderRequest) (MCPPromptRenderResponse, error)
}

// RecallHost is optionally implemented by hosts that can search stored session
// history without starting a run.
type RecallHost interface {
	RecallSearch(context.Context, RecallRequest) (RecallResponse, error)
}

// MemoryCatalogHost is optionally implemented by hosts that can expose curated
// memory records without starting a run.
type MemoryCatalogHost interface {
	MemoryCatalog(context.Context, MemoryCatalogRequest) (MemoryCatalogResponse, error)
}

// SkillCatalogEntry describes one reusable skill advertised by a host.
type SkillCatalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// RoleCatalogEntry describes one reusable role advertised by a host.
type RoleCatalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
}

// MCPResourceCatalogEntry describes one MCP resource advertised by a host.
type MCPResourceCatalogEntry struct {
	Server      string         `json:"server"`
	URI         string         `json:"uri"`
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	MIMEType    string         `json:"mime_type,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Size        *int64         `json:"size,omitempty"`
}

// MCPPromptCatalogEntry describes one MCP prompt advertised by a host.
type MCPPromptCatalogEntry struct {
	Server      string                     `json:"server"`
	Name        string                     `json:"name"`
	Title       string                     `json:"title,omitempty"`
	Description string                     `json:"description,omitempty"`
	Arguments   []MCPPromptCatalogArgument `json:"arguments,omitempty"`
}

// MCPPromptCatalogArgument describes one argument accepted by an MCP prompt.
type MCPPromptCatalogArgument struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// MCPReadResourceRequest selects one resource URI from one configured server.
type MCPReadResourceRequest struct {
	Server string `json:"server"`
	URI    string `json:"uri"`
}

// MCPResourceReadResponse contains the contents returned by an MCP
// resources/read request.
type MCPResourceReadResponse struct {
	Server   string               `json:"server"`
	URI      string               `json:"uri"`
	Contents []MCPResourceContent `json:"contents"`
}

// MCPResourceContent is one text or blob content item returned from a resource.
type MCPResourceContent struct {
	URI      string         `json:"uri"`
	MIMEType string         `json:"mime_type,omitempty"`
	Text     *string        `json:"text,omitempty"`
	Blob     *string        `json:"blob,omitempty"`
	Meta     map[string]any `json:"_meta,omitempty"`
}

// MCPPromptRenderRequest selects one prompt from one configured server.
type MCPPromptRenderRequest struct {
	Server    string            `json:"server"`
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

// MCPPromptRenderResponse contains the messages returned by an MCP prompts/get
// request.
type MCPPromptRenderResponse struct {
	Server      string             `json:"server"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Messages    []MCPPromptMessage `json:"messages"`
}

// MCPPromptMessage is one rendered prompt message.
type MCPPromptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// RecallRequest searches stored session history.
type RecallRequest struct {
	Query        string `json:"query"`
	Limit        int    `json:"limit,omitempty"`
	MemoriesOnly bool   `json:"memories_only,omitempty"`
}

// RecallHit is one stored session search result returned by a recall-capable
// daemon host.
type RecallHit struct {
	SessionID string           `json:"session_id"`
	Index     int              `json:"index"`
	Role      glue.MessageRole `json:"role,omitempty"`
	Snippet   string           `json:"snippet"`
	Score     float64          `json:"score"`
	Timestamp time.Time        `json:"timestamp,omitempty"`
}

// RecallResponse contains stored session search hits.
type RecallResponse struct {
	Hits []RecallHit `json:"hits"`
}

// MemoryCatalogRequest configures a memory catalog request.
type MemoryCatalogRequest struct {
	Limit int
}

// MemoryEntry describes one curated host memory.
type MemoryEntry struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Tags      []string  `json:"tags,omitempty"`
	Timestamp time.Time `json:"timestamp,omitempty"`
}

// MemoryCatalogResponse contains curated host memories.
type MemoryCatalogResponse struct {
	Memories []MemoryEntry `json:"memories"`
}

// Options configures [New].
type Options struct {
	// Host is required. A *glue.Agent satisfies this interface.
	Host Host

	// Token is required for every route except /v1/health.
	Token string

	// PermissionPolicy, when non-nil, can allow, deny, or defer
	// side-effecting tool permission requests before the daemon emits a
	// permission_request event. The daemon package remains channel-blind:
	// hosts that need channel/client policy decide from the supplied context.
	PermissionPolicy PermissionPolicy

	// Now supplies event timestamps. Nil uses time.Now.
	Now func() time.Time

	// NewID returns ids for runs and events. Nil uses crypto/rand.
	NewID func(prefix string) string

	// PermissionTimeout caps how long a side-effecting tool waits for an
	// HTTP decision. Zero uses a conservative default.
	PermissionTimeout time.Duration
}

// PermissionPolicy optionally decides side-effecting tool permission requests
// before the daemon asks the run owner over HTTP.
type PermissionPolicy interface {
	DecidePermission(context.Context, PermissionContext, glue.PermissionRequest) (PermissionPolicyDecision, error)
}

// PermissionPolicyFunc adapts a function into a [PermissionPolicy].
type PermissionPolicyFunc func(context.Context, PermissionContext, glue.PermissionRequest) (PermissionPolicyDecision, error)

// DecidePermission implements [PermissionPolicy].
func (f PermissionPolicyFunc) DecidePermission(ctx context.Context, info PermissionContext, req glue.PermissionRequest) (PermissionPolicyDecision, error) {
	if f == nil {
		return PermissionPolicyDecision{}, nil
	}
	return f(ctx, info, req)
}

// PermissionContext describes the daemon run that owns a permission request.
type PermissionContext struct {
	RunID     string
	SessionID string
	ClientID  string
}

// PermissionPolicyAction is the host policy outcome for one permission
// request.
type PermissionPolicyAction int

const (
	// PermissionPolicyPrompt keeps the existing daemon behavior: use cached
	// remembered decisions or ask the owning client over HTTP.
	PermissionPolicyPrompt PermissionPolicyAction = iota
	// PermissionPolicyAllow allows the side effect without asking the client.
	PermissionPolicyAllow
	// PermissionPolicyDeny denies the side effect without asking the client.
	PermissionPolicyDeny
)

// PermissionPolicyDecision is returned by [PermissionPolicy].
type PermissionPolicyDecision struct {
	Action      PermissionPolicyAction
	Reason      string
	RememberFor glue.RememberScope
}

// Server is an http.Handler for the local daemon protocol.
type Server struct {
	host              Host
	token             string
	permissionPolicy  PermissionPolicy
	now               func() time.Time
	newID             func(prefix string) string
	permissionTimeout time.Duration

	mu   sync.Mutex
	runs map[string]*run

	permMu        sync.Mutex
	sessionAllows map[string]struct{}
	targetAllows  map[string]struct{}
	foreverAllows map[string]struct{}
}

// EventEnvelope is the JSON payload sent in each SSE data frame.
type EventEnvelope struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Seq       int64     `json:"seq"`
	RunID     string    `json:"run_id"`
	SessionID string    `json:"session_id"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"`
	Payload   any       `json:"payload,omitempty"`
}

type protocolError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type errorResponse struct {
	Error protocolError `json:"error"`
}

type startRunRequest struct {
	Text      string            `json:"text"`
	Skill     string            `json:"skill,omitempty"`
	Arguments map[string]string `json:"arguments,omitempty"`
	ClientID  string            `json:"client_id,omitempty"`
	Role      string            `json:"role,omitempty"`
	Model     string            `json:"model,omitempty"`
	MaxTurns  int               `json:"max_turns,omitempty"`
	Options   map[string]any    `json:"options,omitempty"`
}

type startRunResponse struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	EventsURL string `json:"events_url"`
}

type permissionRequestPayload struct {
	PermissionID string                 `json:"permission_id"`
	Request      glue.PermissionRequest `json:"request"`
	ExpiresAt    time.Time              `json:"expires_at"`
}

type permissionDecisionRequest struct {
	Allow       bool   `json:"allow"`
	Reason      string `json:"reason,omitempty"`
	RememberFor string `json:"remember_for,omitempty"`
}

type permissionDecisionResponse struct {
	PermissionID string `json:"permission_id"`
	Accepted     bool   `json:"accepted"`
}

type statusResponse struct {
	OK           bool     `json:"ok"`
	Version      int      `json:"version"`
	ActiveRuns   int      `json:"active_runs"`
	ToolsCount   int      `json:"tools_count"`
	Capabilities []string `json:"capabilities"`
}

type toolCatalogResponse struct {
	Tools []toolCatalogEntry `json:"tools"`
}

type skillCatalogResponse struct {
	Skills []SkillCatalogEntry `json:"skills"`
}

type roleCatalogResponse struct {
	Roles []RoleCatalogEntry `json:"roles"`
}

type mcpResourceCatalogResponse struct {
	Resources []MCPResourceCatalogEntry `json:"resources"`
}

type mcpPromptCatalogResponse struct {
	Prompts []MCPPromptCatalogEntry `json:"prompts"`
}

type toolCatalogEntry struct {
	Name                    string          `json:"name"`
	Description             string          `json:"description,omitempty"`
	Parameters              json.RawMessage `json:"parameters,omitempty"`
	RequiresPermission      bool            `json:"requires_permission"`
	PermissionAction        string          `json:"permission_action,omitempty"`
	PermissionTargetPreview string          `json:"permission_target_preview,omitempty"`
}

// New constructs a daemon Server.
func New(opts Options) (*Server, error) {
	if opts.Host == nil {
		return nil, errors.New("daemon: Host is required")
	}
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		return nil, errors.New("daemon: Token is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	newID := opts.NewID
	if newID == nil {
		newID = randomID
	}
	permissionTimeout := opts.PermissionTimeout
	if permissionTimeout <= 0 {
		permissionTimeout = defaultPermissionTimeout
	}
	return &Server{
		host:              opts.Host,
		token:             token,
		permissionPolicy:  opts.PermissionPolicy,
		now:               now,
		newID:             newID,
		permissionTimeout: permissionTimeout,
		runs:              map[string]*run{},
		sessionAllows:     map[string]struct{}{},
		targetAllows:      map[string]struct{}{},
		foreverAllows:     map[string]struct{}{},
	}, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/health" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": protocolVersion})
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token", false)
		return
	}

	if r.URL.Path == "/v1/status" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleStatus(w, r)
		return
	}

	if r.URL.Path == "/v1/tools" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleTools(w, r)
		return
	}

	if r.URL.Path == "/v1/skills" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleSkills(w, r)
		return
	}

	if r.URL.Path == "/v1/roles" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleRoles(w, r)
		return
	}

	if r.URL.Path == "/v1/recall" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleRecall(w, r)
		return
	}

	if r.URL.Path == "/v1/memories" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleMemories(w, r)
		return
	}

	if r.URL.Path == "/v1/mcp/resources" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleMCPResources(w, r)
		return
	}

	if r.URL.Path == "/v1/mcp/resources/read" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleMCPReadResource(w, r)
		return
	}

	if r.URL.Path == "/v1/mcp/prompts" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleMCPPrompts(w, r)
		return
	}

	if r.URL.Path == "/v1/mcp/prompts/get" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleMCPRenderPrompt(w, r)
		return
	}

	if sessionID, ok := parseStartRunPath(r.URL.Path); ok {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleStartRun(w, r, sessionID)
		return
	}

	if runID, ok := parseRunEventsPath(r.URL.Path); ok {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleRunEvents(w, r, runID)
		return
	}

	if runID, permissionID, ok := parsePermissionDecisionPath(r.URL.Path); ok {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handlePermissionDecision(w, r, runID, permissionID)
		return
	}

	if runID, ok := parseRunPath(r.URL.Path); ok {
		if r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		s.handleCancelRun(w, r, runID)
		return
	}

	writeError(w, http.StatusNotFound, "not_found", "route not found", false)
}

func (s *Server) authorized(r *http.Request) bool {
	want := "Bearer " + s.token
	got := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	toolsCount := 0
	if host, ok := s.host.(ToolCatalogHost); ok {
		toolsCount = len(host.ToolCatalog())
	}
	capabilities := []string{
		"runs",
		"events",
		"permissions",
		"tools",
		"status",
	}
	if _, ok := s.host.(MCPResourceCatalogHost); ok {
		capabilities = append(capabilities, "mcp_resources")
	}
	if _, ok := s.host.(MCPPromptCatalogHost); ok {
		capabilities = append(capabilities, "mcp_prompts")
	}
	if _, ok := s.host.(MCPResourceReaderHost); ok {
		capabilities = append(capabilities, "mcp_resource_read")
	}
	if _, ok := s.host.(MCPPromptRendererHost); ok {
		capabilities = append(capabilities, "mcp_prompt_get")
	}
	if _, ok := s.host.(SkillCatalogHost); ok {
		capabilities = append(capabilities, "skills")
	}
	if _, ok := s.host.(RoleCatalogHost); ok {
		capabilities = append(capabilities, "roles")
	}
	if _, ok := s.host.(RecallHost); ok {
		capabilities = append(capabilities, "recall")
	}
	if _, ok := s.host.(MemoryCatalogHost); ok {
		capabilities = append(capabilities, "memories")
	}
	writeJSON(w, http.StatusOK, statusResponse{
		OK:           true,
		Version:      protocolVersion,
		ActiveRuns:   s.activeRunCount(),
		ToolsCount:   toolsCount,
		Capabilities: capabilities,
	})
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req startRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", false)
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	req.Skill = strings.TrimSpace(req.Skill)
	if req.Text == "" && req.Skill == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "text or skill is required", false)
		return
	}
	if req.Text != "" && req.Skill != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "choose only one of text or skill", false)
		return
	}
	session, err := s.host.Session(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	run := s.newRun(sessionID, req.ClientID, cancel)
	go s.executeRun(ctx, run, session, req)

	writeJSON(w, http.StatusCreated, startRunResponse{
		RunID:     run.id,
		SessionID: sessionID,
		EventsURL: "/v1/runs/" + url.PathEscape(run.id) + "/events",
	})
}

func (s *Server) handleTools(w http.ResponseWriter, _ *http.Request) {
	host, ok := s.host.(ToolCatalogHost)
	if !ok {
		writeJSON(w, http.StatusOK, toolCatalogResponse{Tools: []toolCatalogEntry{}})
		return
	}
	specs := host.ToolCatalog()
	tools := make([]toolCatalogEntry, 0, len(specs))
	for _, spec := range specs {
		entry := toolCatalogEntry{
			Name:               spec.Name,
			Description:        spec.Description,
			Parameters:         append(json.RawMessage(nil), spec.Parameters...),
			RequiresPermission: spec.RequiresPermission,
			PermissionAction:   spec.PermissionAction,
		}
		if spec.PermissionTarget != nil {
			entry.PermissionTargetPreview = spec.PermissionTarget(glue.ToolCall{Name: spec.Name})
		}
		tools = append(tools, entry)
	}
	writeJSON(w, http.StatusOK, toolCatalogResponse{Tools: tools})
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	host, ok := s.host.(SkillCatalogHost)
	if !ok {
		writeJSON(w, http.StatusOK, skillCatalogResponse{Skills: []SkillCatalogEntry{}})
		return
	}
	skills, err := host.SkillCatalog(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	if skills == nil {
		skills = []SkillCatalogEntry{}
	}
	writeJSON(w, http.StatusOK, skillCatalogResponse{Skills: skills})
}

func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request) {
	host, ok := s.host.(RoleCatalogHost)
	if !ok {
		writeJSON(w, http.StatusOK, roleCatalogResponse{Roles: []RoleCatalogEntry{}})
		return
	}
	roles, err := host.RoleCatalog(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	if roles == nil {
		roles = []RoleCatalogEntry{}
	}
	writeJSON(w, http.StatusOK, roleCatalogResponse{Roles: roles})
}

func (s *Server) handleRecall(w http.ResponseWriter, r *http.Request) {
	host, ok := s.host.(RecallHost)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "recall is not supported by this host", false)
		return
	}
	var req RecallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", false)
		return
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "query is required", false)
		return
	}
	if req.Limit < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must be non-negative", false)
		return
	}
	hits, err := host.RecallSearch(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	if hits.Hits == nil {
		hits.Hits = []RecallHit{}
	}
	writeJSON(w, http.StatusOK, RecallResponse{Hits: hits.Hits})
}

func (s *Server) handleMemories(w http.ResponseWriter, r *http.Request) {
	limit, err := nonNegativeIntQuery(r, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), false)
		return
	}
	host, ok := s.host.(MemoryCatalogHost)
	if !ok {
		writeJSON(w, http.StatusOK, MemoryCatalogResponse{Memories: []MemoryEntry{}})
		return
	}
	catalog, err := host.MemoryCatalog(r.Context(), MemoryCatalogRequest{Limit: limit})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	if catalog.Memories == nil {
		catalog.Memories = []MemoryEntry{}
	}
	if limit > 0 && len(catalog.Memories) > limit {
		catalog.Memories = catalog.Memories[:limit]
	}
	writeJSON(w, http.StatusOK, catalog)
}

func (s *Server) handleMCPResources(w http.ResponseWriter, r *http.Request) {
	host, ok := s.host.(MCPResourceCatalogHost)
	if !ok {
		writeJSON(w, http.StatusOK, mcpResourceCatalogResponse{Resources: []MCPResourceCatalogEntry{}})
		return
	}
	resources, err := host.MCPResourceCatalog(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	if resources == nil {
		resources = []MCPResourceCatalogEntry{}
	}
	writeJSON(w, http.StatusOK, mcpResourceCatalogResponse{Resources: resources})
}

func (s *Server) handleMCPPrompts(w http.ResponseWriter, r *http.Request) {
	host, ok := s.host.(MCPPromptCatalogHost)
	if !ok {
		writeJSON(w, http.StatusOK, mcpPromptCatalogResponse{Prompts: []MCPPromptCatalogEntry{}})
		return
	}
	prompts, err := host.MCPPromptCatalog(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	if prompts == nil {
		prompts = []MCPPromptCatalogEntry{}
	}
	writeJSON(w, http.StatusOK, mcpPromptCatalogResponse{Prompts: prompts})
}

func (s *Server) handleMCPReadResource(w http.ResponseWriter, r *http.Request) {
	host, ok := s.host.(MCPResourceReaderHost)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "MCP resource reads are not supported by this host", false)
		return
	}
	var req MCPReadResourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", false)
		return
	}
	req.Server = strings.TrimSpace(req.Server)
	req.URI = strings.TrimSpace(req.URI)
	if req.Server == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "server is required", false)
		return
	}
	if req.URI == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "uri is required", false)
		return
	}
	read, err := host.MCPReadResource(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	if read.Contents == nil {
		read.Contents = []MCPResourceContent{}
	}
	writeJSON(w, http.StatusOK, read)
}

func (s *Server) handleMCPRenderPrompt(w http.ResponseWriter, r *http.Request) {
	host, ok := s.host.(MCPPromptRendererHost)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "MCP prompt rendering is not supported by this host", false)
		return
	}
	var req MCPPromptRenderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", false)
		return
	}
	req.Server = strings.TrimSpace(req.Server)
	req.Name = strings.TrimSpace(req.Name)
	if req.Server == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "server is required", false)
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name is required", false)
		return
	}
	rendered, err := host.MCPRenderPrompt(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), false)
		return
	}
	if rendered.Messages == nil {
		rendered.Messages = []MCPPromptMessage{}
	}
	writeJSON(w, http.StatusOK, rendered)
}

func (s *Server) executeRun(ctx context.Context, r *run, session *glue.Session, req startRunRequest) {
	startPayload := map[string]any{"client_id": req.ClientID}
	if req.Skill != "" {
		startPayload["skill"] = req.Skill
	}
	r.emit("run_start", startPayload)
	options := []glue.PromptOption{
		glue.WithPermission(runPermission{server: s, run: r}),
		glue.WithEvents(func(event glue.Event) {
			r.emit(string(event.Type), event)
		}),
	}
	if strings.TrimSpace(req.Role) != "" {
		options = append(options, glue.WithRole(req.Role))
	}
	if strings.TrimSpace(req.Model) != "" {
		options = append(options, glue.WithModel(req.Model))
	}
	if req.MaxTurns > 0 {
		options = append(options, glue.WithMaxTurns(req.MaxTurns))
	}
	if len(req.Options) > 0 {
		options = append(options, glue.WithProviderOptions(req.Options))
	}

	var (
		result glue.PromptResult
		err    error
	)
	if req.Skill != "" {
		result, err = session.Skill(ctx, req.Skill, req.Arguments, options...)
	} else {
		result, err = session.Prompt(ctx, req.Text, options...)
	}
	if err != nil {
		r.emit("run_error", map[string]any{"error": errorFor(err)})
		r.finish()
		return
	}
	r.emit("run_done", map[string]any{
		"text":         result.Text,
		"message":      result.Message,
		"new_messages": result.NewMessages,
	})
	r.finish()
}

func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request, runID string) {
	run := s.getRun(runID)
	if run == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found", false)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported", false)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	next := 0
	for {
		events, done, notify := run.eventsFrom(next)
		for _, event := range events {
			if err := writeSSE(w, event); err != nil {
				run.cancel()
				return
			}
			next++
			flusher.Flush()
		}
		if done {
			return
		}
		select {
		case <-notify:
		case <-r.Context().Done():
			run.cancel()
			return
		}
	}
}

func (s *Server) handleCancelRun(w http.ResponseWriter, _ *http.Request, runID string) {
	run := s.getRun(runID)
	if run == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found", false)
		return
	}
	run.cancel()
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": runID, "canceled": true})
}

func (s *Server) handlePermissionDecision(w http.ResponseWriter, r *http.Request, runID, permissionID string) {
	run := s.getRun(runID)
	if run == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found", false)
		return
	}
	if clientID := r.Header.Get("X-Glue-Client-ID"); clientID != "" && run.clientID != "" && clientID != run.clientID {
		writeError(w, http.StatusForbidden, "forbidden", "permission decision belongs to another client", false)
		return
	}
	var req permissionDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", false)
		return
	}
	scope, ok := parseRememberScope(req.RememberFor)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid remember_for", false)
		return
	}
	decision := glue.PermissionDecision{
		Allow:       req.Allow,
		Reason:      req.Reason,
		RememberFor: scope,
	}
	if !decision.Allow && strings.TrimSpace(decision.Reason) == "" {
		decision.Reason = "permission denied by daemon client"
	}
	if !run.resolvePermission(permissionID, decision) {
		writeError(w, http.StatusNotFound, "not_found", "permission request not found", false)
		return
	}
	writeJSON(w, http.StatusOK, permissionDecisionResponse{PermissionID: permissionID, Accepted: true})
}

func (s *Server) newRun(sessionID, clientID string, cancel context.CancelFunc) *run {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		id := s.newID("run")
		if _, exists := s.runs[id]; exists {
			continue
		}
		r := &run{
			id:        id,
			sessionID: sessionID,
			clientID:  clientID,
			cancel:    cancel,
			now:       s.now,
			newID:     s.newID,
			notify:    make(chan struct{}),
			pending:   map[string]*pendingPermission{},
		}
		s.runs[id] = r
		return r
	}
}

func (s *Server) getRun(id string) *run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id]
}

func (s *Server) activeRunCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	for _, run := range s.runs {
		if run != nil && !run.isDone() {
			count++
		}
	}
	return count
}

func parseStartRunPath(path string) (string, bool) {
	const prefix = "/v1/sessions/"
	const suffix = "/runs"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if raw == "" || strings.Contains(raw, "/") {
		return "", false
	}
	id, err := url.PathUnescape(raw)
	return id, err == nil && id != ""
}

func parseRunEventsPath(path string) (string, bool) {
	const prefix = "/v1/runs/"
	const suffix = "/events"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if raw == "" || strings.Contains(raw, "/") {
		return "", false
	}
	id, err := url.PathUnescape(raw)
	return id, err == nil && id != ""
}

func parsePermissionDecisionPath(path string) (runID, permissionID string, ok bool) {
	const prefix = "/v1/runs/"
	const middle = "/permissions/"
	const suffix = "/decision"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	rawRunID, rawPermissionID, found := strings.Cut(rest, middle)
	if !found || rawRunID == "" || rawPermissionID == "" || strings.Contains(rawRunID, "/") || strings.Contains(rawPermissionID, "/") {
		return "", "", false
	}
	runID, err := url.PathUnescape(rawRunID)
	if err != nil || runID == "" {
		return "", "", false
	}
	permissionID, err = url.PathUnescape(rawPermissionID)
	if err != nil || permissionID == "" {
		return "", "", false
	}
	return runID, permissionID, true
}

func parseRunPath(path string) (string, bool) {
	const prefix = "/v1/runs/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	raw := strings.TrimPrefix(path, prefix)
	if raw == "" || strings.Contains(raw, "/") {
		return "", false
	}
	id, err := url.PathUnescape(raw)
	return id, err == nil && id != ""
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeJSON(w, status, errorResponse{Error: protocolError{Code: code, Message: message, Retryable: retryable}})
}

func nonNegativeIntQuery(r *http.Request, name string) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be non-negative", name)
	}
	return n, nil
}

func writeSSE(w http.ResponseWriter, event EventEnvelope) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %s\n", event.ID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event.Type); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return nil
}

func errorFor(err error) protocolError {
	code := "internal"
	if errors.Is(err, context.Canceled) {
		code = "canceled"
	}
	return protocolError{Code: code, Message: err.Error(), Retryable: false}
}

type runPermission struct {
	server *Server
	run    *run
}

func (p runPermission) Decide(ctx context.Context, req glue.PermissionRequest) (glue.PermissionDecision, error) {
	return p.server.decidePermission(ctx, p.run, req)
}

func (s *Server) decidePermission(ctx context.Context, run *run, req glue.PermissionRequest) (glue.PermissionDecision, error) {
	if decision, handled, err := s.applyPermissionPolicy(ctx, run, req); err != nil || handled {
		return decision, err
	}
	if decision, ok := s.cachedPermission(run, req); ok {
		return decision, nil
	}

	var permissionID string
	var pending *pendingPermission
	for {
		permissionID = s.newID("perm")
		pending = &pendingPermission{
			id:   permissionID,
			done: make(chan glue.PermissionDecision, 1),
		}
		if run.addPermission(pending) {
			break
		}
	}

	expiresAt := s.now().UTC().Add(s.permissionTimeout)
	run.emit("permission_request", permissionRequestPayload{
		PermissionID: permissionID,
		Request:      req,
		ExpiresAt:    expiresAt,
	})

	timer := time.NewTimer(s.permissionTimeout)
	defer timer.Stop()
	select {
	case decision := <-pending.done:
		s.rememberPermission(run, req, decision)
		return decision, nil
	case <-timer.C:
		if !run.expirePermission(permissionID, pending) {
			decision := <-pending.done
			s.rememberPermission(run, req, decision)
			return decision, nil
		}
		return glue.PermissionDecision{Allow: false, Reason: "permission denied: daemon permission request timed out"}, nil
	case <-ctx.Done():
		if !run.expirePermission(permissionID, pending) {
			decision := <-pending.done
			s.rememberPermission(run, req, decision)
			return decision, nil
		}
		return glue.PermissionDecision{}, ctx.Err()
	}
}

func (s *Server) applyPermissionPolicy(ctx context.Context, run *run, req glue.PermissionRequest) (glue.PermissionDecision, bool, error) {
	if s.permissionPolicy == nil {
		return glue.PermissionDecision{}, false, nil
	}
	info := PermissionContext{}
	if run != nil {
		info.RunID = run.id
		info.SessionID = run.sessionID
		info.ClientID = run.clientID
	}
	if info.SessionID == "" {
		info.SessionID = req.SessionID
	}
	policyDecision, err := s.permissionPolicy.DecidePermission(ctx, info, req)
	if err != nil {
		return glue.PermissionDecision{}, true, err
	}
	switch policyDecision.Action {
	case PermissionPolicyPrompt:
		return glue.PermissionDecision{}, false, nil
	case PermissionPolicyAllow:
		return glue.PermissionDecision{
			Allow:       true,
			Reason:      policyDecision.Reason,
			RememberFor: policyDecision.RememberFor,
		}, true, nil
	case PermissionPolicyDeny:
		reason := strings.TrimSpace(policyDecision.Reason)
		if reason == "" {
			reason = "permission denied by daemon policy"
		}
		return glue.PermissionDecision{Allow: false, Reason: reason}, true, nil
	default:
		return glue.PermissionDecision{}, true, fmt.Errorf("daemon: invalid permission policy action %d", policyDecision.Action)
	}
}

func (s *Server) cachedPermission(run *run, req glue.PermissionRequest) (glue.PermissionDecision, bool) {
	sessionKey := permissionSessionKey(run, req)
	targetKey := permissionTargetKey(run, req)
	foreverKey := permissionForeverKey(run, req)
	s.permMu.Lock()
	defer s.permMu.Unlock()
	if _, ok := s.foreverAllows[foreverKey]; ok {
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberForever}, true
	}
	if _, ok := s.sessionAllows[sessionKey]; ok {
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSession}, true
	}
	if _, ok := s.targetAllows[targetKey]; ok {
		return glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSessionTarget}, true
	}
	return glue.PermissionDecision{}, false
}

func (s *Server) rememberPermission(run *run, req glue.PermissionRequest, decision glue.PermissionDecision) {
	if !decision.Allow {
		return
	}
	s.permMu.Lock()
	defer s.permMu.Unlock()
	switch decision.RememberFor {
	case glue.RememberSession:
		s.sessionAllows[permissionSessionKey(run, req)] = struct{}{}
	case glue.RememberSessionTarget:
		s.targetAllows[permissionTargetKey(run, req)] = struct{}{}
	case glue.RememberForever:
		s.foreverAllows[permissionForeverKey(run, req)] = struct{}{}
	}
}

func permissionSessionKey(run *run, req glue.PermissionRequest) string {
	return permissionOwnerKey(run, req) + "\x00" + req.SessionID + "\x00" + req.Tool + "\x00" + req.Action
}

func permissionTargetKey(run *run, req glue.PermissionRequest) string {
	return permissionSessionKey(run, req) + "\x00" + req.Target
}

func permissionForeverKey(run *run, req glue.PermissionRequest) string {
	return permissionOwnerKey(run, req) + "\x00" + req.Tool + "\x00" + req.Action + "\x00" + req.Target
}

func permissionOwnerKey(run *run, req glue.PermissionRequest) string {
	if run != nil && strings.TrimSpace(run.clientID) != "" {
		return "client:" + strings.TrimSpace(run.clientID)
	}
	if run != nil && strings.TrimSpace(run.sessionID) != "" {
		return "session:" + strings.TrimSpace(run.sessionID)
	}
	return "session:" + strings.TrimSpace(req.SessionID)
}

func parseRememberScope(raw string) (glue.RememberScope, bool) {
	switch strings.TrimSpace(raw) {
	case "", "never":
		return glue.RememberNever, true
	case "session":
		return glue.RememberSession, true
	case "session_target":
		return glue.RememberSessionTarget, true
	case "forever":
		return glue.RememberForever, true
	default:
		return glue.RememberNever, false
	}
}

func randomID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
