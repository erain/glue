package cli

import (
	"flag"
	"testing"
)

func TestRegisterStandardFlags_Defaults(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	get := RegisterStandardFlags(fs, nil)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	got := get()
	want := StandardFlagDefaults
	if got != want {
		t.Fatalf("defaults round-trip:\n got %+v\nwant %+v", got, want)
	}
}

func TestRegisterStandardFlags_ParsesArgv(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	get := RegisterStandardFlags(fs, nil)
	args := []string{
		"--provider", "openrouter,gemini",
		"--model", "openrouter/free",
		"--id", "review",
		"--store", "/tmp/sessions",
		"--work", "/repo",
		"--max-turns", "8",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	got := get()
	want := StandardConfig{
		Provider: "openrouter,gemini",
		Model:    "openrouter/free",
		ID:       "review",
		Store:    "/tmp/sessions",
		Work:     "/repo",
		MaxTurns: 8,
	}
	if got != want {
		t.Fatalf("parsed argv:\n got %+v\nwant %+v", got, want)
	}
}

func TestRegisterStandardFlags_DefaultsOverride(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	get := RegisterStandardFlags(fs, &StandardConfig{
		Provider: "openrouter",
		Store:    ".myagent/sessions",
		MaxTurns: 16,
	})
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	got := get()
	if got.Provider != "openrouter" {
		t.Fatalf("override provider: got %q", got.Provider)
	}
	if got.Store != ".myagent/sessions" {
		t.Fatalf("override store: got %q", got.Store)
	}
	if got.MaxTurns != 16 {
		t.Fatalf("override max-turns: got %d", got.MaxTurns)
	}
	// Unset override fields fall back to package defaults.
	if got.ID != StandardFlagDefaults.ID {
		t.Fatalf("non-overridden ID drifted: got %q", got.ID)
	}
}
