// Holds the embedded agent template.
//
// template/ in this repository is the source of truth for what every
// scaffolded agent looks like; it is compiled into the CLI binary so that
// `openroutines new` output always matches the pinned binary version.
package openroutines

import "embed"

//go:embed all:template
var TemplateFS embed.FS
