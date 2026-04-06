//go:build !coguri

// uri_v2_stub.go — stub for URIRegistry when coguri library is unavailable.
//
// mcp_server.go references URIRegistry under the mcpserver build tag.
// When the coguri library isn't present (no coguri build tag), this stub
// provides a nil-valued placeholder so the package compiles cleanly.
// The nil check in mcp_server.go (if URIRegistry != nil) ensures the
// Resolve method is never actually called.
package main

import "context"

// uriContent mirrors the subset of coguri.Content used by mcp_server.go.
type uriContent struct {
	Metadata map[string]any
}

// uriRegistryStub provides the Resolve method signature needed by mcp_server.go.
type uriRegistryStub struct{}

// Resolve satisfies the compiler. Never called at runtime (URIRegistry is nil).
func (r *uriRegistryStub) Resolve(_ context.Context, _ string) (*uriContent, error) {
	return nil, nil
}

// URIRegistry is nil when the coguri library is not linked.
var URIRegistry *uriRegistryStub
