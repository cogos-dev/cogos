// Package projection holds the cross-package types emitted by the Projection
// Compiler (ADR projection-compiler-primitive).
//
// The Projection Compiler is the depth-axis primitive that compiles
// reflective cogdocs (verbatim operator articulations) into structured
// Reconcilable-readable events. It implements the seven-method
// pkg/substrate/reconcile.Reconcilable contract.
//
// This package intentionally contains only the data types. Compilation
// logic lives in internal/engine/projection_compiler.go.
//
// Types follow the substrate rename convention: no redundant "Projection"
// prefix when the package name already supplies the namespace. Cross-package
// callers refer to projection.Event and projection.FrictionEvent.
package projection
