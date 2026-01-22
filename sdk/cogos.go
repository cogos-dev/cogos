// Package sdk provides the CogOS SDK for cognitive workspace integration.
//
// The SDK provides holographic projection of workspace state through URI resolution.
// URIs are the source of truth. The kernel projects. Widgets consume projections.
//
// # Quick Start
//
//	kernel, err := sdk.Connect(".")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer kernel.Close()
//
//	// Resolve a URI
//	resource, _ := kernel.Resolve("cog://mem/semantic/insights")
//
//	// Project into typed struct
//	var coherence types.CoherenceState
//	kernel.Project("cog://coherence", &coherence)
//
// # URI Scheme
//
// All workspace access is through cog:// URIs:
//
//	cog://mem/*         - Cogdocs (knowledge, sessions, etc.)
//	cog://signals/*     - Signal field (stigmergic coordination)
//	cog://context       - Four-tier context for inference
//	cog://thread/*      - Conversation threads
//	cog://coherence     - Workspace coherence state
//	cog://identity      - Workspace identity
//	cog://src           - SRC constants (immutable)
//	cog://adr/*         - Architecture Decision Records
//	cog://ledger/*      - Event ledger
//
// Query parameters act as projections (filters, shapes):
//
//	cog://mem/semantic?q=topic&limit=10
//	cog://signals/inference?above=0.3
//	cog://context?budget=50000
//
// # SRC Constants
//
// The SDK embeds the Self-Reference Coherence constants:
//
//	src := sdk.Constants()
//	fmt.Printf("τ₂ = %f\n", src.Tau2)  // 1.386... (2*ln(2))
//	fmt.Printf("g_eff = %f\n", src.GEff)  // 0.333... (1/3)
//
// These constants are derived from mathematics, not configuration.
// They are available without a workspace connection.
package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cogos-dev/cogos/sdk/internal/fs"
	"github.com/cogos-dev/cogos/sdk/types"
)

// Version is the SDK version.
const Version = "0.1.0"

// Connect establishes a connection to the workspace kernel.
//
// The workspaceRoot should be the directory containing .cog/.
// Use "." for the current directory.
//
// Connect will:
//  1. Find and validate the workspace root
//  2. Verify the workspace structure (.cog/id.cog exists)
//  3. Create and configure the Kernel
//  4. Register default projectors
//
// Returns ErrWorkspaceNotFound if no .cog directory exists.
//
// Example:
//
//	kernel, err := sdk.Connect(".")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer kernel.Close()
func Connect(workspaceRoot string) (*Kernel, error) {
	// Resolve to absolute path
	absRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, NewPathError("Connect", workspaceRoot, err)
	}

	// Check for .cog directory
	cogDir := filepath.Join(absRoot, ".cog")
	info, err := os.Stat(cogDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NewPathError("Connect", cogDir, ErrWorkspaceNotFound).
				WithRecover("Run 'cog init' to create a workspace")
		}
		return nil, NewPathError("Connect", cogDir, err)
	}
	if !info.IsDir() {
		return nil, NewPathError("Connect", cogDir, fmt.Errorf(".cog is not a directory"))
	}

	// Check for id.cog (workspace identity)
	idPath := filepath.Join(cogDir, "id.cog")
	if _, err := os.Stat(idPath); err != nil {
		if os.IsNotExist(err) {
			return nil, NewPathError("Connect", idPath, ErrWorkspaceNotFound).
				WithRecover("Workspace missing id.cog - may be corrupted")
		}
		return nil, NewPathError("Connect", idPath, err)
	}

	// Create kernel
	kernel := newKernel(absRoot)

	// Register default projectors
	registerBuiltinProjectors(kernel)

	return kernel, nil
}

// FindWorkspaceRoot searches upward from the given path for a .cog directory.
// Returns the workspace root, or error if not found.
func FindWorkspaceRoot(startPath string) (string, error) {
	absPath, err := filepath.Abs(startPath)
	if err != nil {
		return "", err
	}

	current := absPath
	for {
		cogDir := filepath.Join(current, ".cog")
		if info, err := os.Stat(cogDir); err == nil && info.IsDir() {
			return current, nil
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", ErrWorkspaceNotFound
		}
		current = parent
	}
}

// MustConnect is like Connect but panics on error.
func MustConnect(workspaceRoot string) *Kernel {
	kernel, err := Connect(workspaceRoot)
	if err != nil {
		panic(err)
	}
	return kernel
}

// registerBuiltinProjectors adds all built-in projectors.
func registerBuiltinProjectors(k *Kernel) {
	// Core projectors (Phase 1)
	k.RegisterProjector(&srcProjector{BaseProjector: NewBaseProjector("src")})
	k.RegisterProjector(&identityProjector{BaseProjector: NewBaseProjector("identity"), kernel: k})
	k.RegisterProjector(&coherenceProjector{BaseProjector: NewBaseProjector("coherence"), kernel: k})
	k.RegisterProjector(&memoryProjector{BaseProjector: NewBaseProjector("memory"), kernel: k})

	// Phase 2 mutable projectors
	k.RegisterProjector(&signalProjector{BaseProjector: NewBaseProjector("signals"), kernel: k})
	k.RegisterProjector(&threadProjector{BaseProjector: NewBaseProjector("thread"), kernel: k})

	// Extended projectors (from kernel)
	k.RegisterProjector(&adrProjector{BaseProjector: NewBaseProjector("adr"), kernel: k})
	k.RegisterProjector(&specProjector{BaseProjector: NewBaseProjector("spec"), kernel: k})
	k.RegisterProjector(&specProjector{BaseProjector: NewBaseProjector("specs"), kernel: k})
	k.RegisterProjector(&statusProjector{BaseProjector: NewBaseProjector("status"), kernel: k})
	k.RegisterProjector(&canonicalProjector{BaseProjector: NewBaseProjector("canonical"), kernel: k})
	k.RegisterProjector(&handoffProjector{BaseProjector: NewBaseProjector("handoff"), kernel: k})
	k.RegisterProjector(&handoffProjector{BaseProjector: NewBaseProjector("handoffs"), kernel: k})
	k.RegisterProjector(&crystalProjector{BaseProjector: NewBaseProjector("crystal"), kernel: k})
	k.RegisterProjector(&ledgerProjector{BaseProjector: NewBaseProjector("ledger"), kernel: k})
	k.RegisterProjector(&roleProjector{BaseProjector: NewBaseProjector("role"), kernel: k})
	k.RegisterProjector(&roleProjector{BaseProjector: NewBaseProjector("roles"), kernel: k})
	k.RegisterProjector(&skillProjector{BaseProjector: NewBaseProjector("skill"), kernel: k})
	k.RegisterProjector(&skillProjector{BaseProjector: NewBaseProjector("skills"), kernel: k})
	k.RegisterProjector(&agentProjector{BaseProjector: NewBaseProjector("agent"), kernel: k})
	k.RegisterProjector(&agentProjector{BaseProjector: NewBaseProjector("agents"), kernel: k})
	k.RegisterProjector(&kernelProjector{BaseProjector: NewBaseProjector("kernel"), kernel: k})

	// Phase 3 context and inference projectors
	k.RegisterProjector(&contextProjector{BaseProjector: NewBaseProjector("context"), kernel: k})
	k.RegisterProjector(&inferenceProjector{
		BaseProjector: NewBaseProjector("inference"),
		kernel:        k,
		config:        types.DefaultInferenceConfig(),
	})
}

// --- Phase 1 Projector Implementations ---

// srcProjector handles cog://src
type srcProjector struct {
	BaseProjector
}

func (p *srcProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	src := Constants()
	return NewJSONResource(uri.Raw, src)
}

// identityProjector handles cog://identity
type identityProjector struct {
	BaseProjector
	kernel *Kernel
}

func (p *identityProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	idPath := filepath.Join(p.kernel.CogDir(), "id.cog")
	content, err := os.ReadFile(idPath)
	if err != nil {
		return nil, NewPathError("Resolve", idPath, err)
	}

	resource := NewResource(uri.Raw, content)
	resource.ContentType = ContentTypeCogdoc
	return resource, nil
}

// coherenceProjector handles cog://coherence
type coherenceProjector struct {
	BaseProjector
	kernel *Kernel
}

func (p *coherenceProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	hashPath := filepath.Join(p.kernel.StateDir(), "canonical-hash")
	canonical, err := os.ReadFile(hashPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, NewPathError("Resolve", hashPath, err)
	}

	state := map[string]any{
		"coherent":       true,
		"canonical_hash": string(canonical),
		"current_hash":   "",
		"drift":          []string{},
	}

	return NewJSONResource(uri.Raw, state)
}

// memoryProjector handles cog://mem/*
type memoryProjector struct {
	BaseProjector
	kernel *Kernel
}

// CanMutate returns true - memory cogdocs can be created and updated.
func (p *memoryProjector) CanMutate() bool {
	return true
}

func (p *memoryProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	if uri.Path == "" {
		return p.listSectors(uri)
	}

	memPath := filepath.Join(p.kernel.MemoryDir(), uri.Path)

	info, err := os.Stat(memPath)
	if err != nil {
		// Try with .cog.md extension
		cogPath := memPath + ".cog.md"
		if content, err := os.ReadFile(cogPath); err == nil {
			resource := NewResource(uri.Raw, content)
			resource.ContentType = ContentTypeCogdoc
			return resource, nil
		}
		// Try with .md extension
		mdPath := memPath + ".md"
		if content, err := os.ReadFile(mdPath); err == nil {
			resource := NewResource(uri.Raw, content)
			resource.ContentType = ContentTypeMarkdown
			return resource, nil
		}
		return nil, NotFoundError("Resolve", uri.Raw)
	}

	if info.IsDir() {
		return p.listDirectory(uri, memPath)
	}

	content, err := os.ReadFile(memPath)
	if err != nil {
		return nil, NewPathError("Resolve", memPath, err)
	}

	resource := NewResource(uri.Raw, content)
	if filepath.Ext(memPath) == ".md" {
		resource.ContentType = ContentTypeMarkdown
	}
	return resource, nil
}

func (p *memoryProjector) listSectors(uri *ParsedURI) (*Resource, error) {
	sectors := []string{"semantic", "episodic", "procedural", "reflective"}
	children := make([]*Resource, 0, len(sectors))

	for _, sector := range sectors {
		sectorPath := filepath.Join(p.kernel.MemoryDir(), sector)
		if info, err := os.Stat(sectorPath); err == nil && info.IsDir() {
			child := NewResource("cog://mem/"+sector, nil)
			child.SetMetadata("sector", sector)
			children = append(children, child)
		}
	}

	resource := NewResource(uri.Raw, nil)
	resource.Children = children
	return resource, nil
}

func (p *memoryProjector) listDirectory(uri *ParsedURI, dirPath string) (*Resource, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, NewPathError("Resolve", dirPath, err)
	}

	children := make([]*Resource, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		childURI := uri.Raw + "/" + name
		child := NewResource(childURI, nil)
		child.SetMetadata("name", name)
		child.SetMetadata("is_dir", entry.IsDir())
		children = append(children, child)
	}

	resource := NewResource(uri.Raw, nil)
	resource.Children = children
	return resource, nil
}

// Mutate writes or updates cogdocs in memory.
//
// Operations:
//   - Set: Create or replace a cogdoc
//   - Delete: Remove a cogdoc
//
// For Set, content should be a valid cogdoc (Markdown with YAML frontmatter):
//
//	---
//	type: insight
//	id: my-insight
//	title: My Insight
//	created: 2026-01-10
//	---
//	# Content here
//
// Cogdocs are validated before writing if the path ends in .cog.md.
func (p *memoryProjector) Mutate(ctx context.Context, uri *ParsedURI, m *Mutation) error {
	if uri.Path == "" {
		return NewURIError("Mutate", uri.Raw, fmt.Errorf("memory path required (e.g., cog://mem/semantic/insights/new-insight)"))
	}

	switch m.Op {
	case MutationSet:
		return p.setMemory(uri, m.Content, m.Metadata)
	case MutationDelete:
		return p.deleteMemory(uri)
	default:
		return NewURIError("Mutate", uri.Raw, fmt.Errorf("unsupported op: %s (use set or delete)", m.Op))
	}
}

// setMemory creates or replaces a cogdoc.
func (p *memoryProjector) setMemory(uri *ParsedURI, content []byte, metadata map[string]any) error {
	memPath := filepath.Join(p.kernel.MemoryDir(), uri.Path)

	// Determine final path (add extension if needed)
	if filepath.Ext(memPath) == "" {
		// Check if content looks like a cogdoc
		if len(content) > 4 && string(content[:4]) == "---\n" {
			memPath += ".cog.md"
		} else {
			memPath += ".md"
		}
	}

	// Validate cogdocs before writing
	if filepath.Ext(memPath) == ".md" && strings.HasSuffix(memPath, ".cog.md") {
		// Validate cogdoc content
		validate := true
		if metadata != nil {
			if v, ok := metadata["validate"].(bool); ok {
				validate = v
			}
		}
		if validate {
			if err := p.validateCogdocContent(content); err != nil {
				return NewURIError("Mutate", uri.Raw, fmt.Errorf("cogdoc validation failed: %w", err))
			}
		}
	}

	// Ensure directory exists
	dir := filepath.Dir(memPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return NewPathError("Mutate", dir, err)
	}

	// Use internal/fs for atomic write
	return writeAtomicFile(memPath, content, 0644)
}

// validateCogdocContent validates cogdoc content without writing to disk.
func (p *memoryProjector) validateCogdocContent(content []byte) error {
	str := string(content)

	// Check for frontmatter
	if !strings.HasPrefix(str, "---\n") {
		return fmt.Errorf("no frontmatter (must start with ---)")
	}

	end := strings.Index(str[4:], "\n---")
	if end == -1 {
		return fmt.Errorf("unclosed frontmatter")
	}

	fmContent := str[4 : 4+end]
	result := ValidateFrontmatter(fmContent)
	if !result.Valid {
		return fmt.Errorf("%s", strings.Join(result.Errors, "; "))
	}

	return nil
}

// deleteMemory removes a cogdoc.
func (p *memoryProjector) deleteMemory(uri *ParsedURI) error {
	memPath := filepath.Join(p.kernel.MemoryDir(), uri.Path)

	// Try exact path first
	if _, err := os.Stat(memPath); err == nil {
		return os.Remove(memPath)
	}

	// Try with extensions
	for _, ext := range []string{".cog.md", ".md"} {
		extPath := memPath + ext
		if _, err := os.Stat(extPath); err == nil {
			return os.Remove(extPath)
		}
	}

	return NotFoundError("Mutate", uri.Raw)
}

// writeAtomicFile writes data to a file atomically.
func writeAtomicFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	tmpPath := path + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	if err := os.WriteFile(tmpPath, data, perm); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}

	return nil
}

// --- Extended Projector Implementations (ported from kernel) ---

// adrProjector handles cog://adr/*
type adrProjector struct {
	BaseProjector
	kernel *Kernel
}

func (p *adrProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	adrDir := filepath.Join(p.kernel.CogDir(), "adr")

	if uri.Path == "" {
		// List all ADRs
		return p.listADRs(uri, adrDir)
	}

	// Resolve specific ADR using glob pattern (e.g., "004" -> "004-*.md")
	pattern := filepath.Join(adrDir, uri.Path+"-*.md")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		// Try exact match
		exactPath := filepath.Join(adrDir, uri.Path+".md")
		if content, err := os.ReadFile(exactPath); err == nil {
			resource := NewResource(uri.Raw, content)
			resource.ContentType = ContentTypeMarkdown
			return resource, nil
		}
		return nil, NotFoundError("Resolve", uri.Raw)
	}

	content, err := os.ReadFile(matches[0])
	if err != nil {
		return nil, NewPathError("Resolve", matches[0], err)
	}

	resource := NewResource(uri.Raw, content)
	resource.ContentType = ContentTypeMarkdown
	return resource, nil
}

func (p *adrProjector) listADRs(uri *ParsedURI, adrDir string) (*Resource, error) {
	entries, err := os.ReadDir(adrDir)
	if err != nil {
		if os.IsNotExist(err) {
			return NewResource(uri.Raw, nil), nil
		}
		return nil, NewPathError("Resolve", adrDir, err)
	}

	children := make([]*Resource, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		name := entry.Name()
		childURI := "cog://adr/" + name[:len(name)-3] // Remove .md
		child := NewResource(childURI, nil)
		child.SetMetadata("name", name)
		children = append(children, child)
	}

	resource := NewResource(uri.Raw, nil)
	resource.Children = children
	return resource, nil
}

// specProjector handles cog://spec/* and cog://specs/*
type specProjector struct {
	BaseProjector
	kernel *Kernel
}

func (p *specProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	specDir := filepath.Join(p.kernel.CogDir(), "specs")

	if uri.Path == "" {
		return p.listDirectory(uri, specDir)
	}

	specPath := filepath.Join(specDir, uri.Path+".cog.md")
	content, err := os.ReadFile(specPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NotFoundError("Resolve", uri.Raw)
		}
		return nil, NewPathError("Resolve", specPath, err)
	}

	resource := NewResource(uri.Raw, content)
	resource.ContentType = ContentTypeCogdoc
	return resource, nil
}

func (p *specProjector) listDirectory(uri *ParsedURI, dirPath string) (*Resource, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewResource(uri.Raw, nil), nil
		}
		return nil, NewPathError("Resolve", dirPath, err)
	}

	children := make([]*Resource, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		childURI := uri.Raw + "/" + name
		child := NewResource(childURI, nil)
		child.SetMetadata("name", name)
		child.SetMetadata("is_dir", entry.IsDir())
		children = append(children, child)
	}

	resource := NewResource(uri.Raw, nil)
	resource.Children = children
	return resource, nil
}

// statusProjector handles cog://status/*
type statusProjector struct {
	BaseProjector
	kernel *Kernel
}

func (p *statusProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	statusDir := filepath.Join(p.kernel.CogDir(), "status")

	if uri.Path == "" {
		return p.listDirectory(uri, statusDir)
	}

	statusPath := filepath.Join(statusDir, uri.Path+".json")
	content, err := os.ReadFile(statusPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NotFoundError("Resolve", uri.Raw)
		}
		return nil, NewPathError("Resolve", statusPath, err)
	}

	resource := NewResource(uri.Raw, content)
	resource.ContentType = ContentTypeJSON
	return resource, nil
}

func (p *statusProjector) listDirectory(uri *ParsedURI, dirPath string) (*Resource, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewResource(uri.Raw, nil), nil
		}
		return nil, NewPathError("Resolve", dirPath, err)
	}

	children := make([]*Resource, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		childURI := uri.Raw + "/" + name
		child := NewResource(childURI, nil)
		child.SetMetadata("name", name)
		children = append(children, child)
	}

	resource := NewResource(uri.Raw, nil)
	resource.Children = children
	return resource, nil
}

// canonicalProjector handles cog://canonical
type canonicalProjector struct {
	BaseProjector
	kernel *Kernel
}

func (p *canonicalProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	hashPath := filepath.Join(p.kernel.StateDir(), "canonical-hash")
	content, err := os.ReadFile(hashPath)
	if err != nil {
		if os.IsNotExist(err) {
			state := map[string]any{
				"exists": false,
				"hash":   "",
			}
			return NewJSONResource(uri.Raw, state)
		}
		return nil, NewPathError("Resolve", hashPath, err)
	}

	state := map[string]any{
		"exists": true,
		"hash":   string(content),
	}
	return NewJSONResource(uri.Raw, state)
}

// handoffProjector handles cog://handoff/* and cog://handoffs/*
type handoffProjector struct {
	BaseProjector
	kernel *Kernel
}

func (p *handoffProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	handoffDir := filepath.Join(p.kernel.Root(), "projects", "cog_lab_package", "handoffs")

	if uri.Path == "" {
		return p.listDirectory(uri, handoffDir)
	}

	// Try glob pattern first
	pattern := filepath.Join(handoffDir, uri.Path+"*.md")
	matches, err := filepath.Glob(pattern)
	if err == nil && len(matches) > 0 {
		content, err := os.ReadFile(matches[0])
		if err != nil {
			return nil, NewPathError("Resolve", matches[0], err)
		}
		resource := NewResource(uri.Raw, content)
		resource.ContentType = ContentTypeMarkdown
		return resource, nil
	}

	// Try exact path
	exactPath := filepath.Join(handoffDir, uri.Path+".md")
	content, err := os.ReadFile(exactPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NotFoundError("Resolve", uri.Raw)
		}
		return nil, NewPathError("Resolve", exactPath, err)
	}

	resource := NewResource(uri.Raw, content)
	resource.ContentType = ContentTypeMarkdown
	return resource, nil
}

func (p *handoffProjector) listDirectory(uri *ParsedURI, dirPath string) (*Resource, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewResource(uri.Raw, nil), nil
		}
		return nil, NewPathError("Resolve", dirPath, err)
	}

	children := make([]*Resource, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		childURI := uri.Raw + "/" + name
		child := NewResource(childURI, nil)
		child.SetMetadata("name", name)
		children = append(children, child)
	}

	resource := NewResource(uri.Raw, nil)
	resource.Children = children
	return resource, nil
}

// crystalProjector handles cog://crystal/*
type crystalProjector struct {
	BaseProjector
	kernel *Kernel
}

func (p *crystalProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	if uri.Path == "" {
		return nil, InvalidURIError(uri.Raw, "crystal namespace requires a session ID path")
	}

	crystalPath := filepath.Join(p.kernel.CogDir(), "ledger", uri.Path, "crystal.json")
	content, err := os.ReadFile(crystalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NotFoundError("Resolve", uri.Raw)
		}
		return nil, NewPathError("Resolve", crystalPath, err)
	}

	resource := NewResource(uri.Raw, content)
	resource.ContentType = ContentTypeJSON
	return resource, nil
}

// ledgerProjector handles cog://ledger/*
type ledgerProjector struct {
	BaseProjector
	kernel *Kernel
}

// CanMutate returns true - events can be appended to the ledger.
func (p *ledgerProjector) CanMutate() bool {
	return true
}

func (p *ledgerProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	ledgerDir := filepath.Join(p.kernel.CogDir(), "ledger")

	if uri.Path == "" {
		return p.listDirectory(uri, ledgerDir)
	}

	sessionDir := filepath.Join(ledgerDir, uri.Path)
	info, err := os.Stat(sessionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NotFoundError("Resolve", uri.Raw)
		}
		return nil, NewPathError("Resolve", sessionDir, err)
	}

	if info.IsDir() {
		return p.listDirectory(uri, sessionDir)
	}

	content, err := os.ReadFile(sessionDir)
	if err != nil {
		return nil, NewPathError("Resolve", sessionDir, err)
	}

	resource := NewResource(uri.Raw, content)
	return resource, nil
}

func (p *ledgerProjector) listDirectory(uri *ParsedURI, dirPath string) (*Resource, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewResource(uri.Raw, nil), nil
		}
		return nil, NewPathError("Resolve", dirPath, err)
	}

	children := make([]*Resource, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		childURI := uri.Raw + "/" + name
		child := NewResource(childURI, nil)
		child.SetMetadata("name", name)
		child.SetMetadata("is_dir", entry.IsDir())
		children = append(children, child)
	}

	resource := NewResource(uri.Raw, nil)
	resource.Children = children
	return resource, nil
}

// Mutate appends events to the ledger.
//
// Operations:
//   - Append: Append an event to the session's event log
//
// The URI path should be the session ID: cog://ledger/{session-id}
//
// For Append, content should be an Event:
//
//	{"type": "message", "source": "cog-chat", "data": {"role": "user", "content": "Hello"}}
//
// The event will be assigned a sequence number and timestamp automatically.
func (p *ledgerProjector) Mutate(ctx context.Context, uri *ParsedURI, m *Mutation) error {
	if uri.Path == "" {
		return NewURIError("Mutate", uri.Raw, fmt.Errorf("session ID required (e.g., cog://ledger/{session-id})"))
	}

	// Extract session ID from path (e.g., "abc123" or "abc123/events.jsonl")
	pathParts := strings.Split(uri.Path, "/")
	sessionID := pathParts[0]

	switch m.Op {
	case MutationAppend:
		return p.appendEvent(sessionID, m.Content)
	default:
		return NewURIError("Mutate", uri.Raw, fmt.Errorf("unsupported op: %s (ledger is append-only)", m.Op))
	}
}

// appendEvent appends an event to the session's event log.
func (p *ledgerProjector) appendEvent(sessionID string, content []byte) error {
	// Parse the event data
	var event types.Event
	if err := json.Unmarshal(content, &event); err != nil {
		return NewError("Mutate", fmt.Errorf("invalid event data: %w", err))
	}

	// Assign sequence number
	event.Seq = p.kernel.NextEventSeq()

	// Set timestamp if not provided
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	// Set session ID
	event.SessionID = sessionID

	// Serialize to JSONL
	line, err := event.ToJSONLine()
	if err != nil {
		return NewError("Mutate", fmt.Errorf("failed to serialize event: %w", err))
	}

	// Append to events file
	eventsPath := filepath.Join(p.kernel.CogDir(), "ledger", sessionID, "events.jsonl")
	return fs.AppendLine(eventsPath, line)
}

// roleProjector handles cog://role/* and cog://roles/*
type roleProjector struct {
	BaseProjector
	kernel *Kernel
}

func (p *roleProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	roleDir := filepath.Join(p.kernel.CogDir(), "roles")

	if uri.Path == "" {
		return p.listDirectory(uri, roleDir)
	}

	rolePath := filepath.Join(roleDir, uri.Path)
	info, err := os.Stat(rolePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NotFoundError("Resolve", uri.Raw)
		}
		return nil, NewPathError("Resolve", rolePath, err)
	}

	if info.IsDir() {
		return p.listDirectory(uri, rolePath)
	}

	content, err := os.ReadFile(rolePath)
	if err != nil {
		return nil, NewPathError("Resolve", rolePath, err)
	}

	resource := NewResource(uri.Raw, content)
	return resource, nil
}

func (p *roleProjector) listDirectory(uri *ParsedURI, dirPath string) (*Resource, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewResource(uri.Raw, nil), nil
		}
		return nil, NewPathError("Resolve", dirPath, err)
	}

	children := make([]*Resource, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		childURI := uri.Raw + "/" + name
		child := NewResource(childURI, nil)
		child.SetMetadata("name", name)
		child.SetMetadata("is_dir", entry.IsDir())
		children = append(children, child)
	}

	resource := NewResource(uri.Raw, nil)
	resource.Children = children
	return resource, nil
}

// skillProjector handles cog://skill/* and cog://skills/*
type skillProjector struct {
	BaseProjector
	kernel *Kernel
}

func (p *skillProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	skillDir := filepath.Join(p.kernel.Root(), ".claude", "skills")

	if uri.Path == "" {
		return p.listDirectory(uri, skillDir)
	}

	skillPath := filepath.Join(skillDir, uri.Path)
	info, err := os.Stat(skillPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NotFoundError("Resolve", uri.Raw)
		}
		return nil, NewPathError("Resolve", skillPath, err)
	}

	if info.IsDir() {
		return p.listDirectory(uri, skillPath)
	}

	content, err := os.ReadFile(skillPath)
	if err != nil {
		return nil, NewPathError("Resolve", skillPath, err)
	}

	resource := NewResource(uri.Raw, content)
	return resource, nil
}

func (p *skillProjector) listDirectory(uri *ParsedURI, dirPath string) (*Resource, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewResource(uri.Raw, nil), nil
		}
		return nil, NewPathError("Resolve", dirPath, err)
	}

	children := make([]*Resource, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		childURI := uri.Raw + "/" + name
		child := NewResource(childURI, nil)
		child.SetMetadata("name", name)
		child.SetMetadata("is_dir", entry.IsDir())
		children = append(children, child)
	}

	resource := NewResource(uri.Raw, nil)
	resource.Children = children
	return resource, nil
}

// agentProjector handles cog://agent/* and cog://agents/*
type agentProjector struct {
	BaseProjector
	kernel *Kernel
}

func (p *agentProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	agentDir := filepath.Join(p.kernel.Root(), ".claude", "agents")

	if uri.Path == "" {
		return p.listDirectory(uri, agentDir)
	}

	agentPath := filepath.Join(agentDir, uri.Path)
	info, err := os.Stat(agentPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NotFoundError("Resolve", uri.Raw)
		}
		return nil, NewPathError("Resolve", agentPath, err)
	}

	if info.IsDir() {
		return p.listDirectory(uri, agentPath)
	}

	content, err := os.ReadFile(agentPath)
	if err != nil {
		return nil, NewPathError("Resolve", agentPath, err)
	}

	resource := NewResource(uri.Raw, content)
	return resource, nil
}

func (p *agentProjector) listDirectory(uri *ParsedURI, dirPath string) (*Resource, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewResource(uri.Raw, nil), nil
		}
		return nil, NewPathError("Resolve", dirPath, err)
	}

	children := make([]*Resource, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		childURI := uri.Raw + "/" + name
		child := NewResource(childURI, nil)
		child.SetMetadata("name", name)
		child.SetMetadata("is_dir", entry.IsDir())
		children = append(children, child)
	}

	resource := NewResource(uri.Raw, nil)
	resource.Children = children
	return resource, nil
}

// kernelProjector handles cog://kernel/*
type kernelProjector struct {
	BaseProjector
	kernel *Kernel
}

func (p *kernelProjector) Resolve(_ context.Context, uri *ParsedURI) (*Resource, error) {
	if uri.Path == "" {
		// Return kernel info
		info := map[string]any{
			"version":  Version,
			"cog_dir":  p.kernel.CogDir(),
			"root":     p.kernel.Root(),
			"closed":   p.kernel.IsClosed(),
		}
		return NewJSONResource(uri.Raw, info)
	}

	kernelPath := filepath.Join(p.kernel.CogDir(), uri.Path)
	content, err := os.ReadFile(kernelPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NotFoundError("Resolve", uri.Raw)
		}
		return nil, NewPathError("Resolve", kernelPath, err)
	}

	resource := NewResource(uri.Raw, content)
	return resource, nil
}
