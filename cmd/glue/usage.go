// Token-usage accounting and optional cost estimation, shared by the
// run and connect subcommands.

package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/erain/glue"
	// Register the shipped providers so they resolve through the
	// providers registry by name (--provider). Importing for side
)

type usageSummary struct {
	HasUsage         bool
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalTokens      int64
}

type usagePricing struct {
	Enabled                 bool
	InputUSDPerMillion      float64
	OutputUSDPerMillion     float64
	CacheReadUSDPerMillion  float64
	CacheWriteUSDPerMillion float64
}

func summarizeUsage(messages []glue.Message) usageSummary {
	var summary usageSummary
	for _, message := range messages {
		if message.Role != glue.MessageRoleAssistant || message.Usage == nil {
			continue
		}
		usage := message.Usage
		summary.HasUsage = true
		summary.InputTokens += usage.InputTokens
		summary.OutputTokens += usage.OutputTokens
		summary.CacheReadTokens += usage.CacheReadTokens
		summary.CacheWriteTokens += usage.CacheWriteTokens
		total := usage.TotalTokens
		if total == 0 {
			total = usage.InputTokens + usage.OutputTokens
		}
		summary.TotalTokens += total
	}
	return summary
}

func registerUsagePricingFlags(flags *flag.FlagSet) *usagePricing {
	var pricing usagePricing
	flags.Float64Var(&pricing.InputUSDPerMillion, "usage-input-price", 0, "USD per 1M input tokens for --usage cost estimates")
	flags.Float64Var(&pricing.OutputUSDPerMillion, "usage-output-price", 0, "USD per 1M output tokens for --usage cost estimates")
	flags.Float64Var(&pricing.CacheReadUSDPerMillion, "usage-cache-read-price", 0, "USD per 1M cache-read tokens for --usage cost estimates")
	flags.Float64Var(&pricing.CacheWriteUSDPerMillion, "usage-cache-write-price", 0, "USD per 1M cache-write tokens for --usage cost estimates")
	return &pricing
}

func markUsagePricingFlagState(flags *flag.FlagSet, pricing *usagePricing) {
	flags.Visit(func(flag *flag.Flag) {
		switch flag.Name {
		case "usage-input-price", "usage-output-price", "usage-cache-read-price", "usage-cache-write-price":
			pricing.Enabled = true
		}
	})
}

func validateUsagePricing(pricing usagePricing) error {
	for name, value := range map[string]float64{
		"--usage-input-price":       pricing.InputUSDPerMillion,
		"--usage-output-price":      pricing.OutputUSDPerMillion,
		"--usage-cache-read-price":  pricing.CacheReadUSDPerMillion,
		"--usage-cache-write-price": pricing.CacheWriteUSDPerMillion,
	} {
		if value < 0 {
			return fmt.Errorf("%s must be non-negative", name)
		}
	}
	return nil
}

func usagePricingEnabled(pricing usagePricing) bool {
	return pricing.Enabled
}

func estimateUsageCostUSD(summary usageSummary, pricing usagePricing) float64 {
	const tokensPerMillion = 1_000_000
	return (float64(summary.InputTokens)*pricing.InputUSDPerMillion +
		float64(summary.OutputTokens)*pricing.OutputUSDPerMillion +
		float64(summary.CacheReadTokens)*pricing.CacheReadUSDPerMillion +
		float64(summary.CacheWriteTokens)*pricing.CacheWriteUSDPerMillion) / tokensPerMillion
}

func writeUsageSummary(w io.Writer, summary usageSummary, pricing usagePricing) {
	if !summary.HasUsage {
		return
	}
	parts := []string{
		fmt.Sprintf("input=%d", summary.InputTokens),
		fmt.Sprintf("output=%d", summary.OutputTokens),
	}
	if summary.CacheReadTokens != 0 {
		parts = append(parts, fmt.Sprintf("cache_read=%d", summary.CacheReadTokens))
	}
	if summary.CacheWriteTokens != 0 {
		parts = append(parts, fmt.Sprintf("cache_write=%d", summary.CacheWriteTokens))
	}
	parts = append(parts, fmt.Sprintf("total=%d", summary.TotalTokens))
	if usagePricingEnabled(pricing) {
		parts = append(parts, fmt.Sprintf("cost_usd=%.6f", estimateUsageCostUSD(summary, pricing)))
	}
	fmt.Fprintf(w, "usage: %s\n", strings.Join(parts, " "))
}
