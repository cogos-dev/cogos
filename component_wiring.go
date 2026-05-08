// component_wiring.go — DI wiring for the internal/providers/component package.
//
// The component provider lives in internal/providers/component/ (Wave 1a of
// ADR-085) and reaches the component registry/indexer machinery that still
// lives at the apps/cogos root through function variables declared in that
// package. This file sets those variables at init() time.
//
// The blank import on the component package ensures its own init() — which
// registers the provider with pkg/reconcile — runs. A blank import is not
// strictly required here because the explicit references below already pull
// the package in, but is documented in ADR-085 rule 2 and kept consistent
// for future provider extractions.

package main

import (
	"github.com/myrgic/cogos/internal/providers/component"
)

func init() {
	component.LoadRegistry = func(root string) (*component.Registry, error) {
		reg, err := loadComponentRegistry(root)
		if err != nil {
			return nil, err
		}
		out := &component.Registry{
			Reconciler: component.ReconcilerConfig{
				PruneUnregistered: reg.Reconciler.PruneUnregistered,
			},
			Components: make(map[string]component.RegistryDecl, len(reg.Components)),
		}
		for path, decl := range reg.Components {
			out.Components[path] = component.RegistryDecl{
				Kind:     decl.Kind,
				Required: decl.Required,
			}
		}
		return out, nil
	}

	component.IndexComponentPaths = func(root string) ([]string, error) {
		idx, err := indexComponents(root)
		if err != nil {
			return nil, err
		}
		paths := make([]string, 0, len(idx.Components))
		for path := range idx.Components {
			paths = append(paths, path)
		}
		return paths, nil
	}

	component.LoadBlob = func(root, path string) (*component.Blob, error) {
		b, err := loadComponentBlob(root, path)
		if err != nil {
			return nil, err
		}
		return &component.Blob{
			Kind:        b.Kind,
			TreeHash:    b.TreeHash,
			CommitHash:  b.CommitHash,
			BlobHash:    b.BlobHash,
			Dirty:       b.Dirty,
			Language:    b.Language,
			BuildSystem: b.BuildSystem,
			IndexedAt:   b.IndexedAt,
		}, nil
	}

	component.EncodePath = encodePath
	component.NowISO = nowISO
}
