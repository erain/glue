package tui

import (
	"testing"

	"github.com/erain/glue"
)

func TestSumTurnUsage(t *testing.T) {
	t.Parallel()
	if got := sumTurnUsage(nil); got != nil {
		t.Fatalf("no messages should sum to nil, got %+v", got)
	}
	msgs := []glue.Message{
		{Role: glue.MessageRoleUser},
		{Role: glue.MessageRoleAssistant, Usage: &glue.Usage{InputTokens: 100, OutputTokens: 10}},
		{Role: glue.MessageRoleTool},
		{Role: glue.MessageRoleAssistant, Usage: &glue.Usage{InputTokens: 200, OutputTokens: 20, TotalTokens: 220}},
	}
	got := sumTurnUsage(msgs)
	if got == nil {
		t.Fatal("expected usage")
	}
	if got.InputTokens != 300 || got.OutputTokens != 30 {
		t.Fatalf("sum = %+v", got)
	}
	// First message had no TotalTokens: falls back to in+out.
	if got.TotalTokens != 110+220 {
		t.Fatalf("TotalTokens = %d, want 330", got.TotalTokens)
	}
}

func TestPricingCostUSD(t *testing.T) {
	t.Parallel()
	p := Pricing{
		Enabled:            true,
		InputUSDPerMillion: 2, OutputUSDPerMillion: 10,
		CacheReadUSDPerMillion: 0.5,
	}
	u := glue.Usage{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 2_000_000}
	// 1M*2 + 0.1M*10 + 2M*0.5 = 2 + 1 + 1 = 4 USD
	if got := p.CostUSD(u); got != 4 {
		t.Fatalf("CostUSD = %v, want 4", got)
	}
}

func TestHandleTurnDoneAccumulatesUsage(t *testing.T) {
	t.Parallel()
	m := &Model{turn: newTurnState()}
	m.handleTurnDone(turnDoneMsg{Usage: &glue.Usage{InputTokens: 10, OutputTokens: 1, TotalTokens: 11}})
	m.turn = newTurnState()
	m.handleTurnDone(turnDoneMsg{Usage: &glue.Usage{InputTokens: 20, OutputTokens: 2, TotalTokens: 22}})
	if m.lastUsage == nil || m.lastUsage.InputTokens != 20 {
		t.Fatalf("lastUsage = %+v, want the newest turn", m.lastUsage)
	}
	if m.sessionUsage.InputTokens != 30 || m.sessionUsage.TotalTokens != 33 {
		t.Fatalf("sessionUsage = %+v, want cumulative", m.sessionUsage)
	}
}
