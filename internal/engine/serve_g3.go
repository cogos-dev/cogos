// serve_g3.go — G3 identity-embedding helpers for the inference gateway.
//
// This file provides the helper functions that Step 0 of G3 uses to enrich
// BoundIdentity with the WorkspaceRoot and MemoryNamespace fields from the
// identity CRD.
//
// Resolution chain (Step 0):
//  1. Load {workspaceRoot}/.cog/config/identities/{subject}.yaml via identity.LoadCRD.
//  2. Call CRDSpec.ExpressionFor("kernel") — falls back to "*" wildcard if no
//     exact match.
//  3. Populate WorkspaceRoot and MemoryNamespace from the matched expression.
//  4. On any error (missing file, parse failure): return nil — caller treats
//     the missing expression as a minimal binding.
//
// cog:// → filesystem resolution (Part A):
//
//	resolveWorkspaceRootPath converts a WorkspaceRoot cog:// URI to an absolute
//	filesystem path using ResolveURI(workspaceRoot, uri). If the URI is empty or
//	unresolvable, returns "" so the caller can fall back cleanly.
package engine

import (
	"log/slog"
	"os"

	subidentity "github.com/myrgic/cogos/pkg/substrate/identity"
)

// resolveIdentityExpression loads the Identity CRD for subject and returns
// the expression matching aud (with "*" wildcard fallback). Returns nil when:
//   - The CRD file does not exist (not an error — treat as minimal binding).
//   - The CRD fails to load or parse.
//   - No matching expression exists.
func resolveIdentityExpression(workspaceRoot, subject, aud string) *subidentity.Expression {
	if workspaceRoot == "" || subject == "" {
		return nil
	}
	crd, err := subidentity.LoadCRD(workspaceRoot, subject)
	if err != nil {
		// Missing file is expected when no identity CRD exists for the subject.
		// Log only when the file exists but fails to parse.
		if !os.IsNotExist(err) {
			slog.Debug("g3: load identity CRD failed (ignored)", "subject", subject, "err", err)
		}
		return nil
	}
	return crd.Spec.ExpressionFor(aud)
}

// resolveWorkspaceRootPath converts a WorkspaceRoot cog:// URI to an absolute
// filesystem path. Returns "" when uri is empty or the resolution fails —
// callers fall back to a neutral working directory.
func resolveWorkspaceRootPath(kernelWorkspaceRoot, uri string) string {
	if uri == "" {
		return ""
	}
	res, err := ResolveURI(kernelWorkspaceRoot, uri)
	if err != nil {
		slog.Debug("g3: resolve WorkspaceRoot URI failed (ignored)", "uri", uri, "err", err)
		return ""
	}
	return res.Path
}

// resolveMemoryNamespacePrefix converts a MemoryNamespace cog:// URI to an
// absolute filesystem path prefix used for memory-scope filtering in
// assembleContextInnerWithOpts (G3 Part B).
//
// The resolved path is the directory that all scoped documents must live
// under. Returns "" when namespace is empty or resolution fails — callers
// treat "" as "no scoping".
func resolveMemoryNamespacePrefix(workspaceRoot, namespace string) string {
	if namespace == "" {
		return ""
	}
	res, err := ResolveURI(workspaceRoot, namespace)
	if err != nil {
		slog.Debug("g3: resolve MemoryNamespace URI failed (ignored)", "namespace", namespace, "err", err)
		return ""
	}
	return res.Path
}
