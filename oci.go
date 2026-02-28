// oci.go — Local OCI layout store for kernel binary artifacts.
//
// Wraps oras-go v2 to provide content-addressed storage of kernel binaries
// in a standard OCI image layout at .cog/oci/. This enables the auto-reload
// loop: make push → kernel detects new digest → pull → re-exec.
//
// The layout follows the OCI Image Layout Specification:
//
//	.cog/oci/
//	├── oci-layout       # {"imageLayoutVersion": "1.0.0"}
//	├── index.json       # manifest references (watched by kernel)
//	└── blobs/sha256/    # content-addressed blobs
//
// Each push creates a manifest with a single binary layer, tagged "latest".
// The kernel's fsnotify watcher detects index.json changes and triggers
// digest comparison → pull → graceful re-exec.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
)

const (
	ociLayoutSubdir    = ".cog/oci"
	ociMediaTypeBinary = "application/vnd.cogos.kernel.v1+binary"
	ociArtifactType    = "application/vnd.cogos.kernel.v1"
	ociTagLatest       = "latest"
)

// OCIStore manages a local OCI layout at .cog/oci/.
type OCIStore struct {
	root      string // workspace root
	layoutDir string // absolute path to .cog/oci/
}

// OCIInfo holds metadata about the stored OCI artifact.
type OCIInfo struct {
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
	PushedAt string `json:"pushed_at,omitempty"`
}

// NewOCIStore creates a store for the given workspace root.
func NewOCIStore(workspaceRoot string) *OCIStore {
	return &OCIStore{
		root:      workspaceRoot,
		layoutDir: filepath.Join(workspaceRoot, ociLayoutSubdir),
	}
}

// EnsureLayout creates .cog/oci/ if it doesn't exist.
// The oras oci.New() call handles oci-layout file creation.
func (s *OCIStore) EnsureLayout() error {
	return os.MkdirAll(s.layoutDir, 0755)
}

// IndexPath returns the absolute path to .cog/oci/index.json.
func (s *OCIStore) IndexPath() string {
	return filepath.Join(s.layoutDir, "index.json")
}

// Push reads a binary file, stores it as a blob in the OCI layout,
// creates a manifest referencing it, and tags it as "latest".
// Returns the manifest digest.
func (s *OCIStore) Push(ctx context.Context, binaryPath string) (string, error) {
	if err := s.EnsureLayout(); err != nil {
		return "", fmt.Errorf("ensure layout: %w", err)
	}

	store, err := oci.New(s.layoutDir)
	if err != nil {
		return "", fmt.Errorf("open layout: %w", err)
	}

	// Read binary
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		return "", fmt.Errorf("read binary: %w", err)
	}

	// Push binary blob
	blobDesc := ocispec.Descriptor{
		MediaType: ociMediaTypeBinary,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
		Annotations: map[string]string{
			ocispec.AnnotationTitle: "cog",
		},
	}

	// Push binary blob (skip if already exists — same binary, no-op)
	exists, _ := store.Exists(ctx, blobDesc)
	if !exists {
		if err := store.Push(ctx, blobDesc, newBytesReader(data)); err != nil {
			return "", fmt.Errorf("push blob: %w", err)
		}
	}

	// Create empty config (required by OCI manifest spec)
	configData := []byte("{}")
	configDesc := ocispec.Descriptor{
		MediaType: ociArtifactType,
		Digest:    digest.FromBytes(configData),
		Size:      int64(len(configData)),
	}

	exists, _ = store.Exists(ctx, configDesc)
	if !exists {
		if err := store.Push(ctx, configDesc, newBytesReader(configData)); err != nil {
			return "", fmt.Errorf("push config: %w", err)
		}
	}

	// Build manifest
	now := time.Now().UTC()
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{blobDesc},
		Annotations: map[string]string{
			ocispec.AnnotationCreated: now.Format(time.RFC3339),
			"org.cogos.os":           runtime.GOOS,
			"org.cogos.arch":         runtime.GOARCH,
			"org.cogos.version":      Version,
		},
	}
	manifest.SchemaVersion = 2

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal manifest: %w", err)
	}

	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}

	// Manifest always has a new timestamp, so it's always a new blob
	exists, _ = store.Exists(ctx, manifestDesc)
	if !exists {
		if err := store.Push(ctx, manifestDesc, newBytesReader(manifestJSON)); err != nil {
			return "", fmt.Errorf("push manifest: %w", err)
		}
	}

	// Tag as "latest"
	if err := store.Tag(ctx, manifestDesc, ociTagLatest); err != nil {
		return "", fmt.Errorf("tag: %w", err)
	}

	// Persist index.json — this is what triggers the watcher
	if err := store.SaveIndex(); err != nil {
		return "", fmt.Errorf("save index: %w", err)
	}

	log.Printf("[oci] pushed %s (%d bytes, digest=%s)", binaryPath, len(data), manifestDesc.Digest)
	return string(manifestDesc.Digest), nil
}

// Resolve returns the manifest digest of the "latest" tag, or "" if not found.
func (s *OCIStore) Resolve(ctx context.Context) (string, error) {
	if _, err := os.Stat(s.IndexPath()); os.IsNotExist(err) {
		return "", nil
	}

	store, err := oci.New(s.layoutDir)
	if err != nil {
		return "", fmt.Errorf("open layout: %w", err)
	}

	desc, err := store.Resolve(ctx, ociTagLatest)
	if err != nil {
		return "", nil // Tag not found is not an error
	}

	return string(desc.Digest), nil
}

// ResolveLayerDigest returns the binary layer digest of the "latest" artifact.
// This is stable across pushes of the same binary (unlike manifest digest which
// changes due to timestamp annotations). Used for auto-reload comparison.
func (s *OCIStore) ResolveLayerDigest(ctx context.Context) (string, error) {
	if _, err := os.Stat(s.IndexPath()); os.IsNotExist(err) {
		return "", nil
	}

	store, err := oci.New(s.layoutDir)
	if err != nil {
		return "", fmt.Errorf("open layout: %w", err)
	}

	desc, err := store.Resolve(ctx, ociTagLatest)
	if err != nil {
		return "", nil
	}

	// Fetch manifest to get layer digest
	rc, err := store.Fetch(ctx, desc)
	if err != nil {
		return "", fmt.Errorf("fetch manifest: %w", err)
	}
	manifestJSON, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return "", fmt.Errorf("unmarshal manifest: %w", err)
	}

	if len(manifest.Layers) == 0 {
		return "", nil
	}

	return string(manifest.Layers[0].Digest), nil
}

// Pull extracts the kernel binary from the "latest" manifest to destPath.
// Uses atomic write: writes to destPath.tmp, chmod +x, then renames.
// Returns the layer digest.
func (s *OCIStore) Pull(ctx context.Context, destPath string) (string, error) {
	store, err := oci.New(s.layoutDir)
	if err != nil {
		return "", fmt.Errorf("open layout: %w", err)
	}

	// Resolve latest → manifest
	desc, err := store.Resolve(ctx, ociTagLatest)
	if err != nil {
		return "", fmt.Errorf("resolve latest: %w", err)
	}

	// Fetch manifest
	rc, err := store.Fetch(ctx, desc)
	if err != nil {
		return "", fmt.Errorf("fetch manifest: %w", err)
	}
	manifestJSON, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return "", fmt.Errorf("unmarshal manifest: %w", err)
	}

	if len(manifest.Layers) == 0 {
		return "", fmt.Errorf("manifest has no layers")
	}

	// Fetch binary blob (first layer)
	blobDesc := manifest.Layers[0]
	blobRC, err := store.Fetch(ctx, blobDesc)
	if err != nil {
		return "", fmt.Errorf("fetch blob: %w", err)
	}
	defer blobRC.Close()

	// Atomic write: temp file → chmod → rename
	tmpPath := destPath + ".oci-tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}

	if _, err := io.Copy(f, blobRC); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write temp: %w", err)
	}
	f.Close()

	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("chmod: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("rename: %w", err)
	}

	return string(blobDesc.Digest), nil
}

// Info returns metadata about the current "latest" artifact.
func (s *OCIStore) Info(ctx context.Context) (*OCIInfo, error) {
	if _, err := os.Stat(s.IndexPath()); os.IsNotExist(err) {
		return nil, fmt.Errorf("no OCI layout at %s", s.layoutDir)
	}

	store, err := oci.New(s.layoutDir)
	if err != nil {
		return nil, fmt.Errorf("open layout: %w", err)
	}

	desc, err := store.Resolve(ctx, ociTagLatest)
	if err != nil {
		return nil, fmt.Errorf("resolve latest: %w", err)
	}

	// Fetch manifest for annotations
	rc, err := store.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	manifestJSON, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return nil, fmt.Errorf("unmarshal manifest: %w", err)
	}

	info := &OCIInfo{
		Digest: string(desc.Digest),
	}

	if len(manifest.Layers) > 0 {
		info.Size = manifest.Layers[0].Size
	}

	if manifest.Annotations != nil {
		info.OS = manifest.Annotations["org.cogos.os"]
		info.Arch = manifest.Annotations["org.cogos.arch"]
		info.Version = manifest.Annotations["org.cogos.version"]
		info.PushedAt = manifest.Annotations[ocispec.AnnotationCreated]
	}

	return info, nil
}

// SelfDigest computes the SHA256 digest of the currently running binary.
func SelfDigest() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}

	f, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// bytesReader wraps a byte slice as an io.Reader.
type bytesReader struct {
	*io.SectionReader
}

func newBytesReader(data []byte) io.Reader {
	return io.NewSectionReader(newReaderAt(data), 0, int64(len(data)))
}

type readerAt struct {
	data []byte
}

func newReaderAt(data []byte) io.ReaderAt {
	return &readerAt{data: data}
}

func (r *readerAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Ensure oras.Copy is importable (used transitively)
var _ = oras.PackManifestVersion1_1
