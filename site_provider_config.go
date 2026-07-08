// site_provider_config.go — Config loading and build helpers for SiteProvider.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// artifactSHAFile is the deploy-embedded manifest recording the content hash of
// the artifact a target was built from. FetchLive reads it back to compare live
// against a freshly-built artifact; Deploy writes it before staging.
const artifactSHAFile = ".artifact-sha"

// artifactHash computes a deterministic sha256 over every file in distDir,
// hashing the sorted sequence of (relative-path, file-bytes) so the result is
// independent of filesystem walk order. Directories and symlinks are ignored;
// only regular file contents contribute. Returns lowercase hex.
//
// Each entry is length-prefixed (path length, path, content length, content) so
// that no two distinct trees can collide by concatenation ambiguity.
func artifactHash(distDir string) (string, error) {
	var rels []string
	err := filepath.Walk(distDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(distDir, path)
		if err != nil {
			return err
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("artifactHash: walk %s: %w", distDir, err)
	}
	sort.Strings(rels)

	h := sha256.New()
	var lenBuf [8]byte
	for _, rel := range rels {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(rel)))
		h.Write(lenBuf[:])
		h.Write([]byte(rel))

		data, err := os.ReadFile(filepath.Join(distDir, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("artifactHash: read %s: %w", rel, err)
		}
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(data)))
		h.Write(lenBuf[:])
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// siteLoadCRDs walks <root>/apps/*/site.yaml and returns parsed SiteCRDs.
// Parse errors are hard failures; validation errors are logged as warnings
// (invalid CRDs are still returned so the planner can surface them).
func siteLoadCRDs(root string) ([]SiteCRD, error) {
	pattern := filepath.Join(root, "apps", "*", "site.yaml")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("site provider: glob %q: %w", pattern, err)
	}
	var crds []SiteCRD
	for _, yamlPath := range matches {
		data, err := os.ReadFile(yamlPath)
		if err != nil {
			return nil, fmt.Errorf("site provider: read %s: %w", yamlPath, err)
		}
		var crd SiteCRD
		if err := yaml.Unmarshal(data, &crd); err != nil {
			return nil, fmt.Errorf("site provider: parse %s: %w", yamlPath, err)
		}
		if err := crd.Validate(); err != nil {
			log.Printf("[site] warning: %s validation: %v", yamlPath, err)
		}
		crds = append(crds, crd)
	}
	return crds, nil
}

// siteCRDByName looks up a SiteCRD by metadata name from a pre-loaded slice.
func siteCRDByName(crds []SiteCRD, name string) (SiteCRD, bool) {
	for _, c := range crds {
		if c.Metadata.Name == name {
			return c, true
		}
	}
	return SiteCRD{}, false
}

// siteBuild runs build.sh in the given app directory.
func siteBuild(ctx context.Context, appDir, name string) error {
	cmd := exec.CommandContext(ctx, "bash", "build.sh")
	cmd.Dir = appDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build.sh for %s: %w\n%s", name, err, out)
	}
	log.Printf("[site] build %s: %s", name, out)
	return nil
}

// buildAndHash runs the app's build (the same build.sh path ApplyPlan uses) and
// returns the content hash of the produced dist directory. The build writes to
// appDir/dist in place — build.sh hard-codes that path and relies on the app's
// location for its ../../packages copies, so there is no separate temp build dir
// to redirect to or clean up; the dist tree is the artifact ApplyPlan deploys.
func (s *SiteProvider) buildAndHash(ctx context.Context, appDir, name string) (string, error) {
	if err := siteBuild(ctx, appDir, name); err != nil {
		return "", err
	}
	return artifactHash(filepath.Join(appDir, "dist"))
}
