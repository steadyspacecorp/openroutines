// Package openroutines holds the embedded agent template.
//
// template/ in this repository is the source of truth for what every
// scaffolded agent looks like; it is compiled into the CLI binary so that
// `openroutines scaffold` output always matches the pinned binary version.
package openroutines

import "embed"

// TemplateFS contains the agent skeleton stamped out by `openroutines scaffold`.
//
//go:embed all:template
var TemplateFS embed.FS
