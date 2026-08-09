package plugin

import "github.com/steadyspacecorp/openroutines/internal/source"

// Acquires a plugin source: a local directory, a git URL, or an
// owner/repo GitHub shorthand. Both go through a temporary clone, so the
// returned bundle root always corresponds to the recorded provenance.
// revision pins the checkout; empty means the clone's head.
func Fetch(sourceRef, subPath, revision string) (root string, prov Source, cleanup func(), err error) {
	root, provenance, cleanup, err := source.Fetch(sourceRef, subPath, revision)
	return root, Source{Repository: provenance.Repository, Path: provenance.Path, Revision: provenance.Revision}, cleanup, err
}
