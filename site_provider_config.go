// site_provider_config.go — Config loading and build helpers for SiteProvider.

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

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
