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

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/erain/glue"
)

// Config wires the TUI to a glue agent. Provider/Model/WorkDir are
// display-only — the agent already owns them, the TUI just labels them.
type Config struct {
	Agent         *glue.Agent
	SessionID     string
	Provider      string
	Model         string
	WorkDir       string
	Tools         []glue.Tool       // already registered on the agent; for /tools listing
	BasePermission glue.Permission  // wrapped under permissionBridge so the TUI can prompt first
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
	// agent's options.
	m.send = p.Send
	m.perm = newPermissionBridge(p.Send)

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

	// Permission bridge — created in Run, applied per-prompt via WithPermission.
	perm *permissionBridge

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
	lastUsage *glue.Usage

	// Input history (in-process; not persisted)
	history    []string
	historyPos int // -1 means "not browsing"

	// firstChunkSeen flips true on the first text delta or tool event of
	// the active turn. Used to gate "thinking…" vs the actual state name
	// in the status bar.
	firstChunkSeen bool

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
	body := strings.Join([]string{
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
	}, "\n")
	m.transcript = append(m.transcript, transcriptItem{
		Kind:       itemBlock,
		BlockTitle: "Welcome to glue",
		BlockBody:  body,
	})
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Glamour needs a width; we compute the assistant-render width to
		// match the transcript body so wrapping aligns with what the user sees.
		if m.md == nil {
			m.md = newMarkdownRenderer(msg.Width)
		} else {
			m.md.Resize(msg.Width)
		}
		m.layout()
		m.ready = true
		m.rerender()
		return m, nil

	case tea.KeyMsg:
		// Permission prompt takes keyboard precedence.
		if m.pending != nil {
			return m.handlePermissionKey(msg)
		}
		return m.handleInputKey(msg)

	case tea.MouseMsg:
		var c tea.Cmd
		m.viewport, c = m.viewport.Update(msg)
		return m, c

	case spinner.TickMsg:
		// Spinner only animates during an in-flight turn; once the turn
		// ends, stop ticking instead of consuming Update cycles forever.
		if m.turn == nil {
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
		m.pending = &permPending{req: msg.Req, respond: msg.Respond}
		m.markPendingTool(msg.Req)
		m.rerender()
		return m, nil

	case turnDoneMsg:
		m.handleTurnDone(msg)
		m.rerender()
		return m, nil

	case systemMsg:
		m.appendSystem(string(msg))
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

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	headerH := 1
	statusH := 1
	inputH := m.inputHeight()
	// Border (1 top + 1 bottom) + padding (0,1) on the input box adds 2 rows.
	bottomH := inputH + 2
	bodyH := m.height - headerH - statusH - bottomH
	if bodyH < 3 {
		bodyH = 3
	}
	m.viewport.Width = m.width
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
	// Permission prompts render inside the relevant tool card now, so the
	// input box only carries the textarea. Cap the visual width on wide
	// terminals so it doesn't feel disconnected from the chat above.
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
	if m.pending != nil {
		parts = append(parts, toolWarning.Render("permission needed"))
	}
	if !m.viewport.AtBottom() {
		parts = append(parts, keyHint.Render("↓ more below"))
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
	// Esc cancels the current turn if one is in flight. Otherwise it's a
	// no-op (no surprises for users who hit it by reflex).
	if msg.Type == tea.KeyEsc {
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
	m.layout()
	return m, c
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

	m.transcript = append(m.transcript, transcriptItem{Kind: itemUser, Text: text})
	cmd := m.startTurn(text)
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
