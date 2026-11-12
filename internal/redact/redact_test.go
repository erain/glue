// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package redact

import (
	"strings"
	"testing"
)

func TestRedactTokens(t *testing.T) {
	for _, secret := range []string{
		"ghp_" + strings.Repeat("a1B2", 9),
		"gho_" + strings.Repeat("Z9y8", 9),
		"github_pat_11ABCDEFG0123456789_" + strings.Repeat("x", 59),
		"AIza" + strings.Repeat("Sy-_0aB", 5),
		"sk-proj-" + strings.Repeat("Ab12", 12),
		"sk-or-v1-" + strings.Repeat("0f", 32),
	} {
		for _, in := range []string{secret, "token=" + secret, `{"key":"` + secret + `"}`, "Authorization: Bearer " + secret + "\n"} {
			got := String(in)
			if strings.Contains(got, secret) || !strings.Contains(got, Replacement) {
				t.Errorf("String(%q) = %q, want secret redacted", in, got)
			}
		}
	}
}

func TestRedactKeepsLookalikes(t *testing.T) {
	for _, in := range []string{
		"/usr/local/bin/ax-task-runner",
		"risk-assessment-for-the-quarterly-report",
		"task-1234567890abcdefghij",
		"desk-organizer-instructions-v2",
		"ghp_short",
		"my_ghp_" + strings.Repeat("a", 30),
		"AIzaShort",
	} {
		if got := String(in); got != in {
			t.Errorf("String(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestRedactRegisteredValues(t *testing.T) {
	Register("plain-registered-value")
	if got := String("x plain-registered-value y"); got != "x "+Replacement+" y" {
		t.Errorf("registered value not redacted: %q", got)
	}
}
