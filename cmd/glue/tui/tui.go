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
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())

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

	// Cancel-on-second-ctrl-c semantics
	armedQuit bool

	// Mutex protects send (set once after construction, then read-only).
	mu sync.Mutex
}

func newModel(ctx context.Context, cfg Config) *Model {
	ta := textarea.New()
	ta.Placeholder = "Type a message, or /help"
	ta.Prompt = "│ "
	ta.CharLimit = 0
	ta.SetWidth(80)
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.Focus()

	vp := viewport.New(80, 20)

	return &Model{
		ctx:        ctx,
		cfg:        cfg,
		viewport:   vp,
		input:      ta,
		transcript: nil,
		historyPos: -1,
	}
}

func (m *Model) Init() tea.Cmd {
	// Seed the transcript with a greeting line.
	m.appendSystem(fmt.Sprintf("glue · %s · %s/%s · %s",
		m.cfg.SessionID, providerOrDefault(m.cfg.Provider), modelOrDefault(m.cfg.Model), workOrDot(m.cfg.WorkDir)))
	m.appendSystem("Type a message and press Enter. /help for commands. Ctrl+C twice to quit.")
	return tea.Batch(textarea.Blink)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
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

	case textDeltaMsg:
		m.handleTextDelta(string(msg))
		m.rerender()
		return m, nil

	case toolStartMsg:
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

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	headerH := 1
	statusH := 1
	inputH := m.inputHeight()
	// Borders on the input box add 2.
	bottomH := inputH + 2
	if m.pending != nil {
		bottomH += m.permBoxHeight()
	}
	bodyH := m.height - headerH - statusH - bottomH
	if bodyH < 3 {
		bodyH = 3
	}
	m.viewport.Width = m.width
	m.viewport.Height = bodyH
	m.input.SetWidth(m.width - 2) // textarea adds its own gutter
}

func (m *Model) inputHeight() int {
	// Grow with content up to 8 rows, then scroll inside the textarea.
	const min, max = 3, 8
	h := m.input.LineCount()
	if h < min {
		h = min
	}
	if h > max {
		h = max
	}
	m.input.SetHeight(h)
	return h
}

func (m *Model) permBoxHeight() int {
	// Header line + buttons + ~2 lines of context = ~5 with borders.
	return 5
}

func (m *Model) rerender() {
	// Re-flow the transcript into a single string for the viewport.
	var b strings.Builder
	width := m.viewport.Width
	if width == 0 {
		width = 80
	}
	for i, it := range m.transcript {
		if i > 0 {
			b.WriteByte('\n')
			b.WriteByte('\n')
		}
		b.WriteString(it.render(width))
	}
	m.viewport.SetContent(b.String())
	m.viewport.GotoBottom()
	m.layout()
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
	box := inputBoxStyle.Width(m.width - 2).Render(m.input.View())
	if m.pending == nil {
		return box
	}
	perm := m.renderPermPrompt()
	return lipgloss.JoinVertical(lipgloss.Left, perm, box)
}

func (m *Model) renderPermPrompt() string {
	p := m.pending
	target := p.req.Target
	if target == "" {
		target = "(unspecified)"
	}
	head := toolWarning.Render(fmt.Sprintf("Permission requested: %s %s", p.req.Tool, target))
	hint := keyHint.Render("  [a]llow once    [s]ession    [t]target    [n]o    [esc] deny")
	return permBox.Width(m.width - 2).Render(head + "\n" + hint)
}

func (m *Model) statusView() string {
	var parts []string
	parts = append(parts, fmt.Sprintf("turn %d", m.turnNum))
	if m.lastUsage != nil {
		parts = append(parts, fmt.Sprintf("%d in / %d out",
			m.lastUsage.InputTokens, m.lastUsage.OutputTokens))
	}
	if m.turn != nil {
		parts = append(parts, toolWarning.Render("running…"))
	}
	parts = append(parts, keyHint.Render("/help · Ctrl+C twice to quit"))
	return statusStyle.Render(strings.Join(parts, "  ·  "))
}

// ---------- helpers: input handling ----------

func (m *Model) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		// Shift+Enter is the textarea's "newline"; bare Enter submits.
		// bubbles/textarea routes both through Enter; we treat Alt+Enter
		// as the newline because Shift+Enter is not portable across terms.
		if msg.Alt {
			break // fall through to textarea
		}
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
		m.appendSystem("a turn is already running; Ctrl+C to cancel.")
		m.rerender()
		return m, nil
	}

	m.transcript = append(m.transcript, transcriptItem{Kind: itemUser, Text: text})
	m.startTurn(text)
	m.rerender()
	return m, nil
}

func (m *Model) handleSlash(cmd slashCommand) (tea.Model, tea.Cmd) {
	switch cmd.Name {
	case "exit", "quit", "q":
		return m, tea.Quit
	case "help":
		m.appendSystem("commands:\n" + describeCommands())
		m.rerender()
		return m, nil
	case "clear":
		m.cfg.SessionID = "tui:" + shortID()
		m.appendSystem("new session: " + m.cfg.SessionID)
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
			m.appendSystem("tools:\n  " + strings.Join(names, "\n  "))
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
	m.turn = nil
	m.turnNum++
}

// ---------- agent goroutine ----------

func (m *Model) startTurn(prompt string) {
	if m.send == nil {
		// Should not happen — Run installs send before p.Run.
		m.appendSystem("tui: program send not initialised")
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	state := newTurnState()
	state.cancel = cancel
	m.turn = state

	send := m.send
	bridge := m.perm
	session, err := m.cfg.Agent.Session(ctx, m.cfg.SessionID)
	if err != nil {
		cancel()
		m.turn = nil
		m.appendSystem("session error: " + err.Error())
		return
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
