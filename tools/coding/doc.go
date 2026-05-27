// Package coding assembles the reusable local coding-agent tool bundle.
//
// The package is intentionally a thin SDK layer over the lower-level
// filesystem, shell, git, and executor primitives. Products such as Peggy
// should configure this package instead of owning coding-agent tool wiring
// directly.
package coding
