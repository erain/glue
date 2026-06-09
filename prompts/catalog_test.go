package prompts

import (
	"strings"
	"testing"
	"testing/fstest"
)

func newTestFS() fstest.MapFS {
	return fstest.MapFS{
		"prompts/v1.md":    {Data: []byte("v1 body\n")},
		"prompts/v2.md":    {Data: []byte("v2 body\n\n")},
		"prompts/notes/x":  {Data: []byte("nested non-md")},
		"prompts/skip.txt": {Data: []byte("not markdown")},
	}
}

func TestCatalog_GetKnownVersion(t *testing.T) {
	cat, err := NewCatalog(newTestFS(), "prompts", "v1")
	if err != nil {
		t.Fatal(err)
	}
	body, err := cat.Get("v2")
	if err != nil {
		t.Fatal(err)
	}
	if body != "v2 body\n" {
		t.Fatalf("expected single-trailing-newline body, got %q", body)
	}
}

func TestCatalog_GetEmptyUsesDefault(t *testing.T) {
	cat, err := NewCatalog(newTestFS(), "prompts", "v2")
	if err != nil {
		t.Fatal(err)
	}
	body, err := cat.Get("")
	if err != nil {
		t.Fatal(err)
	}
	if body != "v2 body\n" {
		t.Fatalf("expected default body, got %q", body)
	}
}

func TestCatalog_GetUnknownLists(t *testing.T) {
	cat, err := NewCatalog(newTestFS(), "prompts", "v1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = cat.Get("v3")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, `unknown version "v3"`) {
		t.Fatalf("error should name version: %v", err)
	}
	if !strings.Contains(msg, "v1") || !strings.Contains(msg, "v2") {
		t.Fatalf("error should list available versions: %v", err)
	}
}

func TestCatalog_NewCatalog_DefaultMustExist(t *testing.T) {
	_, err := NewCatalog(newTestFS(), "prompts", "v9")
	if err == nil {
		t.Fatal("expected error when default version missing")
	}
	if !strings.Contains(err.Error(), `default version "v9" not found`) {
		t.Fatalf("unexpected message: %v", err)
	}
}

func TestCatalog_NewCatalog_EmptyDefault(t *testing.T) {
	_, err := NewCatalog(newTestFS(), "prompts", "")
	if err == nil {
		t.Fatal("expected error when default version empty")
	}
}

func TestCatalog_NewCatalog_MissingDir(t *testing.T) {
	_, err := NewCatalog(newTestFS(), "no-such-dir", "v1")
	if err == nil {
		t.Fatal("expected error when dir missing")
	}
}

func TestCatalog_VersionsAndDefault(t *testing.T) {
	cat, err := NewCatalog(newTestFS(), "prompts", "v1")
	if err != nil {
		t.Fatal(err)
	}
	got := cat.Versions()
	if len(got) != 2 || got[0] != "v1" || got[1] != "v2" {
		t.Fatalf("Versions() = %v; want [v1 v2]", got)
	}
	if cat.Default() != "v1" {
		t.Fatalf("Default() = %q; want v1", cat.Default())
	}
}

func TestCatalog_Nil(t *testing.T) {
	var c *Catalog
	if _, err := c.Get("v1"); err == nil {
		t.Fatal("nil catalog Get should error")
	}
	if c.Versions() != nil {
		t.Fatal("nil catalog Versions should be nil")
	}
	if c.Default() != "" {
		t.Fatal("nil catalog Default should be empty")
	}
}
