package tui

import (
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aymanbagabas/go-osc52/v2"
)

// clipboardWriter is where OSC 52 copy sequences go. It points at os.Stderr:
// the same controlling tty bubbletea renders to, but a separate stream so the
// momentary escape doesn't interleave with the alt-screen frame buffer on
// stdout. The terminal interprets OSC 52 regardless of alt-screen state.
// Overridable in tests.
var clipboardWriter io.Writer = os.Stderr

// copyToClipboard asks the terminal to place s on the system clipboard via an
// OSC 52 sequence. Unlike shelling out to pbcopy/xclip, this works over SSH
// and needs no external binary — the trade-off is that the terminal must
// support OSC 52 (most modern ones do) and may cap very large payloads.
func copyToClipboard(s string) error {
	_, err := osc52.New(s).WriteTo(clipboardWriter)
	return err
}

// assistantTexts returns the raw markdown of every assistant reply in the
// transcript, oldest first. The raw text (transcriptItem.Text) is the clean
// source the user wants — not the glamour-rendered form they see on screen.
// Empty replies (tool-only turns) are skipped so /copy N numbering matches
// what the user perceives as a "reply."
func (m *Model) assistantTexts() []string {
	var out []string
	for i := range m.transcript {
		it := &m.transcript[i]
		if it.Kind == itemAssistant && strings.TrimSpace(it.Text) != "" {
			out = append(out, it.Text)
		}
	}
	return out
}

// pickCopyTarget resolves a /copy argument against the assistant replies,
// returning the text to copy and a human label. A non-empty errMsg means the
// selection failed and should be shown instead of copying. Pure so the
// numbering/bounds logic is unit-testable without a terminal.
func pickCopyTarget(texts []string, arg string) (text, label, errMsg string) {
	if len(texts) == 0 {
		return "", "", "nothing to copy yet — no assistant reply in this session."
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return texts[len(texts)-1], "last reply", ""
	}
	n, err := strconvAtoi(arg)
	if err != nil || n < 1 || n > len(texts) {
		return "", "", fmt.Sprintf("/copy: pick a reply between 1 and %d (got %q)", len(texts), arg)
	}
	return texts[n-1], fmt.Sprintf("reply %d", n), ""
}

// runSlashCopy handles `/copy [N]` (and Ctrl+Y, which calls it with no arg):
// copies the raw markdown of the most recent assistant reply, or the Nth
// reply (1-based) when N is given, to the system clipboard.
func (m *Model) runSlashCopy(arg string) (tea.Model, tea.Cmd) {
	text, label, errMsg := pickCopyTarget(m.assistantTexts(), arg)
	if errMsg != "" {
		m.appendSystem(errMsg)
	} else if err := copyToClipboard(text); err != nil {
		m.appendSystem("copy failed: " + err.Error())
	} else {
		m.appendSystem(fmt.Sprintf("copied %s (%d chars) to clipboard.", label, len([]rune(text))))
	}
	m.rerender()
	return m, nil
}
