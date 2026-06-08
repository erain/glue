package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/erain/glue"
)

// treeNode is one session in the rendered lineage. Children are sorted
// by CreatedAt so the visual tree matches the chronological branching
// order.
type treeNode struct {
	Summary  glue.SessionSummary
	ParentID string // empty for the root
	AtIndex  int    // forked at message index in parent
	Children []*treeNode
}

// buildSessionTree returns the lineage anchored at the current session.
// Strategy: list every session, follow the current session's parent
// pointers up to a root, then DFS down from that root through any
// session whose parent matches a node we've already placed. Sessions
// with no relationship to the current lineage are skipped.
//
// The first return is the root of the rendered tree; the second is the
// flat DFS order used by the picker for ↑/↓ navigation; the third is
// the index of the current session within that flat order, or -1 if
// the current session id is absent from the store.
func buildSessionTree(
	ctx context.Context,
	agent *glue.Agent,
	store glue.Store,
	currentID string,
) (*treeNode, []*treeNode, int, error) {
	if agent == nil || store == nil {
		return nil, nil, -1, fmt.Errorf("buildSessionTree: agent and store required")
	}
	// 1. List all sessions metadata-only and load their state in a
	// second pass to read the parent pointers. We bound the pass by
	// the listing limit (default 200) since deeply-nested trees are
	// rare and this view is interactive.
	summaries, err := agent.ListSessions(ctx, glue.ListSessionsOptions{Limit: 500})
	if err != nil {
		return nil, nil, -1, err
	}

	type loaded struct {
		summary glue.SessionSummary
		parent  string
		atIdx   int
	}
	all := make(map[string]loaded, len(summaries))
	for _, s := range summaries {
		state, ok, err := store.Load(ctx, s.ID)
		if err != nil || !ok {
			continue
		}
		parent, idx, _ := glue.SessionParent(state)
		all[s.ID] = loaded{summary: s, parent: parent, atIdx: idx}
	}

	// 2. Walk up from currentID to find the root.
	rootID := currentID
	for {
		l, ok := all[rootID]
		if !ok || l.parent == "" {
			break
		}
		if _, ok := all[l.parent]; !ok {
			// Parent not in the store; treat this node as a root for
			// rendering purposes.
			break
		}
		rootID = l.parent
	}

	// 3. DFS down from root. Build the tree.
	nodes := make(map[string]*treeNode, len(all))
	var build func(id string) *treeNode
	build = func(id string) *treeNode {
		if n, ok := nodes[id]; ok {
			return n
		}
		l, ok := all[id]
		if !ok {
			return nil
		}
		n := &treeNode{Summary: l.summary, ParentID: l.parent, AtIndex: l.atIdx}
		nodes[id] = n
		var kids []*treeNode
		for childID, child := range all {
			if child.parent == id {
				if k := build(childID); k != nil {
					kids = append(kids, k)
				}
			}
		}
		sort.Slice(kids, func(i, j int) bool {
			return kids[i].Summary.CreatedAt.Before(kids[j].Summary.CreatedAt)
		})
		n.Children = kids
		return n
	}
	root := build(rootID)
	if root == nil {
		return nil, nil, -1, fmt.Errorf("buildSessionTree: current session %q is not in the store", currentID)
	}

	// 4. Flat DFS order for picker navigation.
	var flat []*treeNode
	var dfs func(n *treeNode)
	dfs = func(n *treeNode) {
		flat = append(flat, n)
		for _, k := range n.Children {
			dfs(k)
		}
	}
	dfs(root)

	currentIdx := -1
	for i, n := range flat {
		if n.Summary.ID == currentID {
			currentIdx = i
			break
		}
	}
	return root, flat, currentIdx, nil
}

// treeModal is the keyboard-driven /tree picker state. The cursor walks
// the flat DFS order; the renderer turns each level into its own
// indented row with ├─ / └─ glyphs.
type treeModal struct {
	root   *treeNode
	flat   []*treeNode
	cursor int
}

func (m *treeModal) up() {
	if m == nil || len(m.flat) == 0 {
		return
	}
	if m.cursor > 0 {
		m.cursor--
	}
}

func (m *treeModal) down() {
	if m == nil || len(m.flat) == 0 {
		return
	}
	if m.cursor < len(m.flat)-1 {
		m.cursor++
	}
}

func (m *treeModal) selected() (*treeNode, bool) {
	if m == nil || m.cursor < 0 || m.cursor >= len(m.flat) {
		return nil, false
	}
	return m.flat[m.cursor], true
}

// renderTreeModal returns the full overlay string for the /tree picker.
func renderTreeModal(m *treeModal, width int, currentID string) string {
	if m == nil {
		return ""
	}
	w := width - 4
	if w > 100 {
		w = 100
	}
	if w < 40 {
		w = 40
	}

	var b strings.Builder
	b.WriteString(pickerTitle.Render(" Session tree "))
	b.WriteByte('\n')
	if len(m.flat) == 0 {
		b.WriteString(pickerRow.Render("  (no related sessions)"))
	} else {
		lines := renderTreeLines(m.root, m.flat, m.cursor, currentID, w-2)
		b.WriteString(strings.Join(lines, "\n"))
	}
	b.WriteByte('\n')
	b.WriteString(pickerHint.Render("  ↑/↓ navigate · Enter switch · Esc cancel"))

	rendered := pickerBox.Width(w).Render(b.String())
	if w < width {
		return lipgloss.PlaceHorizontal(width, lipgloss.Center, rendered)
	}
	return rendered
}

// renderTreeLines walks the tree in DFS order using indent glyphs
// derived from each node's depth and whether it is the last child of
// its parent. The flat slice mirrors what buildSessionTree returned, so
// each i-th rendered line corresponds to flat[i].
func renderTreeLines(root *treeNode, flat []*treeNode, cursor int, currentID string, max int) []string {
	if root == nil {
		return nil
	}
	type framePos struct {
		indent string
		last   bool
	}

	// First pass: compute the indent prefix for each flat node so that
	// rendering is a simple zip of flat[i] + indent[i].
	indents := make([]string, len(flat))
	idx := 0

	var walk func(n *treeNode, prefix string, last bool)
	walk = func(n *treeNode, prefix string, last bool) {
		// prefix is the "vertical bars" prefix; the glyph for this node
		// is appended depending on root-ness and last-ness.
		var line string
		if n == root {
			line = ""
		} else {
			if last {
				line = prefix + "└─ "
			} else {
				line = prefix + "├─ "
			}
		}
		indents[idx] = line
		idx++
		// Compute the prefix passed to children: extend with " │ " for
		// non-last branches, "   " for last.
		childPrefix := prefix
		if n != root {
			if last {
				childPrefix += "   "
			} else {
				childPrefix += "│  "
			}
		}
		for i, k := range n.Children {
			walk(k, childPrefix, i == len(n.Children)-1)
		}
	}
	walk(root, "", true)

	out := make([]string, len(flat))
	for i, n := range flat {
		summary := nodeLabel(n, n.Summary.ID == currentID)
		row := indents[i] + summary
		if i == cursor {
			out[i] = pickerSel.Render("› " + truncate(row, max-2))
		} else {
			out[i] = pickerRow.Render("  " + truncate(row, max-2))
		}
	}
	return out
}

func nodeLabel(n *treeNode, isCurrent bool) string {
	dot := "●"
	if isCurrent {
		dot = "◉"
	}
	age := "?"
	if !n.Summary.UpdatedAt.IsZero() {
		age = humanAge(time.Since(n.Summary.UpdatedAt))
	}
	tag := ""
	if n.ParentID != "" {
		tag = fmt.Sprintf(" forked@%d", n.AtIndex)
	}
	id := n.Summary.ID
	if len(id) > 28 {
		id = id[:27] + "…"
	}
	return fmt.Sprintf("%s %s%s  %s  %d msg", dot, id, tag, age, n.Summary.Messages)
}
