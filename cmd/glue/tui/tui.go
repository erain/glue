package tui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/erain/glue"
	"github.com/erain/glue/cmd/glue/atmentions"
)

// Config wires the TUI to a glue agent. Provider/Model/WorkDir are
// display-only — the agent already owns them, the TUI just labels them.
type Config struct {
	Agent          *glue.Agent
	SessionID      string
	Provider       string
	Model          string
	WorkDir        string
	Tools          []glue.Tool     // already registered on the agent; for /tools listing
	BasePermission glue.Permission // wrapped under permissionBridge so the TUI can prompt first

	// Store is the agent's session store, passed through so /compact can
	// write a compacted state back without reaching into Agent
	// internals. Nil disables /compact and /resume's "load transcript"
	// path with a friendly system message; the slash commands still
	// parse so users discover them via /help.
	Store glue.Store

	// Provider is the live provider instance, needed only by /compact
	// (which constructs a SummarizingCompactor on the fly). Nil disables
	// /compact with the same friendly degradation as a missing Store.
	ProviderImpl glue.Provider

	// AlwaysAllow turns off the in-card permission prompt entirely: the
	// permission bridge auto-approves every request without surfacing
	// it. Wired by cmd/glue's --yolo flag.
	AlwaysAllow bool
}

// Run launches the TUI and blocks until the user quits. It returns
// ctx.Err() on context cancellation or a setup error.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Agent == nil {
		return fmt.Errorf("tui: Config.Agent is required")
	}
	if cfg.SessionID == "" {
		cfg.SessionID = "tui:" + shortID()
	}

	m := newModel(ctx, cfg)
	// Note: bracketed paste is on by default in bubbletea v1+; we don't
	// need an explicit opt-in.
	p := tea.NewProgram(m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	// Make Send available to the model (for the permission bridge and the
	// per-turn goroutine), and install the bridge as the agent's
	// per-prompt permission via WithPermission. We do not mutate the
	// agent's options. --yolo bypasses the bridge entirely with an
	// always-allow implementation.
	m.send = p.Send
	if cfg.AlwaysAllow {
		m.perm = alwaysAllowPermission{}
	} else {
		m.perm = newPermissionBridge(p.Send)
	}

	if _, err := p.Run(); err != nil {
		return err
	}
	return nil
}

// shortID returns a 12-char hex id for transient session ids.
func shortID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// turnState tracks an in-flight Prompt call.
type turnState struct {
	cancel context.CancelFunc
	// indexes of currently-streaming items so deltas append cheaply
	asstIndex int                  // -1 if no current assistant item
	toolIndex map[string]int       // ToolCallID -> transcript index
}

func newTurnState() *turnState {
	return &turnState{asstIndex: -1, toolIndex: map[string]int{}}
}

// permPending tracks a permission prompt currently shown to the user.
type permPending struct {
	req     glue.PermissionRequest
	respond chan<- glue.PermissionDecision
}

// Model is the bubbletea root.
type Model struct {
	// Wiring
	ctx  context.Context
	cfg  Config
	send func(tea.Msg) // installed after NewProgram

	// Permission handler — created in Run, applied per-prompt via
	// WithPermission. Normally a *permissionBridge; replaced with an
	// always-allow implementation when Config.AlwaysAllow is set.
	perm glue.Permission

	// Layout
	width, height int
	ready         bool

	// Components
	viewport viewport.Model
	input    textarea.Model
	spinner  spinner.Model
	md       *markdownRenderer

	// Transcript
	transcript []transcriptItem

	// Current turn
	turn     *turnState
	turnNum  int
	pending  *permPending
	// permQueue holds permission requests that arrived while another was
	// already on screen — a background /goal loop and a chat turn can ask
	// concurrently, and a single slot would silently drop one (deadlocking
	// its goroutine until cancel). FIFO: answered in arrival order.
	permQueue []permPending
	lastUsage *glue.Usage

	// goal is the single in-TUI goal pursuit (/goal). Nil means none.
	goal *goalState

	// Input history (in-process; not persisted)
	history    []string
	historyPos int // -1 means "not browsing"

	// firstChunkSeen flips true on the first text delta or tool event of
	// the active turn. Used to gate "thinking…" vs the actual state name
	// in the status bar.
	firstChunkSeen bool

	// picker is the /resume modal state. Nil means "no picker open."
	picker *sessionPicker
	// tree is the /tree modal state. Nil means closed.
	tree *treeModal
	// atPicker is the inline `@file` autocomplete popup. Nil means
	// closed. Distinct from picker/tree because it's a popup ABOVE the
	// input box, not a modal that replaces it.
	atPicker *atPicker
	// slashPicker is the inline `/command` autocomplete popup, opened when
	// the input is a bare slash token. Like atPicker it sits above the
	// input box; the two are mutually exclusive (slash needs a leading `/`
	// with no space, @ needs a whitespace-bounded word).
	slashPicker *slashPicker
	// workspaceFiles is the cached file list for the @-autocomplete
	// walker. Walked lazily on first @ trigger; never refreshed within
	// a session (a follow-up could re-walk on /clear).
	workspaceFiles []string

	// Cancel-on-second-ctrl-c semantics
	armedQuit bool

	// Mutex protects send (set once after construction, then read-only).
	mu sync.Mutex
}

func newModel(ctx context.Context, cfg Config) *Model {
	ta := textarea.New()
	ta.Placeholder = "Ask anything · / for commands"
	// No internal prompt character: the lipgloss box border around the
	// textarea already provides one vertical line, and "│ │" looked like
	// a broken double-border.
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.SetWidth(80)
	// Start at 1 row and grow as the user types up to inputMaxRows.
	// A 3-row minimum made short prompts feel heavy.
	ta.SetHeight(1)
	ta.ShowLineNumbers = false

	// Enter submits (intercepted in handleInputKey before the textarea
	// sees it). Ctrl+J is ASCII LF and works on every terminal, unlike
	// Shift+Enter which most terminals don't distinguish from Enter.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("ctrl+j"),
		key.WithHelp("ctrl+j", "newline"),
	)

	// Strip the highlighted cursor-line background — looks loud on a
	// one-line input.
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(inkMuted).Italic(true)
	ta.BlurredStyle.Placeholder = ta.FocusedStyle.Placeholder
	ta.Cursor.Style = lipgloss.NewStyle().Foreground(accent)

	ta.Focus()

	vp := viewport.New(80, 20)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(warnCol)

	return &Model{
		ctx:        ctx,
		cfg:        cfg,
		viewport:   vp,
		input:      ta,
		spinner:    sp,
		transcript: nil,
		historyPos: -1,
	}
}

func (m *Model) Init() tea.Cmd {
	m.appendWelcome()
	return tea.Batch(textarea.Blink)
}

// appendWelcome seeds the transcript with the welcome card. It is used
// at startup and after /clear so the empty-state is never blank.
func (m *Model) appendWelcome() {
	lines := []string{
		"  Try:",
		welcomeAccent.Render("    › ") + "What does Session.Prompt do?",
		welcomeAccent.Render("    › ") + "Run the tests and fix the first failure.",
		welcomeAccent.Render("    › ") + "Summarize the changes in this branch vs main.",
		"",
		keyHint.Render("  Enter sends · Ctrl+J newline · / for commands · Esc cancels · Ctrl+C exits"),
		"",
		keyHint.Render(fmt.Sprintf("  session %s · %s/%s · %s",
			m.cfg.SessionID,
			providerOrDefault(m.cfg.Provider),
			modelOrDefault(m.cfg.Model),
			workOrDot(m.cfg.WorkDir))),
	}
	if m.cfg.AlwaysAllow {
		lines = append(lines, toolWarning.Render("  ⚠  --yolo enabled: permission prompts are off."))
	}
	m.transcript = append(m.transcript, transcriptItem{
		Kind:       itemBlock,
		BlockTitle: "Welcome to glue",
		BlockBody:  strings.Join(lines, "\n"),
	})
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Glamour needs a width; match the transcript body so wrapping
		// aligns with what the user sees. We cap at bodyMaxWidth for the
		// same reason layout() does — see the constant's doc.
		mdW := msg.Width
		if mdW > bodyMaxWidth {
			mdW = bodyMaxWidth
		}
		if m.md == nil {
			m.md = newMarkdownRenderer(mdW)
		} else {
			m.md.Resize(mdW)
		}
		m.layout()
		m.ready = true
		m.rerender()
		return m, nil

	case tea.KeyMsg:
		// Modals own the keyboard while open: tree view first, then the
		// /resume picker, then the in-card permission prompt, then normal
		// input handling. Order matches "most recent overlay wins."
		if m.tree != nil {
			return m.handleTreeKey(msg)
		}
		if m.picker != nil {
			return m.handlePickerKey(msg)
		}
		if m.pending != nil {
			return m.handlePermissionKey(msg)
		}
		return m.handleInputKey(msg)

	case tea.MouseMsg:
		var c tea.Cmd
		m.viewport, c = m.viewport.Update(msg)
		return m, c

	case spinner.TickMsg:
		// Spinner only animates during an in-flight turn or goal loop; once
		// both end, stop ticking instead of consuming Update cycles forever.
		if m.turn == nil && !m.goalRunning() {
			return m, nil
		}
		var c tea.Cmd
		m.spinner, c = m.spinner.Update(msg)
		m.rerender()
		return m, c

	case textDeltaMsg:
		m.firstChunkSeen = true
		m.handleTextDelta(string(msg))
		m.rerender()
		return m, nil

	case toolStartMsg:
		m.firstChunkSeen = true
		m.handleToolStart(msg)
		m.rerender()
		return m, nil

	case toolEndMsg:
		m.handleToolEnd(msg)
		m.rerender()
		return m, nil

	case permRequestMsg:
		if m.pending != nil {
			m.permQueue = append(m.permQueue, permPending{req: msg.Req, respond: msg.Respond})
			return m, nil
		}
		m.pending = &permPending{req: msg.Req, respond: msg.Respond}
		m.markPendingTool(msg.Req)
		m.rerender()
		return m, nil

	case turnDoneMsg:
		m.handleTurnDone(msg)
		m.rerender()
		return m, nil

	case goalEventMsg:
		m.handleGoalEvent(msg.Ev)
		m.rerender()
		return m, nil

	case goalDoneMsg:
		m.handleGoalDone(msg)
		m.rerender()
		return m, nil

	case systemMsg:
		m.appendSystem(string(msg))
		m.rerender()
		return m, nil

	case sessionSwitchedMsg:
		m.cfg.SessionID = msg.ID
		m.transcript = nil
		m.detachGoalCard()
		m.appendSystem(msg.Note + ": " + msg.ID)
		m.transcript = append(m.transcript, transcriptFromMessages(msg.Messages)...)
		m.turnNum = 0
		m.lastUsage = nil
		m.rerender()
		return m, nil

	case fatalErrMsg:
		m.appendSystem("fatal: " + msg.Err.Error())
		m.rerender()
		return m, tea.Quit
	}

	// Bubble updates for input + viewport.
	var c tea.Cmd
	m.input, c = m.input.Update(msg)
	cmds = append(cmds, c)
	m.viewport, c = m.viewport.Update(msg)
	cmds = append(cmds, c)
	return m, tea.Batch(cmds...)
}

func (m *Model) View() string {
	if !m.ready {
		return "loading…"
	}
	header := m.headerView()
	body := m.viewport.View()
	// Center the transcript body inside the full terminal width on wide
	// terminals — viewport itself is capped at bodyMaxWidth by layout(),
	// so without this it would sit pinned to the left edge.
	if m.width > bodyMaxWidth {
		body = lipgloss.PlaceHorizontal(m.width, lipgloss.Center, body)
	}
	bottom := m.bottomView()
	status := m.statusView()

	return lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		body,
		bottom,
		status,
	)
}

// ---------- helpers: layout / rendering ----------

// inputMaxBoxWidth caps the visible input box on wide terminals so it
// doesn't stretch across a wall and feel disconnected from the
// conversation centered above it.
const inputMaxBoxWidth = 100

// bodyMaxWidth caps the transcript viewport on wide terminals. Locked
// to the input box width so the conversation column and the input box
// align. Without this cap, assistant text wraps at the right edge of a
// 200-col terminal and reads as a wall of text. Header and status bar
// keep full width — they read better edge-to-edge.
const bodyMaxWidth = inputMaxBoxWidth

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	headerH := 1
	statusH := 1
	inputH := m.inputHeight()
	// Border (1 top + 1 bottom) + padding (0,1) on the input box adds 2 rows.
	bottomH := inputH + 2
	// The @-autocomplete popup sits above the input box and eats into the
	// viewport's vertical space while it's open. Height: title(1) +
	// matches(<=visible) + hint(1) + borders(2).
	if m.atPicker != nil {
		rows := len(m.atPicker.matches)
		if rows > atPickerVisibleRows {
			rows = atPickerVisibleRows
		}
		if rows == 0 {
			rows = 1 // "(no matches)" row
		}
		bottomH += rows + 4
	}
	// The /command popup occupies the same slot as the @-picker (they are
	// never open together). Same height math: title(1) + matches + hint(1)
	// + borders(2).
	if m.slashPicker != nil {
		rows := len(m.slashPicker.matches)
		if rows > atPickerVisibleRows {
			rows = atPickerVisibleRows
		}
		if rows == 0 {
			rows = 1 // "(no matching command)" row
		}
		bottomH += rows + 4
	}
	bodyH := m.height - headerH - statusH - bottomH
	if bodyH < 3 {
		bodyH = 3
	}
	// Cap the viewport at bodyMaxWidth and let View() center it inside
	// m.width on wide terminals. Keeps the conversation column readable.
	vpW := m.width
	if vpW > bodyMaxWidth {
		vpW = bodyMaxWidth
	}
	m.viewport.Width = vpW
	m.viewport.Height = bodyH

	// Inner textarea width is the box width minus border (2) and
	// padding (2). Cap the box at inputMaxBoxWidth on wide terminals.
	boxW := m.width - 4
	if boxW > inputMaxBoxWidth {
		boxW = inputMaxBoxWidth
	}
	if boxW < 20 {
		boxW = 20
	}
	m.input.SetWidth(boxW - 4)
}

func (m *Model) inputHeight() int {
	// Default to a single row and grow as the user types, up to 6.
	// Past 6 the textarea scrolls internally.
	const minH, maxH = 1, 6
	h := m.input.LineCount()
	if h < minH {
		h = minH
	}
	if h > maxH {
		h = maxH
	}
	m.input.SetHeight(h)
	return h
}


func (m *Model) rerender() {
	// Sticky scroll: only auto-scroll if the user is already at the
	// bottom. If they scrolled up to read older context, preserve their
	// view position so the next streaming delta doesn't yank them away.
	wasAtBottom := m.viewport.AtBottom()

	width := m.viewport.Width
	if width == 0 {
		width = 80
	}
	ctx := renderCtx{
		width:   width,
		spinner: m.spinner.View(),
	}

	var b strings.Builder
	prevContentful := false
	for _, it := range m.transcript {
		// Insert a subtle horizontal rule between distinct turns, defined
		// as "the previous block ended a turn and a new user message is
		// starting." This makes the user → assistant → user cadence
		// scannable even on long sessions.
		if it.Kind == itemUser && prevContentful {
			b.WriteString(turnRule(width))
			b.WriteString("\n\n")
		}
		b.WriteString(it.render(ctx))
		b.WriteString("\n\n")
		switch it.Kind {
		case itemAssistant, itemTool:
			prevContentful = true
		case itemUser:
			prevContentful = false
		}
	}
	m.viewport.SetContent(strings.TrimRight(b.String(), "\n"))
	if wasAtBottom {
		m.viewport.GotoBottom()
	}
	m.layout()
}

// turnRule returns the thin rule rendered between turns.
func turnRule(width int) string {
	n := width - 4
	if n < 20 {
		n = 20
	}
	if n > 80 {
		n = 80
	}
	return turnSeparator(strings.Repeat("─", n))
}

func (m *Model) headerView() string {
	left := headerBrand.Render("glue") +
		"  " + headerStyle.Render(fmt.Sprintf("session %s · %s/%s",
			m.cfg.SessionID, providerOrDefault(m.cfg.Provider), modelOrDefault(m.cfg.Model)))
	right := headerStyle.Render(workOrDot(m.cfg.WorkDir))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m *Model) bottomView() string {
	// Modals take over the bottom region while open.
	if m.tree != nil {
		return renderTreeModal(m.tree, m.width, m.cfg.SessionID)
	}
	if m.picker != nil {
		return renderPicker(m.picker, m.width)
	}
	// @-autocomplete popup sits ABOVE the input box rather than
	// replacing it. The textarea keeps the cursor; the popup just
	// suggests files.
	if m.atPicker != nil {
		input := m.renderInputBox()
		pop := renderAtPicker(m.atPicker, m.width)
		return lipgloss.JoinVertical(lipgloss.Left, pop, input)
	}
	if m.slashPicker != nil {
		input := m.renderInputBox()
		pop := renderSlashPicker(m.slashPicker, m.width)
		return lipgloss.JoinVertical(lipgloss.Left, pop, input)
	}
	return m.renderInputBox()
}

// renderInputBox lays out just the input box (no overlays). Extracted so
// the @-autocomplete branch can stack the popup above the same box.
func (m *Model) renderInputBox() string {
	boxW := m.width - 4
	if boxW > inputMaxBoxWidth {
		boxW = inputMaxBoxWidth
	}
	if boxW < 20 {
		boxW = 20
	}
	box := inputBoxStyle.Width(boxW).Render(m.input.View())
	if boxW < m.width {
		return lipgloss.PlaceHorizontal(m.width, lipgloss.Center, box)
	}
	return box
}

func (m *Model) statusView() string {
	var parts []string
	if m.turnNum > 0 || m.turn != nil {
		parts = append(parts, fmt.Sprintf("turn %d", m.turnNum+boolToInt(m.turn != nil)))
	}
	if m.lastUsage != nil {
		parts = append(parts, fmt.Sprintf("%d in / %d out",
			m.lastUsage.InputTokens, m.lastUsage.OutputTokens))
	}
	if m.turn != nil {
		state := "thinking"
		if m.firstChunkSeen {
			state = "streaming"
		}
		for _, it := range m.transcript {
			if it.Kind == itemTool && it.ToolPhase == tsRunning {
				state = it.ToolName
				break
			}
		}
		parts = append(parts, m.spinner.View()+" "+toolWarning.Render(state))
	}
	if m.goal != nil {
		parts = append(parts, m.goal.statusSegment())
	}
	if m.pending != nil {
		parts = append(parts, toolWarning.Render("permission needed"))
	}
	if !m.viewport.AtBottom() {
		parts = append(parts, keyHint.Render("↓ more below"))
	}
	if m.cfg.AlwaysAllow {
		parts = append(parts, toolWarning.Render("yolo"))
	}
	parts = append(parts, keyHint.Render("Enter send · ^J newline · /help · esc cancel · ^C exit"))
	return statusStyle.Render(strings.Join(parts, "  ·  "))
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- helpers: input handling ----------

func (m *Model) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Esc semantics, ordered most-specific first:
	//   - @-autocomplete open: close it, remove the @-token from input
	//   - turn in flight: cancel it
	//   - otherwise: no-op
	if msg.Type == tea.KeyEsc {
		if m.slashPicker != nil {
			// Just close the popup; leave the typed text so the user can
			// finish or edit the command by hand.
			m.slashPicker = nil
			m.layout()
			m.rerender()
			return m, nil
		}
		if m.atPicker != nil {
			m.input.SetValue(removeAtToken(m.input.Value()))
			m.input.CursorEnd()
			m.atPicker = nil
			m.layout()
			m.rerender()
			return m, nil
		}
		if m.turn != nil {
			m.turn.cancel()
			m.appendSystem("turn cancelled (esc).")
			m.rerender()
			return m, nil
		}
		return m, nil
	}

	// Ctrl+C: first press cancels current turn; second exits.
	if msg.Type == tea.KeyCtrlC {
		if m.turn != nil {
			m.turn.cancel()
			m.appendSystem("turn cancelled.")
			m.armedQuit = false
			m.rerender()
			return m, nil
		}
		if m.armedQuit {
			return m, tea.Quit
		}
		m.armedQuit = true
		m.appendSystem("press Ctrl+C again to quit, or type to continue.")
		m.rerender()
		return m, nil
	}
	m.armedQuit = false

	// /command autocomplete intercepts navigation/complete keys BEFORE
	// history scroll and Enter-submit. Tab completes the highlighted
	// command; Enter runs the input as typed (so a fully-typed command
	// fires on the first Enter), except when the command is only partly
	// typed — then Enter completes it, matching the @-picker's feel.
	if m.slashPicker != nil {
		switch msg.Type {
		case tea.KeyTab:
			return m.slashPickerAccept()
		case tea.KeyUp:
			m.slashPicker.up()
			m.rerender()
			return m, nil
		case tea.KeyDown:
			m.slashPicker.down()
			m.rerender()
			return m, nil
		case tea.KeyEnter:
			if sel, ok := m.slashPicker.selected(); ok && !m.slashPicker.exactMatch() {
				// Complete to the highlighted command. If it takes no
				// argument, run it straight away; otherwise fill `/name `
				// and wait for the user to type the argument.
				m.slashPicker = nil
				if sel.Args == "" {
					m.input.SetValue("/" + sel.Name)
					return m.submit()
				}
				m.input.SetValue(applySlashSelection(sel.Name))
				m.input.CursorEnd()
				m.layout()
				m.rerender()
				return m, nil
			}
			// Exact match (or no matches): run the input as typed.
			m.slashPicker = nil
			return m.submit()
		}
	}

	// @-autocomplete commands intercept their keys BEFORE history scroll
	// and Enter-submit, so the navigation/select/cancel UX works.
	if m.atPicker != nil {
		switch msg.Type {
		case tea.KeyTab:
			return m.atPickerAccept()
		case tea.KeyUp:
			m.atPicker.up()
			m.rerender()
			return m, nil
		case tea.KeyDown:
			m.atPicker.down()
			m.rerender()
			return m, nil
		case tea.KeyEnter:
			// Enter with a real match inserts; Enter with no matches
			// falls through to submit so a user can still send a
			// literal "@nonsense" prompt.
			if len(m.atPicker.matches) > 0 {
				return m.atPickerAccept()
			}
		}
	}

	switch msg.Type {
	case tea.KeyUp:
		if m.input.LineCount() <= 1 && len(m.history) > 0 {
			m.scrollHistory(-1)
			return m, nil
		}
	case tea.KeyDown:
		if m.input.LineCount() <= 1 && m.historyPos >= 0 {
			m.scrollHistory(+1)
			return m, nil
		}
	case tea.KeyPgUp:
		m.viewport.HalfViewUp()
		return m, nil
	case tea.KeyPgDown:
		m.viewport.HalfViewDown()
		return m, nil
	case tea.KeyEnter:
		// Enter submits unconditionally. Multi-line input uses Ctrl+J
		// (ASCII LF), bound on the textarea's KeyMap.InsertNewline in
		// newModel. Shift+Enter is intentionally NOT supported — most
		// terminals don't distinguish it from Enter.
		return m.submit()
	}

	var c tea.Cmd
	m.input, c = m.input.Update(msg)
	// After the textarea has processed the keystroke, re-evaluate which
	// inline autocomplete (if any) should be open for the new input.
	m.refreshPickers()
	m.layout()
	return m, c
}

// refreshPickers opens, updates, or closes the inline autocomplete popups
// after a keystroke. A leading slash takes precedence (the input is a
// command being typed); otherwise fall back to the @-file picker. The two
// are mutually exclusive.
func (m *Model) refreshPickers() {
	if q, ok := detectSlashTrigger(m.input.Value()); ok {
		m.atPicker = nil
		if m.slashPicker == nil {
			m.slashPicker = newSlashPicker()
		}
		m.slashPicker.refilter(q)
		return
	}
	m.slashPicker = nil
	m.refreshAtPicker()
}

// slashPickerAccept completes the input to the highlighted command
// (`/name `, ready for an argument) and closes the popup.
func (m *Model) slashPickerAccept() (tea.Model, tea.Cmd) {
	if m.slashPicker == nil {
		return m, nil
	}
	sel, ok := m.slashPicker.selected()
	m.slashPicker = nil
	if !ok {
		m.layout()
		m.rerender()
		return m, nil
	}
	m.input.SetValue(applySlashSelection(sel.Name))
	m.input.CursorEnd()
	m.layout()
	m.rerender()
	return m, nil
}

// refreshAtPicker is called after every keystroke that flowed through
// to the textarea. It opens, updates, or closes the @-autocomplete
// popup based on the new input value.
func (m *Model) refreshAtPicker() {
	q, ok := detectAtTrigger(m.input.Value())
	if !ok {
		m.atPicker = nil
		return
	}
	if m.workspaceFiles == nil {
		m.workspaceFiles = walkWorkspace(m.cfg.WorkDir)
	}
	if m.atPicker == nil {
		m.atPicker = &atPicker{files: m.workspaceFiles}
		m.atPicker.refilter(q)
		return
	}
	if m.atPicker.query != q {
		m.atPicker.refilter(q)
	}
}

// atPickerAccept inserts the currently-selected file into the input,
// closes the popup, and returns to normal typing.
func (m *Model) atPickerAccept() (tea.Model, tea.Cmd) {
	if m.atPicker == nil {
		return m, nil
	}
	sel := m.atPicker.selected()
	m.atPicker = nil
	if sel == "" {
		m.layout()
		m.rerender()
		return m, nil
	}
	m.input.SetValue(applyAtSelection(m.input.Value(), sel))
	m.input.CursorEnd()
	m.layout()
	m.rerender()
	return m, nil
}

func (m *Model) scrollHistory(delta int) {
	if len(m.history) == 0 {
		return
	}
	if m.historyPos == -1 {
		// Snapshot the buffer in case user wants to come back.
		// (Simplification: discard current text.)
		m.historyPos = len(m.history)
	}
	m.historyPos += delta
	if m.historyPos < 0 {
		m.historyPos = 0
	}
	if m.historyPos >= len(m.history) {
		m.historyPos = -1
		m.input.SetValue("")
		return
	}
	m.input.SetValue(m.history[m.historyPos])
	m.input.CursorEnd()
}

func (m *Model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimRight(m.input.Value(), "\n")
	if strings.TrimSpace(text) == "" {
		return m, nil
	}
	m.input.Reset()
	m.history = append(m.history, text)
	m.historyPos = -1

	if cmd, ok := parseSlashCommand(text); ok {
		return m.handleSlash(cmd)
	}

	if m.turn != nil {
		m.appendSystem("a turn is already running; Esc to cancel.")
		m.rerender()
		return m, nil
	}

	// Expand @file mentions into the prompt before it's sent to the
	// model. The user sees their original `@util.go` in the transcript;
	// the agent sees the rewritten prompt with the file contents
	// appended. Skipped mentions become system messages so the user
	// knows why a file didn't get inlined.
	atRes, atErr := atmentions.Expand(text, atmentions.Options{WorkDir: m.cfg.WorkDir})
	if atErr != nil {
		m.appendSystem("@-mention error: " + atErr.Error())
		m.rerender()
		return m, nil
	}
	for _, skip := range atRes.Skipped {
		m.appendSystem(skip.Mention + ": " + skip.Reason)
	}

	m.transcript = append(m.transcript, transcriptItem{Kind: itemUser, Text: text})
	cmd := m.startTurn(atRes.Prompt)
	m.rerender()
	return m, cmd
}

func (m *Model) handleSlash(cmd slashCommand) (tea.Model, tea.Cmd) {
	switch cmd.Name {
	case "exit", "quit", "q":
		return m, tea.Quit
	case "help":
		m.appendBlock("Commands", describeCommands())
		m.rerender()
		return m, nil
	case "clear", "new":
		// Nuke the transcript AND start a new session id. The old
		// behavior (only switching session id) confused users who saw
		// last turn's content under a fresh session.
		m.transcript = nil
		m.detachGoalCard()
		m.cfg.SessionID = "tui:" + shortID()
		m.appendWelcome()
		m.appendSystem("transcript cleared. new session: " + m.cfg.SessionID)
		m.turnNum = 0
		m.lastUsage = nil
		m.rerender()
		return m, nil
	case "usage":
		if m.lastUsage == nil {
			m.appendSystem("no usage reported yet.")
		} else {
			m.appendSystem(fmt.Sprintf("usage: %d in / %d out / %d total",
				m.lastUsage.InputTokens, m.lastUsage.OutputTokens, m.lastUsage.TotalTokens))
		}
		m.rerender()
		return m, nil
	case "tools":
		if len(m.cfg.Tools) == 0 {
			m.appendSystem("no tools registered.")
		} else {
			names := make([]string, 0, len(m.cfg.Tools))
			for _, t := range m.cfg.Tools {
				names = append(names, t.Name)
			}
			sort.Strings(names)
			m.appendBlock("Registered tools", "  "+strings.Join(names, "\n  "))
		}
		m.rerender()
		return m, nil
	case "model":
		if cmd.Arg == "" {
			m.appendSystem("current model: " + modelOrDefault(m.cfg.Model))
		} else {
			m.cfg.Model = cmd.Arg
			m.appendSystem("model set: " + cmd.Arg + " (effective next turn)")
		}
		m.rerender()
		return m, nil
	case "session":
		if cmd.Arg == "" {
			m.appendSystem("current session: " + m.cfg.SessionID)
		} else {
			m.cfg.SessionID = cmd.Arg
			m.appendSystem("session set: " + cmd.Arg)
		}
		m.rerender()
		return m, nil
	case "goal":
		return m.handleSlashGoal(cmd.Arg)
	case "compact":
		return m.runSlashCompact()
	case "resume":
		return m.runSlashResume()
	case "fork":
		return m.runSlashFork(cmd.Arg)
	case "clone":
		return m.runSlashClone()
	case "tree":
		return m.runSlashTree()
	default:
		m.appendSystem("unknown command: /" + cmd.Name + " (try /help)")
		m.rerender()
		return m, nil
	}
}

// appendBlock is the structured counterpart to appendSystem: a titled,
// multi-line panel rendered with a rounded border.
func (m *Model) appendBlock(title, body string) {
	m.transcript = append(m.transcript, transcriptItem{
		Kind:       itemBlock,
		BlockTitle: title,
		BlockBody:  body,
	})
}

// ---------- helpers: permission keyboard ----------

func (m *Model) handlePermissionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.pending == nil {
		return m, nil
	}
	var decision *glue.PermissionDecision
	switch {
	case msg.Type == tea.KeyEsc:
		decision = &glue.PermissionDecision{Allow: false, Reason: "denied (esc)"}
	case msg.Type == tea.KeyCtrlC:
		decision = &glue.PermissionDecision{Allow: false, Reason: "cancelled"}
		// Also cancel the in-flight turn.
		if m.turn != nil {
			m.turn.cancel()
		}
	default:
		switch strings.ToLower(string(msg.Runes)) {
		case "a", "y":
			decision = &glue.PermissionDecision{Allow: true}
		case "s":
			decision = &glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSession}
		case "t":
			decision = &glue.PermissionDecision{Allow: true, RememberFor: glue.RememberSessionTarget}
		case "n":
			decision = &glue.PermissionDecision{Allow: false, Reason: "denied by user"}
		}
	}
	if decision == nil {
		return m, nil
	}
	p := m.pending
	m.pending = nil
	if !decision.Allow {
		m.markToolDenied(p.req.Tool, p.req.Target)
	} else {
		m.markToolRunning(p.req.Tool, p.req.Target)
	}
	p.respond <- *decision
	close(p.respond)
	// Promote the next queued request, if any (e.g. a /goal tool call that
	// arrived while this prompt was on screen).
	if len(m.permQueue) > 0 {
		next := m.permQueue[0]
		m.permQueue = m.permQueue[1:]
		m.pending = &next
		m.markPendingTool(next.req)
	}
	m.rerender()
	return m, nil
}

// ---------- helpers: transcript mutation ----------

func (m *Model) appendSystem(text string) {
	m.transcript = append(m.transcript, transcriptItem{Kind: itemSystem, Text: text})
}

func (m *Model) handleTextDelta(d string) {
	if m.turn == nil {
		return
	}
	if m.turn.asstIndex < 0 {
		m.transcript = append(m.transcript, transcriptItem{Kind: itemAssistant})
		m.turn.asstIndex = len(m.transcript) - 1
	}
	m.transcript[m.turn.asstIndex].Text += d
}

func (m *Model) handleToolStart(msg toolStartMsg) {
	if m.turn == nil {
		return
	}
	if idx, ok := m.turn.toolIndex[msg.CallID]; ok {
		// Already added (e.g. by permission flow); transition to running.
		m.transcript[idx].ToolPhase = tsRunning
		return
	}
	m.transcript = append(m.transcript, transcriptItem{
		Kind:       itemTool,
		ToolCallID: msg.CallID,
		ToolName:   msg.Name,
		ToolArgs:   msg.Args,
		ToolPhase:  tsRunning,
	})
	m.turn.toolIndex[msg.CallID] = len(m.transcript) - 1
}

func (m *Model) handleToolEnd(msg toolEndMsg) {
	if m.turn == nil {
		return
	}
	idx, ok := m.turn.toolIndex[msg.CallID]
	if !ok {
		return
	}
	m.transcript[idx].ToolResult = msg.Text
	m.transcript[idx].ToolErr = msg.IsError
	m.transcript[idx].ToolPhase = tsDone
	// Reset assistant streaming index — the next text after a tool result
	// goes into a fresh assistant item, like Claude Code / Aider.
	m.turn.asstIndex = -1
}

// markPendingTool finds the most recent tool item that matches the
// permission request and marks it pending (the model emitted a tool_call,
// then the loop reached the executor and asked for permission). If we
// haven't seen the tool_start yet, the next handleToolStart() will find
// the existing pending entry and transition it to running.
func (m *Model) markPendingTool(req glue.PermissionRequest) {
	// Synthesize a placeholder item if we don't have one yet.
	for i := len(m.transcript) - 1; i >= 0; i-- {
		it := &m.transcript[i]
		if it.Kind == itemTool && it.ToolName == req.Tool && it.ToolPhase == tsRunning {
			it.ToolPhase = tsPending
			return
		}
	}
	args := req.Args
	argStr := ""
	if len(args) > 0 {
		argStr = string(args)
		// Compact the JSON for display.
		var v any
		if err := json.Unmarshal(args, &v); err == nil {
			if b, err := json.Marshal(v); err == nil {
				argStr = string(b)
			}
		}
	}
	m.transcript = append(m.transcript, transcriptItem{
		Kind:      itemTool,
		ToolName:  req.Tool,
		ToolArgs:  argStr,
		ToolPhase: tsPending,
	})
}

func (m *Model) markToolRunning(name, target string) {
	for i := len(m.transcript) - 1; i >= 0; i-- {
		it := &m.transcript[i]
		if it.Kind == itemTool && it.ToolName == name && it.ToolPhase == tsPending {
			it.ToolPhase = tsRunning
			return
		}
		_ = target
	}
}

func (m *Model) markToolDenied(name, target string) {
	for i := len(m.transcript) - 1; i >= 0; i-- {
		it := &m.transcript[i]
		if it.Kind == itemTool && it.ToolName == name && it.ToolPhase == tsPending {
			it.ToolPhase = tsDenied
			return
		}
		_ = target
	}
}

func (m *Model) handleTurnDone(msg turnDoneMsg) {
	if m.turn == nil {
		return
	}
	if msg.Err != nil {
		m.appendSystem("error: " + msg.Err.Error())
	}
	// Re-render the final assistant item through glamour so code blocks,
	// lists, headings, and inline code settle into proper formatting after
	// the stream completes. We deliberately keep streaming as plain text
	// to avoid partial-markdown flicker.
	if m.md != nil && m.turn.asstIndex >= 0 && m.turn.asstIndex < len(m.transcript) {
		it := &m.transcript[m.turn.asstIndex]
		if strings.TrimSpace(it.Text) != "" {
			it.Rendered = m.md.Render(it.Text)
		}
	}
	m.turn = nil
	m.turnNum++
}

// ---------- agent goroutine ----------

func (m *Model) startTurn(prompt string) tea.Cmd {
	if m.send == nil {
		// Should not happen — Run installs send before p.Run.
		m.appendSystem("tui: program send not initialised")
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	state := newTurnState()
	state.cancel = cancel
	m.turn = state
	m.firstChunkSeen = false

	send := m.send
	bridge := m.perm
	session, err := m.cfg.Agent.Session(ctx, m.cfg.SessionID)
	if err != nil {
		cancel()
		m.turn = nil
		m.appendSystem("session error: " + err.Error())
		return nil
	}

	go func() {
		defer cancel()
		unsubscribe := session.Subscribe(func(e glue.Event) {
			switch e.Type {
			case glue.EventTextDelta:
				if e.Delta != "" {
					send(textDeltaMsg(e.Delta))
				}
			case glue.EventToolStart:
				call := e.ToolCall
				if call == nil {
					return
				}
				args := ""
				if len(call.Arguments) > 0 {
					var v any
					if err := json.Unmarshal(call.Arguments, &v); err == nil {
						b, _ := json.Marshal(v)
						args = string(b)
					} else {
						args = string(call.Arguments)
					}
				}
				send(toolStartMsg{CallID: call.ID, Name: call.Name, Args: args})
			case glue.EventToolEnd:
				text := ""
				isErr := false
				if e.ToolResult != nil {
					isErr = e.ToolResult.IsError
					var parts []string
					for _, p := range e.ToolResult.Content {
						if p.Type == glue.ContentTypeText && p.Text != "" {
							parts = append(parts, p.Text)
						}
					}
					text = strings.Join(parts, "\n")
				}
				send(toolEndMsg{CallID: e.ToolCallID, Text: text, IsError: isErr})
			}
		})
		defer unsubscribe()

		opts := []glue.PromptOption{}
		if bridge != nil {
			opts = append(opts, glue.WithPermission(bridge))
		}
		if m.cfg.Model != "" {
			opts = append(opts, glue.WithModel(m.cfg.Model))
		}

		res, runErr := session.Prompt(ctx, prompt, opts...)
		// Best-effort: surface final text only if streaming didn't emit it
		// (some providers return everything in one shot at the end).
		finalText := ""
		if res.Text != "" {
			finalText = res.Text
		}
		// Pull usage off the latest assistant message if present.
		// (PromptResult exposes it via Usage on some surfaces; if not, leave nil.)
		send(turnDoneMsg{Err: runErr, Text: finalText})
	}()

	// Kick the spinner. It re-arms itself in the Update loop until
	// turnDoneMsg clears m.turn, at which point spinner.TickMsg becomes
	// a no-op (see Update).
	return m.spinner.Tick
}

// ---------- small helpers ----------

func providerOrDefault(p string) string {
	if p == "" {
		return "default"
	}
	return p
}
func modelOrDefault(m string) string {
	if m == "" {
		return "default"
	}
	return m
}
func workOrDot(d string) string {
	if d == "" {
		return "."
	}
	return d
}

// ---------- /compact ----------

func (m *Model) runSlashCompact() (tea.Model, tea.Cmd) {
	if m.cfg.Store == nil || m.cfg.ProviderImpl == nil {
		m.appendSystem("/compact unavailable: store or provider not wired into the TUI")
		m.rerender()
		return m, nil
	}
	if m.turn != nil {
		m.appendSystem("/compact: a turn is running; Esc to cancel first.")
		m.rerender()
		return m, nil
	}
	cfg := m.cfg
	parentCtx := m.ctx
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(parentCtx, 90*time.Second)
		defer cancel()
		session, err := cfg.Agent.Session(ctx, cfg.SessionID)
		if err != nil {
			return systemMsg("/compact: " + err.Error())
		}
		before := len(session.Messages())
		comp := &glue.SummarizingCompactor{
			Provider: cfg.ProviderImpl,
			Model:    cfg.Model,
		}
		out, err := comp.Compact(ctx, session.Messages())
		if err != nil {
			return systemMsg("/compact failed: " + err.Error())
		}
		after := len(out)
		if after >= before {
			return systemMsg(fmt.Sprintf("/compact: nothing to compact (%d messages)", before))
		}
		state := session.State()
		state.Messages = out
		state.UpdatedAt = time.Now()
		if err := cfg.Store.Save(ctx, cfg.SessionID, state); err != nil {
			return systemMsg("/compact: save failed: " + err.Error())
		}
		return systemMsg(fmt.Sprintf("/compact: summarized %d → %d messages", before, after))
	}
}

// ---------- /resume ----------

func (m *Model) runSlashResume() (tea.Model, tea.Cmd) {
	if m.cfg.Agent == nil {
		m.appendSystem("/resume unavailable: no agent")
		m.rerender()
		return m, nil
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	items, err := m.cfg.Agent.ListSessions(ctx, glue.ListSessionsOptions{Limit: 10})
	if err != nil {
		m.appendSystem("/resume: " + err.Error())
		m.rerender()
		return m, nil
	}
	// Hide the currently-active session from the picker — picking it
	// would be a no-op.
	filtered := make([]glue.SessionSummary, 0, len(items))
	for _, s := range items {
		if s.ID != m.cfg.SessionID {
			filtered = append(filtered, s)
		}
	}
	if len(filtered) == 0 {
		m.appendSystem("/resume: no other sessions on the store.")
		m.rerender()
		return m, nil
	}
	m.picker = &sessionPicker{items: filtered}
	m.rerender()
	return m, nil
}

// ---------- /fork, /clone, /tree ----------

func (m *Model) runSlashFork(arg string) (tea.Model, tea.Cmd) {
	if m.cfg.Agent == nil {
		m.appendSystem("/fork unavailable: no agent")
		m.rerender()
		return m, nil
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	current, err := m.cfg.Agent.Session(ctx, m.cfg.SessionID)
	if err != nil {
		m.appendSystem("/fork: " + err.Error())
		m.rerender()
		return m, nil
	}
	msgs := current.Messages()
	if len(msgs) == 0 {
		m.appendSystem("/fork: nothing to fork (session has no messages yet)")
		m.rerender()
		return m, nil
	}
	// Default fork point: just before the most recent user message.
	// That's "redo from my last turn" — the common case.
	at := len(msgs)
	if arg == "" {
		at = lastUserMessageIndex(msgs)
		if at < 0 {
			at = len(msgs)
		}
	} else {
		n, err := parseForkArg(arg, len(msgs))
		if err != nil {
			m.appendSystem("/fork: " + err.Error())
			m.rerender()
			return m, nil
		}
		at = n
	}
	newID := "tui:" + shortID()
	if err := m.cfg.Agent.ForkSession(ctx, m.cfg.SessionID, at, newID); err != nil {
		m.appendSystem("/fork: " + err.Error())
		m.rerender()
		return m, nil
	}
	return m, switchToSession(m, newID, fmt.Sprintf("forked at message %d", at))
}

func (m *Model) runSlashClone() (tea.Model, tea.Cmd) {
	if m.cfg.Agent == nil {
		m.appendSystem("/clone unavailable: no agent")
		m.rerender()
		return m, nil
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	newID := "tui:" + shortID()
	if err := m.cfg.Agent.CloneSession(ctx, m.cfg.SessionID, newID); err != nil {
		m.appendSystem("/clone: " + err.Error())
		m.rerender()
		return m, nil
	}
	return m, switchToSession(m, newID, "cloned current session")
}

func (m *Model) runSlashTree() (tea.Model, tea.Cmd) {
	if m.cfg.Agent == nil || m.cfg.Store == nil {
		m.appendSystem("/tree unavailable: agent or store not wired")
		m.rerender()
		return m, nil
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	root, flat, cursor, err := buildSessionTree(ctx, m.cfg.Agent, m.cfg.Store, m.cfg.SessionID)
	if err != nil {
		m.appendSystem("/tree: " + err.Error())
		m.rerender()
		return m, nil
	}
	if cursor < 0 {
		cursor = 0
	}
	m.tree = &treeModal{root: root, flat: flat, cursor: cursor}
	m.rerender()
	return m, nil
}

func (m *Model) handleTreeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.tree = nil
		m.rerender()
		return m, nil
	case tea.KeyUp:
		m.tree.up()
		m.rerender()
		return m, nil
	case tea.KeyDown:
		m.tree.down()
		m.rerender()
		return m, nil
	case tea.KeyEnter:
		node, ok := m.tree.selected()
		m.tree = nil
		if !ok || node == nil {
			m.rerender()
			return m, nil
		}
		if node.Summary.ID == m.cfg.SessionID {
			m.appendSystem("/tree: already on this session")
			m.rerender()
			return m, nil
		}
		return m, switchToSession(m, node.Summary.ID, "switched via /tree")
	}
	return m, nil
}

// switchToSession is the common path for /fork, /clone, and /tree: load
// the named session, rebuild the transcript, and post a system note.
// Returned as a tea.Cmd so the actual work runs asynchronously; the TUI
// stays responsive while the store reads.
func switchToSession(m *Model, newID, note string) tea.Cmd {
	cfg := m.cfg
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		sess, err := cfg.Agent.Session(ctx, newID)
		if err != nil {
			return systemMsg("switch session: " + err.Error())
		}
		// The state we want lives in the session struct now. Replay it into
		// the TUI by sending a dedicated message — the model resets its
		// transcript when it receives this.
		return sessionSwitchedMsg{
			ID:       newID,
			Note:     note,
			Messages: sess.Messages(),
		}
	}
}

func lastUserMessageIndex(msgs []glue.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == glue.MessageRoleUser {
			return i
		}
	}
	return -1
}

func parseForkArg(arg string, max int) (int, error) {
	n, err := strconvAtoi(strings.TrimSpace(arg))
	if err != nil {
		return 0, fmt.Errorf("invalid index %q", arg)
	}
	if n < 0 || n > max {
		return 0, fmt.Errorf("index %d out of range [0, %d]", n, max)
	}
	return n, nil
}

// strconvAtoi is inlined to avoid pulling strconv into this file just
// for one use.
func strconvAtoi(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	neg := false
	i := 0
	if s[0] == '-' {
		neg = true
		i = 1
	}
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit %q", string(c))
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

func (m *Model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.picker = nil
		m.appendSystem("/resume cancelled.")
		m.rerender()
		return m, nil
	case tea.KeyUp:
		m.picker.up()
		m.rerender()
		return m, nil
	case tea.KeyDown:
		m.picker.down()
		m.rerender()
		return m, nil
	case tea.KeyEnter:
		s, ok := m.picker.selected()
		m.picker = nil
		if !ok {
			m.rerender()
			return m, nil
		}
		// Switch to the picked session and replay its transcript.
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		sess, err := m.cfg.Agent.Session(ctx, s.ID)
		if err != nil {
			m.appendSystem("/resume: " + err.Error())
			m.rerender()
			return m, nil
		}
		m.cfg.SessionID = s.ID
		m.transcript = nil
		m.detachGoalCard()
		m.appendSystem("resumed: " + s.ID)
		m.transcript = append(m.transcript, transcriptFromMessages(sess.Messages())...)
		m.turnNum = 0
		m.lastUsage = nil
		m.rerender()
		return m, nil
	}
	return m, nil
}
