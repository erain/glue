package tui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// slashPicker is the inline `/command` autocomplete popup. It opens when
// the input is a bare slash token (starts with `/`, no whitespace yet) and
// closes once the user types a space (entering args) or clears the slash.
// It mirrors atPicker: a popup ABOVE the input box, not a modal.
type slashPicker struct {
	specs   []slashSpec // sorted alphabetically by Name
	query   string      // text typed after the `/`
	matches []int       // indices into specs of currently matching entries
	cursor  int         // index into matches (NOT specs)
}

// newSlashPicker builds the picker from the canonical command list.
func newSlashPicker() *slashPicker {
	specs := slashSpecs()
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	p := &slashPicker{specs: specs}
	p.refilter("")
	return p
}

// detectSlashTrigger reports whether the input is a command being typed —
// a leading `/` with no whitespace after it yet. The returned query is the
// text after the `/` (possibly empty, i.e. a bare `/` shows every command).
// Once the user types a space the picker should close, because they are now
// entering an argument (e.g. `/model gpt-…`).
func detectSlashTrigger(input string) (query string, ok bool) {
	if !strings.HasPrefix(input, "/") {
		return "", false
	}
	rest := input[1:]
	if strings.ContainsAny(rest, " \t\n") {
		return "", false
	}
	return rest, true
}

// refilter recomputes matches against a new query. Matching is a
// case-insensitive prefix over the command name or any alias; an empty
// query matches everything. Results stay in the picker's alphabetical
// order, so the list never reshuffles under the cursor as you type.
func (p *slashPicker) refilter(query string) {
	p.query = query
	lower := strings.ToLower(query)
	p.matches = p.matches[:0]
	for i, s := range p.specs {
		if lower == "" || specMatches(s, lower) {
			p.matches = append(p.matches, i)
		}
	}
	if p.cursor >= len(p.matches) {
		p.cursor = 0
	}
}

// specMatches reports whether a command name or alias begins with q
// (q already lower-cased).
func specMatches(s slashSpec, q string) bool {
	if strings.HasPrefix(s.Name, q) {
		return true
	}
	for _, a := range s.Aliases {
		if strings.HasPrefix(a, q) {
			return true
		}
	}
	return false
}

func (p *slashPicker) up() {
	if p == nil || len(p.matches) == 0 {
		return
	}
	if p.cursor > 0 {
		p.cursor--
	}
}

func (p *slashPicker) down() {
	if p == nil || len(p.matches) == 0 {
		return
	}
	if p.cursor < len(p.matches)-1 {
		p.cursor++
	}
}

// selected returns the currently-focused command spec, or ok=false when
// there are no matches.
func (p *slashPicker) selected() (slashSpec, bool) {
	if p == nil || len(p.matches) == 0 {
		return slashSpec{}, false
	}
	idx := p.matches[p.cursor]
	if idx < 0 || idx >= len(p.specs) {
		return slashSpec{}, false
	}
	return p.specs[idx], true
}

// exactMatch reports whether the typed query already equals the selected
// command's name (case-insensitive) — i.e. the user has fully typed it, so
// Enter should run it rather than re-complete.
func (p *slashPicker) exactMatch() bool {
	s, ok := p.selected()
	if !ok {
		return false
	}
	return strings.EqualFold(p.query, s.Name)
}

// applySlashSelection returns the input rewritten to the chosen command
// with a trailing space, ready for an argument (e.g. `/model `). The slash
// command always occupies the whole input, so the previous text is
// replaced entirely.
func applySlashSelection(name string) string {
	return "/" + name + " "
}

// ------ rendering ------

// renderSlashPicker returns the popup string positioned above the input.
// width is the full TUI width. It mirrors renderAtPicker's frame so the two
// autocompletes look identical.
func renderSlashPicker(p *slashPicker, width int) string {
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
	head := "Commands"
	if p.query != "" {
		head = "Commands matching /" + p.query
	}
	b.WriteString(pickerTitle.Render(" " + head + " "))
	b.WriteByte('\n')

	if len(p.matches) == 0 {
		b.WriteString(atRow.Render("  (no matching command)"))
	} else {
		start := 0
		if p.cursor >= atPickerVisibleRows {
			start = p.cursor - atPickerVisibleRows + 1
		}
		end := start + atPickerVisibleRows
		if end > len(p.matches) {
			end = len(p.matches)
		}
		for i := start; i < end; i++ {
			s := p.specs[p.matches[i]]
			row := truncate(s.display()+"  —  "+s.Desc, w-6)
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
	b.WriteString(keyHint.Render("  ↑/↓ navigate · Tab complete · Enter run · Esc cancel"))

	rendered := atBox.Width(w).Render(b.String())
	if w < width {
		return lipgloss.PlaceHorizontal(width, lipgloss.Center, rendered)
	}
	return rendered
}
