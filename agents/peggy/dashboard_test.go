package peggy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
	filestore "github.com/erain/glue/stores/file"
)

func TestRunDashboardOnceRendersDaemonAndLocalState(t *testing.T) {
	var recallSeen bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("authorization = %q", got)
		}
		switch r.URL.Path {
		case "/v1/status":
			writeDashboardTestJSON(t, w, dashboardStatus{
				OK:           true,
				Version:      1,
				ActiveRuns:   1,
				ToolsCount:   2,
				Capabilities: []string{"diagnostics", "tools", "skills", "roles", "memories", "recall"},
			})
		case "/v1/diagnostics":
			writeDashboardTestJSON(t, w, daemon.DiagnosticResponse{
				OK:         true,
				Version:    1,
				ActiveRuns: 1,
				ToolsCount: 2,
				Runtime: daemon.DiagnosticInfo{
					Provider:  "gemini",
					Model:     "test-model",
					StoreType: "sqlite",
					StorePath: "/tmp/peggy.db",
				},
			})
		case "/v1/tools":
			writeDashboardTestJSON(t, w, dashboardToolCatalog{Tools: []dashboardTool{{
				Name:               "remember",
				Description:        "Persist memory",
				RequiresPermission: false,
			}}})
		case "/v1/skills":
			writeDashboardTestJSON(t, w, dashboardSkillCatalog{Skills: []daemon.SkillCatalogEntry{{
				Name:        "daily_plan",
				Description: "Plan the day",
			}}})
		case "/v1/roles":
			writeDashboardTestJSON(t, w, dashboardRoleCatalog{Roles: []daemon.RoleCatalogEntry{{
				Name:        "reviewer",
				Description: "Review changes",
				Model:       "fast",
			}}})
		case "/v1/memories":
			if got := r.URL.Query().Get("limit"); got != "10" {
				t.Fatalf("memory limit = %q", got)
			}
			writeDashboardTestJSON(t, w, daemon.MemoryCatalogResponse{Memories: []daemon.MemoryEntry{{
				ID:        "mem_1",
				Content:   "User's Aussie is named Inkblot.",
				Tags:      []string{"pet"},
				Timestamp: time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC),
			}}})
		case "/v1/recall":
			var req daemon.RecallRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode recall: %v", err)
			}
			if req.Query != "Aussie" || req.Limit != 5 {
				t.Fatalf("recall request = %+v", req)
			}
			recallSeen = true
			writeDashboardTestJSON(t, w, daemon.RecallResponse{Hits: []daemon.RecallHit{{
				SessionID: MemoriesSessionID,
				Index:     0,
				Role:      glue.MessageRoleAssistant,
				Snippet:   "User's <<Aussie>> is named Inkblot.",
				Timestamp: time.Date(2026, 5, 25, 12, 1, 0, 0, time.UTC),
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cfgPath := seedDashboardSessionConfig(t)
	var out, errOut strings.Builder
	code := Run(context.Background(), []string{
		"dashboard",
		"--once",
		"--config", cfgPath,
		"--base-url", ts.URL,
		"--token", "token",
		"--recall", "Aussie",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	if !recallSeen {
		t.Fatal("recall endpoint was not called")
	}
	html := out.String()
	for _, want := range []string{
		"Peggy Dashboard",
		"Daemon online",
		"gemini / test-model / sqlite /tmp/peggy.db",
		"daily_plan",
		"reviewer",
		"User&#39;s Aussie is named Inkblot.",
		"__memories__",
		"telegram:123",
		"remember",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard HTML missing %q:\n%s", want, html)
		}
	}
}

func TestRunDashboardRejectsNonLocalListenByDefault(t *testing.T) {
	var out, errOut strings.Builder
	code := Run(context.Background(), []string{"dashboard", "--listen", "0.0.0.0:0"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "refusing to bind non-loopback") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRunDashboardOnceRendersMissingDaemonState(t *testing.T) {
	cfgPath := seedDashboardSessionConfig(t)
	var out, errOut strings.Builder
	code := Run(context.Background(), []string{
		"dashboard",
		"--once",
		"--config", cfgPath,
		"--metadata", filepath.Join(t.TempDir(), "missing.json"),
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "daemon metadata unavailable") || !strings.Contains(out.String(), "Daemon offline") {
		t.Fatalf("dashboard HTML = %q", out.String())
	}
}

func seedDashboardSessionConfig(t *testing.T) string {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "sessions")
	store := filestore.New(storePath)
	base := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	if err := store.Save(context.Background(), "telegram:123", glue.SessionState{
		ID:        "telegram:123",
		CreatedAt: base,
		UpdatedAt: base.Add(time.Hour),
		Messages: []glue.Message{
			runnerTextMessage(glue.MessageRoleUser, "hello"),
			runnerTextMessage(glue.MessageRoleAssistant, "hi"),
		},
	}); err != nil {
		t.Fatalf("save session: %v", err)
	}
	return writeRunnerConfig(t, map[string]any{
		"provider": "bogus-provider",
		"store": map[string]any{
			"type": "file",
			"path": storePath,
		},
	})
}

func writeDashboardTestJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode JSON: %v", err)
	}
}
