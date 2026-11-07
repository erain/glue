// Token-usage aggregation and optional dollar pricing for the TUI's
// status bar and /usage command.

package tui

import "github.com/erain/glue"

// Pricing enables dollar estimates in /usage and the status bar. Wired
// from cmd/glue's --usage-*-price flags; the zero value (disabled)
// keeps the display token-only.
type Pricing struct {
	Enabled                 bool
	InputUSDPerMillion      float64
	OutputUSDPerMillion     float64
	CacheReadUSDPerMillion  float64
	CacheWriteUSDPerMillion float64
}

// CostUSD estimates the dollar cost of the given usage totals.
func (p Pricing) CostUSD(u glue.Usage) float64 {
	const tokensPerMillion = 1_000_000
	return (float64(u.InputTokens)*p.InputUSDPerMillion +
		float64(u.OutputTokens)*p.OutputUSDPerMillion +
		float64(u.CacheReadTokens)*p.CacheReadUSDPerMillion +
		float64(u.CacheWriteTokens)*p.CacheWriteUSDPerMillion) / tokensPerMillion
}

// sumTurnUsage totals the assistant-message usage across one turn's new
// messages. Returns nil when the provider reported nothing, so callers
// can distinguish "no data" from zero.
func sumTurnUsage(messages []glue.Message) *glue.Usage {
	var sum glue.Usage
	seen := false
	for _, m := range messages {
		if m.Role != glue.MessageRoleAssistant || m.Usage == nil {
			continue
		}
		seen = true
		sum.InputTokens += m.Usage.InputTokens
		sum.OutputTokens += m.Usage.OutputTokens
		sum.CacheReadTokens += m.Usage.CacheReadTokens
		sum.CacheWriteTokens += m.Usage.CacheWriteTokens
		total := m.Usage.TotalTokens
		if total == 0 {
			total = m.Usage.InputTokens + m.Usage.OutputTokens
		}
		sum.TotalTokens += total
	}
	if !seen {
		return nil
	}
	return &sum
}
