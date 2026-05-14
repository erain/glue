package main

import _ "embed"

// systemPrompt is the canonical glue-review system prompt — a single
// embedded file at prompts/default.md. We deliberately do not ship a
// versioned catalog: there's one product shape (one sticky comment per
// PR with a fenced ```markdown fix block), and exposing a flag to swap
// it out invites users into the prompt-iteration business when they
// should be in the receiving-fix-instructions business.
//
//go:embed prompts/default.md
var systemPrompt string
