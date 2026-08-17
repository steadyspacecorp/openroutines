package plugin

import "github.com/steadyspacecorp/openroutines/internal/source"

func Fetch(sourceRef, subPath, revision string) (root string, prov Source, cleanup func(), err error) {
	root, provenance, cleanup, err := source.Fetch(sourceRef, subPath, revision)
	return root, Source{Repository: provenance.Repository, Path: provenance.Path, Revision: provenance.Revision}, cleanup, err
}
