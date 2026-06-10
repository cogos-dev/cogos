// serve_conv_uri.go — HTTP handler for cog:conversations URI dereference.
//
//	GET /v1/uri/resolve?uri=cog:conversations/…
//
// This handler extends the existing /v1/resolve path (which maps cog: URIs to
// filesystem paths) with conversations-specific resolution.  When the uri=
// parameter is a cog:conversations/… URI, the handler delegates to the wired
// ConversationsResolver and returns the resolved slice as JSON.  All other
// cog: URIs fall through to the existing /v1/resolve handler unchanged.
//
// The conversations resolver is injected via SetConversationsResolver so the
// engine package does not import internal/conversations (circular import
// avoidance).  If no resolver is wired the handler returns 501 with a clear
// error.
//
// Route registration lives in serve.go (NewServer).
package engine

import (
	"context"
	"net/http"
	"os"
	"strings"
)

// ConversationsResolver is the minimal interface the HTTP handler needs from
// the conversations package. Implemented by the daemonURIResolver adapter in
// cmd/cogos/z_conversations_wire.go, which delegates to
// conversations.Provider.ResolveURI on the daemon's provider singleton.
type ConversationsResolver interface {
	// ResolveURI parses and resolves a cog:conversations/… URI.
	// Returns any — the concrete type is *conversations.ResolvedSlice;
	// the handler marshals it to JSON without needing the concrete type.
	ResolveURI(ctx context.Context, uri string) (any, error)
}

// conversationsResolver is the kernel-global resolver wired at boot.
// Nil until SetConversationsResolver is called.
var conversationsResolver ConversationsResolver

// SetConversationsResolver wires the conversations resolver. Call once at boot
// from the daemon binary's wiring file (cmd/cogos/z_conversations_wire.go).
// Thread-safe: the variable is written once at init() before the server
// starts listening.
//
// NOTE for wirers: this must be called from a package that is actually linked
// into the binary that serves /v1/uri/resolve (the kernel daemon, cmd/cogos).
// The repo-root package main is the legacy cog CLI and does NOT boot this
// server — wiring placed there is silently dead in the daemon.
func SetConversationsResolver(r ConversationsResolver) {
	conversationsResolver = r
}

// WiredConversationsResolver returns the currently wired resolver, or nil when
// none has been set. Exposed so binary-assembly regression tests (e.g. in
// cmd/cogos) can assert that the standard wiring sequence actually reaches
// this engine seam — unit tests of the resolver alone cannot catch a missing
// SetConversationsResolver call in a given binary.
func WiredConversationsResolver() ConversationsResolver {
	return conversationsResolver
}

// handleURIResolve resolves a cog: URI.
//
// For cog:conversations/… URIs: delegates to the wired ConversationsResolver
// and returns the resolved slice as JSON with response metadata.
// For all other cog: URIs: falls through to the existing /v1/resolve behaviour
// (filesystem path lookup).
//
//	GET /v1/uri/resolve?uri=cog:conversations/claude-code/abc123?q=harness&res=full
//
//	200 → resolved slice (conversations) or { uri, path, fragment, exists } (other)
//	400 → { error }
//	501 → { error } when no conversations resolver wired
func (s *Server) handleURIResolve(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("uri")
	if uri == "" {
		writeJSONResp(w, http.StatusBadRequest, map[string]string{"error": "uri parameter required"})
		return
	}

	// Dispatch conversations URIs to the conversations resolver.
	if strings.HasPrefix(uri, "cog:conversations") {
		if conversationsResolver == nil {
			writeJSONResp(w, http.StatusNotImplemented, map[string]string{
				"error": "conversations resolver not wired — start the kernel with the conversations provider loaded",
			})
			return
		}
		result, err := conversationsResolver.ResolveURI(r.Context(), uri)
		if err != nil {
			writeJSONResp(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSONResp(w, http.StatusOK, result)
		return
	}

	// All other cog: URIs → filesystem path lookup (mirrors handleResolve).
	res, err := ResolveURI(s.cfg.WorkspaceRoot, uri)
	if err != nil {
		writeJSONResp(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	_, statErr := os.Stat(res.Path)
	writeJSONResp(w, http.StatusOK, map[string]any{
		"uri":      uri,
		"path":     res.Path,
		"fragment": res.Fragment,
		"exists":   statErr == nil,
	})
}
