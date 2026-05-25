package peggy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/erain/glue"
)

func TestExportMemoryBackupContainsOnlyCuratedMemories(t *testing.T) {
	p := newFileBackedPeggy(t)
	ctx := context.Background()
	mem, err := p.AddMemory(ctx, "User's Aussie is named Inkblot.", []string{"pet"})
	if err != nil {
		t.Fatalf("AddMemory: %v", err)
	}
	if err := p.store.Save(ctx, "ordinary", glue.SessionState{
		ID: "ordinary",
		Messages: []glue.Message{{
			Role:    glue.MessageRoleAssistant,
			Content: []glue.ContentPart{{Type: glue.ContentTypeText, Text: "ordinary conversation"}},
		}},
	}); err != nil {
		t.Fatalf("save ordinary session: %v", err)
	}

	backup, err := p.ExportMemoryBackup(ctx)
	if err != nil {
		t.Fatalf("ExportMemoryBackup: %v", err)
	}
	if backup.Version != MemoryBackupVersion || backup.Kind != MemoryBackupKind {
		t.Fatalf("backup metadata = %+v", backup)
	}
	if len(backup.Memories) != 1 || backup.Memories[0].ID != mem.ID {
		t.Fatalf("backup memories = %+v, want only %s", backup.Memories, mem.ID)
	}
	if strings.Contains(backup.Memories[0].Content, "ordinary") {
		t.Fatalf("ordinary session leaked into backup: %+v", backup.Memories)
	}
}

func TestImportMemoryBackupPreservesIDsAndSkipsDuplicateIDs(t *testing.T) {
	source := newFileBackedPeggy(t)
	ctx := context.Background()
	if _, err := source.AddMemory(ctx, "User prefers terse responses.", []string{"preference"}); err != nil {
		t.Fatalf("AddMemory: %v", err)
	}
	if _, err := source.AddMemory(ctx, "User's Aussie is named Inkblot.", []string{"pet"}); err != nil {
		t.Fatalf("AddMemory 2: %v", err)
	}
	backup, err := source.ExportMemoryBackup(ctx)
	if err != nil {
		t.Fatalf("ExportMemoryBackup: %v", err)
	}

	target := newFileBackedPeggy(t)
	report, err := target.ImportMemoryBackup(ctx, backup, MemoryImportOptions{})
	if err != nil {
		t.Fatalf("ImportMemoryBackup: %v", err)
	}
	if report.Imported != 2 || report.Skipped != 0 {
		t.Fatalf("report = %+v, want two imported", report)
	}
	imported, err := target.ListMemories(ctx)
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, mem := range imported {
		gotIDs[mem.ID] = true
	}
	for _, mem := range backup.Memories {
		if !gotIDs[mem.ID] {
			t.Fatalf("imported IDs = %+v, missing %s", imported, mem.ID)
		}
	}

	report, err = target.ImportMemoryBackup(ctx, backup, MemoryImportOptions{})
	if err != nil {
		t.Fatalf("second ImportMemoryBackup: %v", err)
	}
	if report.Imported != 0 || report.Skipped != 2 {
		t.Fatalf("second report = %+v, want duplicate skips", report)
	}
	for _, entry := range report.Entries {
		if entry.Status != "skipped" || entry.Reason != "duplicate_id" {
			t.Fatalf("entry = %+v, want duplicate_id skip", entry)
		}
	}
}

func TestImportMemoryBackupDryRunAndContentDuplicate(t *testing.T) {
	ctx := context.Background()
	target := newFileBackedPeggy(t)
	if _, err := target.AddMemory(ctx, "User likes green tea.", []string{"preference"}); err != nil {
		t.Fatalf("AddMemory: %v", err)
	}
	backup := MemoryBackup{
		Version: MemoryBackupVersion,
		Kind:    MemoryBackupKind,
		Memories: []Memory{{
			ID:        "mem_external",
			Content:   " user   likes GREEN tea. ",
			Timestamp: time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC),
		}},
	}
	report, err := target.ImportMemoryBackup(ctx, backup, MemoryImportOptions{DryRun: true})
	if err != nil {
		t.Fatalf("ImportMemoryBackup dry-run: %v", err)
	}
	if report.WouldImport != 0 || report.Imported != 0 || report.Skipped != 1 || report.Entries[0].Reason != "duplicate_content" {
		t.Fatalf("report = %+v, want duplicate content dry-run", report)
	}
	memories, err := target.ListMemories(ctx)
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(memories) != 1 {
		t.Fatalf("dry-run wrote memories: %+v", memories)
	}
}

func TestValidateMemoryBackupErrors(t *testing.T) {
	_, err := ValidateMemoryBackup(MemoryBackup{
		Version: MemoryBackupVersion,
		Kind:    MemoryBackupKind,
		Memories: []Memory{{
			ID:      "mem_bad",
			Content: "   ",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "content is required") {
		t.Fatalf("err = %v, want content validation", err)
	}

	_, err = ValidateMemoryBackup(MemoryBackup{
		Version: 99,
		Kind:    MemoryBackupKind,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported memory backup version") {
		t.Fatalf("err = %v, want version validation", err)
	}
}

func TestDecodeMemoryBackupAcceptsRawMemoryArray(t *testing.T) {
	ts := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	raw, err := json.Marshal([]Memory{{
		Content:   "User likes tea.",
		Timestamp: ts,
	}})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := DecodeMemoryBackup(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeMemoryBackup: %v", err)
	}
	memories, err := ValidateMemoryBackup(backup)
	if err != nil {
		t.Fatalf("ValidateMemoryBackup: %v", err)
	}
	if len(memories) != 1 || memories[0].ID == "" {
		t.Fatalf("memories = %+v, want synthesized id", memories)
	}
}
