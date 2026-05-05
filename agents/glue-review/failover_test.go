package main

import (
	"strings"
	"testing"
)

func TestResolveProvidersSingle(t *testing.T) {
	t.Parallel()
	got, err := resolveProviders("nvidia")
	if err != nil {
		t.Fatalf("resolveProviders: %v", err)
	}
	if len(got) != 1 || got[0].name != "nvidia" || got[0].envName != "NVIDIA_API_KEY" {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveProvidersList(t *testing.T) {
	t.Parallel()
	got, err := resolveProviders("nvidia, openrouter ,gemini")
	if err != nil {
		t.Fatalf("resolveProviders: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}
	names := []string{got[0].name, got[1].name, got[2].name}
	want := []string{"nvidia", "openrouter", "gemini"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestResolveProvidersDefault(t *testing.T) {
	t.Parallel()
	got, _ := resolveProviders("")
	if len(got) != 1 || got[0].name != "nvidia" {
		t.Fatalf("default should be 'nvidia', got %+v", got)
	}
}

func TestResolveProvidersRejectsUnknown(t *testing.T) {
	t.Parallel()
	_, err := resolveProviders("nvidia,bogus")
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected unknown-provider error, got %v", err)
	}
}

func TestResolveProvidersTolerantOfBlanks(t *testing.T) {
	t.Parallel()
	got, err := resolveProviders("nvidia,,openrouter,")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2, got %+v", got)
	}
}

func TestProviderKeyAvailableReadsEnv(t *testing.T) {
	t.Setenv("NVIDIA_API_KEY", "k")
	t.Setenv("GEMINI_API_KEY", "")
	if !providerKeyAvailable("nvidia") {
		t.Fatal("nvidia should be available with key set")
	}
	if providerKeyAvailable("gemini") {
		t.Fatal("gemini should not be available with empty key")
	}
	if providerKeyAvailable("bogus") {
		t.Fatal("unknown provider must report unavailable")
	}
}
