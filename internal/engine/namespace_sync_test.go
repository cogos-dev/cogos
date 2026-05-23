package engine_test

// namespace_sync_test.go — CI guard that all manual namespace lists never drift
// from the canonical pkg/substrate/uri.Namespaces table (issues #173, #183).
//
// Three manual copies of the namespace set must stay in sync:
//
//	pkg/substrate/uri/namespace.go    — canonical definition (Namespaces map)
//	sdk/uri.go                        — copy (sdk.Namespaces, lines tagged "SINGLE SOURCE")
//	internal/engine/uri_registry.go   — isProjectionNamespace switch statement
//
// The sdk module cannot import pkg/substrate/uri directly (separate Go modules;
// importing would create an import cycle via the main module). The engine
// package's isProjectionNamespace predicate is likewise a manually maintained
// copy pending #166 (direct namespace-registry import).
//
// Note: as of the ADR-100 canonical-vs-shim inversion (2026-05-23), the
// canonical Namespaces map moved from pkg/uri/namespace.go to
// pkg/substrate/uri/namespace.go. The legacy pkg/uri import path is now a
// thin re-export shim aliasing the substrate canonical surface.
//
// This test parses all three source files with go/ast, extracts the namespace
// keys from each, and asserts they are identical sets. It runs as part of
// `go test ./internal/engine/` and will fail CI if a key is added to one list
// but not the others.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// extractNamespaceKeys parses a Go source file and returns the set of string
// keys declared in the first map[string]bool composite literal whose
// enclosing var is named "Namespaces".
func extractNamespaceKeys(t *testing.T, srcPath string) map[string]struct{} {
	t.Helper()

	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read %s: %v", srcPath, err)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", srcPath, err)
	}

	keys := make(map[string]struct{})

	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		// Look for: var Namespaces = map[string]bool{ ... }
		for i, name := range vs.Names {
			if name.Name != "Namespaces" {
				continue
			}
			if i >= len(vs.Values) {
				continue
			}
			compLit, ok := vs.Values[i].(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, elt := range compLit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				lit, ok := kv.Key.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				// Strip surrounding quotes.
				key := lit.Value
				if len(key) >= 2 && key[0] == '"' && key[len(key)-1] == '"' {
					key = key[1 : len(key)-1]
				}
				keys[key] = struct{}{}
			}
		}
		return true
	})

	if len(keys) == 0 {
		t.Fatalf("no Namespaces keys found in %s — check that the var is still named 'Namespaces'", srcPath)
	}
	return keys
}

// repoRoot returns the absolute path to the repository root by walking up from
// this test file's location.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file is .../internal/engine/namespace_sync_test.go
	// walk up two directories
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("cannot locate repo root from %s: go.mod not found at %s", file, root)
	}
	return root
}

func TestNamespaces_SDKAndPkgURI_NeverDrift(t *testing.T) {
	root := repoRoot(t)

	pkgURIFile := filepath.Join(root, "pkg", "substrate", "uri", "namespace.go")
	sdkFile := filepath.Join(root, "sdk", "uri.go")

	pkgKeys := extractNamespaceKeys(t, pkgURIFile)
	sdkKeys := extractNamespaceKeys(t, sdkFile)

	// Check: every pkg/substrate/uri key must be in sdk.
	for k := range pkgKeys {
		if _, ok := sdkKeys[k]; !ok {
			t.Errorf("namespace %q is in pkg/substrate/uri.Namespaces but missing from sdk.Namespaces\n"+
				"  add it to sdk/uri.go Namespaces map", k)
		}
	}

	// Check: every sdk key must be in pkg/substrate/uri.
	for k := range sdkKeys {
		if _, ok := pkgKeys[k]; !ok {
			t.Errorf("namespace %q is in sdk.Namespaces but missing from pkg/substrate/uri.Namespaces\n"+
				"  add it to pkg/substrate/uri/namespace.go Namespaces map", k)
		}
	}

	if t.Failed() {
		t.Logf("pkg/substrate/uri keys (%d): %v", len(pkgKeys), sortedKeys(pkgKeys))
		t.Logf("sdk keys              (%d): %v", len(sdkKeys), sortedKeys(sdkKeys))
	}
}

// extractProjectionNamespaceKeys parses a Go source file and returns the set
// of string literals enumerated in the switch statement inside the function
// named "isProjectionNamespace".  The function body is expected to be a single
// switch with one or more case clauses whose List entries are string BasicLits
// (possibly comma-joined in source: case "a", "b", "c":).
func extractProjectionNamespaceKeys(t *testing.T, srcPath string) map[string]struct{} {
	t.Helper()

	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read %s: %v", srcPath, err)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", srcPath, err)
	}

	keys := make(map[string]struct{})
	found := false

	ast.Inspect(f, func(n ast.Node) bool {
		// Locate the function declaration named "isProjectionNamespace".
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "isProjectionNamespace" {
			return true
		}
		found = true

		// Walk its body to find the switch statement.
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			sw, ok := inner.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			// Each case clause may list multiple expressions (comma-separated).
			for _, stmt := range sw.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok || cc.List == nil {
					continue // skip default clause
				}
				for _, expr := range cc.List {
					lit, ok := expr.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					key := lit.Value
					if len(key) >= 2 && key[0] == '"' && key[len(key)-1] == '"' {
						key = key[1 : len(key)-1]
					}
					keys[key] = struct{}{}
				}
			}
			return false // no need to descend further into the switch
		})

		return false // no need to visit siblings
	})

	if !found {
		t.Fatalf("function isProjectionNamespace not found in %s", srcPath)
	}
	if len(keys) == 0 {
		t.Fatalf("no case values found in isProjectionNamespace in %s — check the switch is still present", srcPath)
	}
	return keys
}

func TestNamespaces_IsProjectionNamespace_NeverDrift(t *testing.T) {
	root := repoRoot(t)

	pkgURIFile := filepath.Join(root, "pkg", "substrate", "uri", "namespace.go")
	uriRegistryFile := filepath.Join(root, "internal", "engine", "uri_registry.go")

	pkgKeys := extractNamespaceKeys(t, pkgURIFile)
	switchKeys := extractProjectionNamespaceKeys(t, uriRegistryFile)

	// Check: every pkg/substrate/uri key must be in isProjectionNamespace.
	for k := range pkgKeys {
		if _, ok := switchKeys[k]; !ok {
			t.Errorf("namespace %q is in pkg/substrate/uri.Namespaces but missing from isProjectionNamespace in uri_registry.go\n"+
				"  add it to the switch in internal/engine/uri_registry.go:isProjectionNamespace", k)
		}
	}

	// Check: every isProjectionNamespace key must be in pkg/substrate/uri.
	for k := range switchKeys {
		if _, ok := pkgKeys[k]; !ok {
			t.Errorf("namespace %q is in isProjectionNamespace but missing from pkg/substrate/uri.Namespaces\n"+
				"  add it to pkg/substrate/uri/namespace.go Namespaces map", k)
		}
	}

	if t.Failed() {
		t.Logf("pkg/substrate/uri keys (%d): %v", len(pkgKeys), sortedKeys(pkgKeys))
		t.Logf("isProjectionNamespace  (%d): %v", len(switchKeys), sortedKeys(switchKeys))
	}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// simple insertion sort (small map, test helper)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
