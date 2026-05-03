package file

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
)

func newTempStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir())
}

func TestStoreLoadMissingReturnsNotFound(t *testing.T) {
	t.Parallel()

	s := newTempStore(t)
	got, found, err := s.Load(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Fatalf("found=true for missing session, state=%#v", got)
	}
}

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()

	s := newTempStore(t)
	state := glue.SessionState{
		ID: "dev",
		Messages: []glue.Message{
			{Role: glue.MessageRoleUser, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "hi"}}},
			{Role: glue.MessageRoleAssistant, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "hello"}}},
		},
		Metadata: map[string]any{"k": "v"},
	}
	if err := s.Save(context.Background(), "dev", state); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, found, err := s.Load(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("found=false after Save")
	}
	if got.Version != glue.SessionStateVersion || got.ID != "dev" {
		t.Fatalf("version=%d id=%q, want %d/dev", got.Version, got.ID, glue.SessionStateVersion)
	}
	if len(got.Messages) != 2 || got.Messages[0].Content[0].Text != "hi" {
		t.Fatalf("messages = %#v, want hi/hello", got.Messages)
	}
	if got.Metadata["k"] != "v" {
		t.Fatalf("metadata = %#v, want k=v", got.Metadata)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not set: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}
}

func TestStoreSaveOverwritesExisting(t *testing.T) {
	t.Parallel()

	s := newTempStore(t)
	if err := s.Save(context.Background(), "x", glue.SessionState{ID: "x", Messages: []glue.Message{{Role: glue.MessageRoleUser, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "first"}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(context.Background(), "x", glue.SessionState{ID: "x", Messages: []glue.Message{{Role: glue.MessageRoleUser, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "second"}}}}}); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Load(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Messages[0].Content[0].Text != "second" {
		t.Fatalf("text = %q, want second", got.Messages[0].Content[0].Text)
	}
}

func TestStoreDeleteIdempotent(t *testing.T) {
	t.Parallel()

	s := newTempStore(t)
	if err := s.Delete(context.Background(), "ghost"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
	if err := s.Save(context.Background(), "x", glue.SessionState{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), "x"); err != nil {
		t.Fatalf("Delete existing: %v", err)
	}
	_, found, err := s.Load(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("found=true after Delete")
	}
	if err := s.Delete(context.Background(), "x"); err != nil {
		t.Fatalf("Delete after delete: %v", err)
	}
}

func TestStoreCorruptedJSONReturnsError(t *testing.T) {
	t.Parallel()

	s := newTempStore(t)
	path, err := s.Path("broken")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Load(context.Background(), "broken")
	if err == nil || !strings.Contains(err.Error(), "load") {
		t.Fatalf("err = %v, want load error", err)
	}
}

func TestStoreURLEscapesSpecialIDs(t *testing.T) {
	t.Parallel()

	s := newTempStore(t)
	id := "weird id/with*chars"
	if err := s.Save(context.Background(), id, glue.SessionState{ID: id}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, err := s.Path(id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(filepath.Base(path), "/*") {
		t.Fatalf("path %q contains unescaped chars", path)
	}
	got, found, err := s.Load(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("Load missed escaped id: found=%v err=%v", found, err)
	}
	if got.ID != id {
		t.Fatalf("ID = %q, want %q", got.ID, id)
	}
}

func TestStoreEmptyDirOrIDErrors(t *testing.T) {
	t.Parallel()

	if _, err := New("").Path("x"); err == nil {
		t.Fatal("expected error for empty dir")
	}
	if _, err := newTempStore(t).Path(""); err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestStoreSavesValidJSON(t *testing.T) {
	t.Parallel()

	s := newTempStore(t)
	if err := s.Save(context.Background(), "x", glue.SessionState{ID: "x", Messages: []glue.Message{{Role: glue.MessageRoleUser, Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "hi"}}}}}); err != nil {
		t.Fatal(err)
	}
	path, _ := s.Path("x")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("on-disk JSON invalid: %s", data)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("file does not end with newline: %q", data[len(data)-3:])
	}
}

func TestStoreSetsTimestampsWhenZero(t *testing.T) {
	t.Parallel()

	s := newTempStore(t)
	before := time.Now().UTC().Add(-time.Second)
	if err := s.Save(context.Background(), "x", glue.SessionState{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Load(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.Before(before) || got.UpdatedAt.Before(before) {
		t.Fatalf("timestamps = %v / %v, want >= %v", got.CreatedAt, got.UpdatedAt, before)
	}
}
