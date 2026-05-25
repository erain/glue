package peggy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/erain/glue"
)

const (
	// MemoryBackupKind identifies Peggy curated-memory backup files.
	MemoryBackupKind = "peggy.memories"

	// MemoryBackupVersion is the JSON backup format version.
	MemoryBackupVersion = 1
)

// MemoryBackup is the JSON envelope used for curated-memory export/import.
// It intentionally contains only the __memories__ session, not ordinary
// conversation history.
type MemoryBackup struct {
	Version    int       `json:"version"`
	Kind       string    `json:"kind"`
	ExportedAt time.Time `json:"exported_at"`
	Memories   []Memory  `json:"memories"`
}

// MemoryImportOptions controls backup import behavior.
type MemoryImportOptions struct {
	DryRun bool
}

// MemoryImportReport summarizes a backup import or dry-run validation.
type MemoryImportReport struct {
	DryRun      bool                `json:"dry_run"`
	WouldImport int                 `json:"would_import,omitempty"`
	Imported    int                 `json:"imported"`
	Skipped     int                 `json:"skipped"`
	Entries     []MemoryImportEntry `json:"entries"`
}

// MemoryImportEntry records the import decision for one backup memory.
type MemoryImportEntry struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// ExportMemoryBackup returns a JSON-ready curated-memory backup.
func (p *Peggy) ExportMemoryBackup(ctx context.Context) (MemoryBackup, error) {
	memories, err := p.ListMemories(ctx)
	if err != nil {
		return MemoryBackup{}, err
	}
	sort.SliceStable(memories, func(i, j int) bool {
		if memories[i].Timestamp.Equal(memories[j].Timestamp) {
			return memories[i].ID < memories[j].ID
		}
		return memories[i].Timestamp.Before(memories[j].Timestamp)
	})
	return MemoryBackup{
		Version:    MemoryBackupVersion,
		Kind:       MemoryBackupKind,
		ExportedAt: time.Now().UTC(),
		Memories:   memories,
	}, nil
}

// DecodeMemoryBackup reads either the Peggy backup envelope or the older
// raw []Memory JSON shape produced by `peggy memories --json`.
func DecodeMemoryBackup(r io.Reader) (MemoryBackup, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return MemoryBackup{}, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return MemoryBackup{}, errors.New("memory backup is empty")
	}
	switch bytes.TrimSpace(data)[0] {
	case '[':
		var memories []Memory
		if err := decodeStrictJSON(data, &memories); err != nil {
			return MemoryBackup{}, fmt.Errorf("decode memory list: %w", err)
		}
		return MemoryBackup{
			Version:  MemoryBackupVersion,
			Kind:     MemoryBackupKind,
			Memories: memories,
		}, nil
	default:
		var backup MemoryBackup
		if err := decodeStrictJSON(data, &backup); err != nil {
			return MemoryBackup{}, fmt.Errorf("decode memory backup: %w", err)
		}
		return backup, nil
	}
}

// ImportMemoryBackup validates and imports curated memories. Existing
// memories are never overwritten; duplicates by stable id or normalized
// content are skipped.
func (p *Peggy) ImportMemoryBackup(ctx context.Context, backup MemoryBackup, opts MemoryImportOptions) (MemoryImportReport, error) {
	if p == nil || p.store == nil {
		return MemoryImportReport{}, errors.New("peggy: ImportMemoryBackup: not initialised")
	}
	memories, err := ValidateMemoryBackup(backup)
	if err != nil {
		return MemoryImportReport{}, err
	}

	memoryWriteMu.Lock()
	defer memoryWriteMu.Unlock()

	state, _, err := p.store.Load(ctx, MemoriesSessionID)
	if err != nil {
		return MemoryImportReport{}, fmt.Errorf("peggy: load memories: %w", err)
	}
	now := time.Now().UTC()
	if state.ID == "" {
		state.ID = MemoriesSessionID
	}
	if state.Version == 0 {
		state.Version = glue.SessionStateVersion
	}
	if state.CreatedAt.IsZero() {
		state.CreatedAt = firstMemoryTimestamp(memories, now)
	}

	existingIDs := map[string]struct{}{}
	existingContents := map[string]struct{}{}
	for _, msg := range state.Messages {
		mem, ok := memoryFromMessage(msg)
		if !ok {
			continue
		}
		existingIDs[mem.ID] = struct{}{}
		existingContents[normalizedMemoryContent(mem.Content)] = struct{}{}
	}

	report := MemoryImportReport{DryRun: opts.DryRun, Entries: make([]MemoryImportEntry, 0, len(memories))}
	for _, mem := range memories {
		entry := MemoryImportEntry{ID: mem.ID}
		contentKey := normalizedMemoryContent(mem.Content)
		if _, exists := existingIDs[mem.ID]; exists {
			entry.Status = "skipped"
			entry.Reason = "duplicate_id"
			report.Skipped++
			report.Entries = append(report.Entries, entry)
			continue
		}
		if _, exists := existingContents[contentKey]; exists {
			entry.Status = "skipped"
			entry.Reason = "duplicate_content"
			report.Skipped++
			report.Entries = append(report.Entries, entry)
			continue
		}
		if opts.DryRun {
			entry.Status = "would_import"
			report.WouldImport++
		} else {
			entry.Status = "imported"
			report.Imported++
			state.Messages = append(state.Messages, memoryMessage(mem))
		}
		existingIDs[mem.ID] = struct{}{}
		existingContents[contentKey] = struct{}{}
		report.Entries = append(report.Entries, entry)
	}

	if !opts.DryRun && report.Imported > 0 {
		state.UpdatedAt = now
		if err := p.store.Save(ctx, MemoriesSessionID, state); err != nil {
			return MemoryImportReport{}, fmt.Errorf("peggy: save memories: %w", err)
		}
	}
	return report, nil
}

// ValidateMemoryBackup validates backup shape and returns normalized memories.
func ValidateMemoryBackup(backup MemoryBackup) ([]Memory, error) {
	if backup.Version != MemoryBackupVersion {
		return nil, fmt.Errorf("unsupported memory backup version %d", backup.Version)
	}
	if backup.Kind != MemoryBackupKind {
		return nil, fmt.Errorf("unsupported memory backup kind %q", backup.Kind)
	}
	memories := make([]Memory, 0, len(backup.Memories))
	seenIDs := map[string]struct{}{}
	for i, mem := range backup.Memories {
		mem.Content = strings.TrimSpace(mem.Content)
		if mem.Content == "" {
			return nil, fmt.Errorf("memory %d: content is required", i)
		}
		if mem.Timestamp.IsZero() {
			return nil, fmt.Errorf("memory %d: timestamp is required", i)
		}
		mem.Timestamp = mem.Timestamp.UTC()
		mem.ID = strings.TrimSpace(mem.ID)
		if mem.ID == "" {
			mem.ID = memoryID(mem.Timestamp, mem.Content)
		}
		if _, ok := seenIDs[mem.ID]; ok {
			return nil, fmt.Errorf("memory %d: duplicate id %q", i, mem.ID)
		}
		seenIDs[mem.ID] = struct{}{}
		mem.Tags = cleanMemoryTags(mem.Tags)
		memories = append(memories, mem)
	}
	return memories, nil
}

func decodeStrictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func firstMemoryTimestamp(memories []Memory, fallback time.Time) time.Time {
	first := fallback
	found := false
	for _, mem := range memories {
		if mem.Timestamp.IsZero() {
			continue
		}
		if !found || mem.Timestamp.Before(first) {
			first = mem.Timestamp
			found = true
		}
	}
	return first
}

func memoryMessage(mem Memory) glue.Message {
	metadata := map[string]any{
		"id":     mem.ID,
		"memory": true,
	}
	if len(mem.Tags) > 0 {
		metadata["tags"] = append([]string(nil), mem.Tags...)
	}
	return glue.Message{
		Role:      glue.MessageRoleAssistant,
		Content:   []glue.ContentPart{{Type: glue.ContentTypeText, Text: mem.Content}},
		Metadata:  metadata,
		CreatedAt: mem.Timestamp,
	}
}

func normalizedMemoryContent(content string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(content))), " ")
}

func cleanMemoryTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	seen := map[string]struct{}{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
