package tui

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	toolsfs "github.com/erain/glue/tools/fs"
)

// atMaxFiles caps the workspace walk so a huge repo doesn't slow the
// TUI's startup. 5000 is plenty for any glue-scale project; pi caps at
// 10k and matchers like fzf scale well past that, but our matching is
// linear today.
const atMaxFiles = 5000

// atPickerVisibleRows is how many matches the popup shows at a time.
// Cursor navigation scrolls within the matches list, not the window.
const atPickerVisibleRows = 8

// atPicker is the file autocomplete state. It is opened by Update when
// the user's input ends in a `@<word>` (with a whitespace boundary
// before the `@`) and closed when that condition no longer holds.
type atPicker struct {
	files   []string // workspace-relative paths, sorted alphabetically
	query   string   // current text after the `@`
	matches []int    // indices into files of currently matching entries
	cursor  int      // index into matches (NOT files)
}

// newAtPicker walks the workspace once and caches the file list. The
// caller decides when to refresh (today: never — a per-session
// snapshot is fine for typical sessions; /clear could re-walk in a
// follow-up).
func newAtPicker(workDir string) *atPicker {
	files := walkWorkspace(workDir)
	p := &atPicker{files: files}
	p.refilter("")
	return p
}

// walkWorkspace returns the workspace-relative file paths under workDir,
// skipping `.git`, secret-shaped paths from tools/fs.Default(), and
// symlinks. Capped at atMaxFiles entries.
func walkWorkspace(workDir string) []string {
	if workDir == "" {
		return nil
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return nil
	}
	bl := toolsfs.Default()
	out := make([]string, 0, 1024)
	_ = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == ".gstack" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if blocked, _ := bl.Match(rel); blocked {
			return nil
		}
		out = append(out, rel)
		if len(out) >= atMaxFiles {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// refilter recomputes the matches list against a new query. Matching is
// case-insensitive substring over the full workspace-relative path,
// ranked by (1) basename-start hit, (2) earliest position of the match,
// (3) shortest path, (4) alphabetical — so "ut" pulls util.go to the
// top instead of node_modules/x/y/something_with_ut_in_middle.go.
func (p *atPicker) refilter(query string) {
	p.query = query
	lower := strings.ToLower(query)
	type rank struct {
		idx           int
		baseStartHit  bool
		earliestMatch int
		pathLen       int
	}
	var ranks []rank
	for i, f := range p.files {
		flow := strings.ToLower(f)
		if lower == "" {
			ranks = append(ranks, rank{idx: i, baseStartHit: false, earliestMatch: 0, pathLen: len(f)})
			continue
		}
		pos := strings.Index(flow, lower)
		if pos < 0 {
			continue
		}
		base := f
		if i := strings.LastIndexByte(f, '/'); i >= 0 {
			base = f[i+1:]
		}
		baseStartHit := strings.HasPrefix(strings.ToLower(base), lower)
		ranks = append(ranks, rank{idx: i, baseStartHit: baseStartHit, earliestMatch: pos, pathLen: len(f)})
	}
	sort.Slice(ranks, func(i, j int) bool {
		a, b := ranks[i], ranks[j]
		if a.baseStartHit != b.baseStartHit {
			return a.baseStartHit
		}
		if a.earliestMatch != b.earliestMatch {
			return a.earliestMatch < b.earliestMatch
		}
		if a.pathLen != b.pathLen {
			return a.pathLen < b.pathLen
		}
		return p.files[a.idx] < p.files[b.idx]
	})
	p.matches = make([]int, len(ranks))
	for i, r := range ranks {
		p.matches[i] = r.idx
	}
	if p.cursor >= len(p.matches) {
		p.cursor = 0
	}
}

func (p *atPicker) up() {
	if p == nil || len(p.matches) == 0 {
		return
	}
	if p.cursor > 0 {
		p.cursor--
	}
}

func (p *atPicker) down() {
	if p == nil || len(p.matches) == 0 {
		return
	}
	if p.cursor < len(p.matches)-1 {
		p.cursor++
	}
}

// selected returns the workspace-relative path of the currently-focused
// match, or "" if there are no matches.
func (p *atPicker) selected() string {
	if p == nil || len(p.matches) == 0 {
		return ""
	}
	idx := p.matches[p.cursor]
	if idx < 0 || idx >= len(p.files) {
		return ""
	}
	return p.files[idx]
}

// detectAtTrigger inspects the input (single-line view) and returns the
// `@<query>` text the user has typed, or ok=false if the input doesn't
// end in a `@<word>` with a whitespace boundary before the `@`.
//
// "@@..." (an escaped literal @, per the @-mention expansion rules in
// cmd/glue/atmentions) does not open the picker.
func detectAtTrigger(input string) (query string, ok bool) {
	if input == "" {
		return "", false
	}
	// Find the last whitespace-delimited "word". A whitespace boundary is
	// a space, tab, or newline.
	last := input
	if i := strings.LastIndexAny(input, " \t\n"); i >= 0 {
		last = input[i+1:]
	}
	if !strings.HasPrefix(last, "@") {
		return "", false
	}
	// @@ is the escape syntax — don't trigger.
	if strings.HasPrefix(last, "@@") {
		return "", false
	}
	q := last[1:]
	// If the query has whitespace internal — shouldn't happen because we
	// already split on whitespace — bail.
	for _, r := range q {
		if unicode.IsSpace(r) {
			return "", false
		}
	}
	return q, true
}

// applyAtSelection rewrites input so the trailing `@<query>` is
// replaced with `@<path> ` (note trailing space). Used when the user
// presses Enter or Tab in the picker.
func applyAtSelection(input, path string) string {
	if i := strings.LastIndexAny(input, " \t\n"); i >= 0 {
		return input[:i+1] + "@" + path + " "
	}
	return "@" + path + " "
}

// removeAtToken strips the trailing `@<query>` from the input. Used
// when the user dismisses the picker via Esc.
func removeAtToken(input string) string {
	if i := strings.LastIndexAny(input, " \t\n"); i >= 0 {
		return input[:i+1]
	}
	return ""
}

// ------ rendering ------

var (
	atBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Padding(0, 1)
	atRow = lipgloss.NewStyle().Foreground(inkSoft)
	atSel = lipgloss.NewStyle().Foreground(accent).Bold(true)
)

// renderAtPicker returns the popup string positioned above the input.
// width is the full TUI width.
func renderAtPicker(p *atPicker, width int) string {
	if p == nil {
		return ""
	}
	w := width - 4
	if w > inputMaxBoxWidth {
		w = inputMaxBoxWidth
	}
	if w < 30 {
		w = 30
	}

	var b strings.Builder
	head := "Files matching @"
	if p.query != "" {
		head += p.query
	}
	b.WriteString(pickerTitle.Render(" " + head + " "))
	b.WriteByte('\n')

	if len(p.matches) == 0 {
		b.WriteString(atRow.Render("  (no matches)"))
	} else {
		// Scroll window: keep the cursor visible. Show at most
		// atPickerVisibleRows rows.
		start := 0
		if p.cursor >= atPickerVisibleRows {
			start = p.cursor - atPickerVisibleRows + 1
		}
		end := start + atPickerVisibleRows
		if end > len(p.matches) {
			end = len(p.matches)
		}
		for i := start; i < end; i++ {
			row := p.files[p.matches[i]]
			row = truncate(row, w-6)
			if i == p.cursor {
				b.WriteString(atSel.Render("› " + row))
			} else {
				b.WriteString(atRow.Render("  " + row))
			}
			if i < end-1 {
				b.WriteByte('\n')
			}
		}
	}
	b.WriteByte('\n')
	b.WriteString(keyHint.Render("  ↑/↓ navigate · Tab/Enter insert · Esc cancel"))

	rendered := atBox.Width(w).Render(b.String())
	if w < width {
		return lipgloss.PlaceHorizontal(width, lipgloss.Center, rendered)
	}
	return rendered
}
