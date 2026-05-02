// Package loop contains Glue's provider-agnostic agent loop.
//
// The loop streams assistant responses from a provider, executes requested
// tools, appends tool results, and repeats until the provider stops or the
// context is canceled. It must not depend on the public glue package,
// provider packages, stores, CLI code, or Markdown context discovery.
//
// This file is the package marker for the bootstrap scaffold. The concrete
// loop entry point and types are added by later issues.
package loop
