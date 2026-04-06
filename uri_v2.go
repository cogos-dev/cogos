//go:build coguri

// uri_v2.go — coguri-based URI resolution for CogOS v3 kernel
//
// This file bridges the coguri library (ADR-067) into the v3 kernel.
// It registers the cog: scheme resolver using the existing projection
// mappings and provides a drop-in replacement for ResolveURI.
//
// The coguri library handles parsing (multi-scheme, workspace, node,
// query params, fragments). This file provides the filesystem resolution
// that turns a parsed cog: URI into an absolute path.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cogos.dev/lib/coguri"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// URIRegistry is the kernel's multi-scheme resolver registry.
// Initialized by InitURIRegistry() at startup.
var URIRegistry *coguri.Registry

// InitURIRegistry creates the resolver registry and registers all built-in
// scheme resolvers. Called once at kernel startup.
func InitURIRegistry(workspaceRoot string) {
	URIRegistry = coguri.NewRegistry()

	// Register the cog: scheme resolver (filesystem projections)
	URIRegistry.Register(&cogSchemeResolver{root: workspaceRoot})

	// Register sha256: scheme resolver (content-addressed lookup)
	URIRegistry.Register(&sha256SchemeResolver{root: workspaceRoot})
}

// ResolveURIv2 resolves any URI through the registry.
// This is the new entry point — replaces ResolveURI for multi-scheme support.
func ResolveURIv2(ctx context.Context, raw string) (*coguri.Content, error) {
	if URIRegistry == nil {
		return nil, fmt.Errorf("URI registry not initialized")
	}
	return URIRegistry.Resolve(ctx, raw)
}

// ── cog: scheme resolver ─────────────────────────────────────────────────────

// cogSchemeResolver resolves cog: URIs to filesystem paths using projections.
type cogSchemeResolver struct {
	root string
}

func (r *cogSchemeResolver) Scheme() string { return "cog" }

func (r *cogSchemeResolver) CanResolve(uri *coguri.URI) bool {
	// Handle both local (cog:projection/path) and legacy (cog://projection/path)
	projection := uri.Projection

	// Legacy compat: if Workspace is set but matches a known projection,
	// treat it as a local reference (cog://mem/x → cog:mem/x)
	if projection == "" && uri.Workspace != "" {
		if _, ok := projections[uri.Workspace]; ok {
			return true
		}
	}

	_, ok := projections[projection]
	return ok
}

func (r *cogSchemeResolver) Resolve(ctx context.Context, uri *coguri.URI) (*coguri.Content, error) {
	projection := uri.Projection
	path := uri.Path

	// Legacy compat: cog://mem/semantic/x → projection=mem, path=semantic/x
	if projection == "" && uri.Workspace != "" {
		if _, ok := projections[uri.Workspace]; ok {
			projection = uri.Workspace
			// The path for legacy URIs needs to be reconstructed
			// cog://mem/semantic/x parses as workspace=mem, projection=semantic, path=x
			if uri.Projection != "" {
				if uri.Path != "" {
					path = uri.Projection + "/" + uri.Path
				} else {
					path = uri.Projection
				}
			}
		}
	}

	proj, ok := projections[projection]
	if !ok {
		return nil, fmt.Errorf("unknown cog: projection %q", projection)
	}

	// Handle ?ref= for git version pinning
	if ref := uri.Ref(); ref != "" {
		return r.resolveAtRef(proj, path, ref, uri)
	}

	fsPath, err := resolveProjection(r.root, proj, path)
	if err != nil {
		return nil, err
	}

	content := &coguri.Content{
		URI:         uri.String(),
		ContentType: detectContentType(fsPath),
		Metadata: map[string]any{
			"path":     fsPath,
			"fragment": uri.Fragment,
		},
	}

	// Read file content if it exists
	if data, err := os.ReadFile(fsPath); err == nil {
		content.Data = data
	}

	// Handle ?format= for sparse responses
	if format := uri.Format(); format != "" {
		return applyFormat(content, format)
	}

	return content, nil
}

// resolveAtRef resolves a URI at a specific git ref using go-git.
// It performs the equivalent of `git show {ref}:{path}`, returning the file
// content from the specified commit/branch/tag.
func (r *cogSchemeResolver) resolveAtRef(proj *Projection, path, ref string, uri *coguri.URI) (*coguri.Content, error) {
	fsPath, err := resolveProjection(r.root, proj, path)
	if err != nil {
		return nil, err
	}

	// Convert absolute path to workspace-relative for git tree lookup.
	relPath, err := filepath.Rel(r.root, fsPath)
	if err != nil {
		return nil, fmt.Errorf("cannot relativize path: %w", err)
	}
	// go-git uses forward slashes in tree paths.
	relPath = filepath.ToSlash(relPath)

	// Open the git repository at the workspace root.
	repo, err := git.PlainOpen(r.root)
	if err != nil {
		return nil, fmt.Errorf("open git repo at %s: %w", r.root, err)
	}

	// Resolve the ref to a commit hash. plumbing.Revision handles:
	// - branch names (main, feature/x)
	// - tag names (v1.0)
	// - commit hashes (abc1234)
	// - relative refs (HEAD~3, main^2)
	hash, err := repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		return nil, fmt.Errorf("resolve git ref %q: %w", ref, err)
	}

	// Get the commit object.
	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return nil, fmt.Errorf("get commit %s for ref %q: %w", hash, ref, err)
	}

	// Get the tree at that commit.
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("get tree for commit %s: %w", hash, err)
	}

	// Look up the file in the tree.
	file, err := tree.File(relPath)
	if err != nil {
		return nil, fmt.Errorf("file %q not found at ref %q (%s): %w", relPath, ref, hash, err)
	}

	// Read the file content.
	contents, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("read file %q at ref %q: %w", relPath, ref, err)
	}

	return &coguri.Content{
		URI:         uri.String(),
		ContentType: detectContentType(fsPath),
		Data:        []byte(contents),
		Metadata: map[string]any{
			"path":     fsPath,
			"rel_path": relPath,
			"ref":      ref,
			"commit":   hash.String(),
			"fragment": uri.Fragment,
		},
	}, nil
}

// detectContentType guesses content type from file extension.
func detectContentType(path string) string {
	switch {
	case strings.HasSuffix(path, ".cog.md"), strings.HasSuffix(path, ".md"):
		return "markdown"
	case strings.HasSuffix(path, ".json"):
		return "json"
	case strings.HasSuffix(path, ".yaml"), strings.HasSuffix(path, ".yml"):
		return "yaml"
	default:
		return "raw"
	}
}

// applyFormat filters content based on ?format= parameter.
func applyFormat(c *coguri.Content, format string) (*coguri.Content, error) {
	switch format {
	case "hash":
		// Return only the hash, clear content
		c.Data = nil
		c.ContentType = "text"
		return c, nil
	case "frontmatter":
		// Extract YAML frontmatter from markdown content
		if c.Data != nil {
			fm := extractFrontmatter(c.Data)
			c.Data = fm
			c.ContentType = "yaml"
		}
		return c, nil
	case "md", "markdown":
		// Strip frontmatter, return body only
		if c.Data != nil {
			body := extractBody(c.Data)
			c.Data = body
			c.ContentType = "markdown"
		}
		return c, nil
	case "json":
		c.ContentType = "json"
		return c, nil
	default:
		return c, nil
	}
}

// extractFrontmatter returns the YAML frontmatter from a cogdoc.
func extractFrontmatter(data []byte) []byte {
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		return nil
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return nil
	}
	return []byte(s[4 : 4+end])
}

// extractBody returns the markdown body after frontmatter.
func extractBody(data []byte) []byte {
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		return data
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return data
	}
	body := s[4+end+4:] // skip closing ---\n
	return []byte(strings.TrimLeft(body, "\n"))
}

// ── sha256: scheme resolver ──────────────────────────────────────────────────

// sha256SchemeResolver resolves content-addressed URIs.
// Currently searches the workspace for blocks matching the hash.
type sha256SchemeResolver struct {
	root string
}

func (r *sha256SchemeResolver) Scheme() string           { return "sha256" }
func (r *sha256SchemeResolver) CanResolve(_ *coguri.URI) bool { return true }

func (r *sha256SchemeResolver) Resolve(ctx context.Context, uri *coguri.URI) (*coguri.Content, error) {
	// TODO: Implement content-addressed lookup against CogBlock store
	// For now, return the hash as metadata so callers know what to look for
	return &coguri.Content{
		URI:         uri.String(),
		ContentType: "reference",
		Metadata: map[string]any{
			"hash":   uri.Path,
			"scheme": "sha256",
		},
	}, nil
}
