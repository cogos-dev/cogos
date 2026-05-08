// site_crd.go — Site CRD type definitions for the cogos kernel reconciliation framework.
//
// SiteCRD declares a single myrgic.* domain deployment. It is consumed by SiteProvider,
// which implements Reconcilable to manage site lifecycle through the standard
// plan/apply/state reconciliation loop. The CRD mirrors the Kubernetes API-conventions
// shape (apiVersion / kind / metadata / spec) so that site.yaml files are familiar to
// anyone who has written a Kubernetes manifest.
//
// Supported deployment strategies in v0.0.1:
//   - "gh-pages": GitHub Pages via the gh-pages branch; target must include {"repo": "owner/repo"}.
//
// Planned strategies (future):
//   - "gitlab-pages", "s3", "k8s-ingress", "self-hosted-rsync"

package main

import (
	"errors"
	"fmt"
)

// knownStrategies enumerates all accepted deploy strategy identifiers.
var knownStrategies = map[string]bool{
	"gh-pages":          true,
	"gitlab-pages":      true,
	"s3":               true,
	"k8s-ingress":      true,
	"self-hosted-rsync": true,
}

// SiteCRD is the top-level declaration of a myrgic.* domain deployment.
// It is typically read from a per-app site.yaml file and loaded by SiteProvider.
type SiteCRD struct {
	// APIVersion identifies the schema version; currently "cogos.myrgic.io/v1alpha1".
	APIVersion string       `yaml:"apiVersion" json:"apiVersion"`
	// Kind is always "Site".
	Kind       string       `yaml:"kind"       json:"kind"`
	// Metadata carries the site name (matching the apps/<name>/ directory).
	Metadata   SiteMetadata `yaml:"metadata"   json:"metadata"`
	// Spec describes the desired state of the site.
	Spec       SiteSpec     `yaml:"spec"       json:"spec"`
}

// SiteMetadata holds identifying information for the site resource.
type SiteMetadata struct {
	// Name must match the apps/<name>/ directory and be unique within the monorepo.
	Name string `yaml:"name" json:"name"`
}

// SiteSpec describes the desired state of a single site deployment.
type SiteSpec struct {
	// Domain is the fully-qualified domain name (e.g. "myrgic.io").
	Domain string `yaml:"domain" json:"domain"`

	// Canonical marks this site as the primary / authoritative domain.
	// Only one site in the monorepo should have canonical: true.
	// Canonical sites may not set redirect_to.
	Canonical bool `yaml:"canonical" json:"canonical"`

	// Source configures the location of site source within the app directory.
	Source SourceSpec `yaml:"source" json:"source"`

	// Build configures the build step that produces the artifact directory.
	Build BuildSpec `yaml:"build" json:"build"`

	// HTTPS configures TLS requirements for the site.
	HTTPS HTTPSSpec `yaml:"https" json:"https"`

	// Deploy configures the deployment target and strategy.
	Deploy DeploySpec `yaml:"deploy" json:"deploy"`

	// RedirectTo, if set, declares that this domain should redirect to the
	// given URL rather than serve content. Mutually exclusive with canonical: true.
	RedirectTo *string `yaml:"redirect_to,omitempty" json:"redirect_to,omitempty"`
}

// SourceSpec locates the site source files within the app directory.
type SourceSpec struct {
	// Path is relative to the app root (apps/<name>/). Defaults to "src/".
	Path string `yaml:"path" json:"path"`
}

// BuildSpec configures the build step that produces the artifact directory.
type BuildSpec struct {
	// Command is the shell command run from the app root to produce the artifact
	// directory. Defaults to "./build.sh".
	Command string `yaml:"command" json:"command"`

	// Dist is the path (relative to the app root) where the build deposits its
	// output. Defaults to "dist/".
	Dist string `yaml:"dist" json:"dist"`
}

// HTTPSSpec configures TLS enforcement for the site.
type HTTPSSpec struct {
	// Required indicates that HTTPS must be enforced (e.g. via GitHub Pages HTTPS
	// enforcement or an ingress annotation). Defaults to false.
	Required bool `yaml:"required" json:"required"`
}

// DeploySpec configures the deployment target and strategy.
type DeploySpec struct {
	// Strategy identifies the deploy mechanism. In v0.0.1 only "gh-pages" is
	// implemented; future strategies are enumerated in the package doc comment.
	Strategy string `yaml:"strategy" json:"strategy"`

	// Target holds strategy-specific configuration parameters.
	// For "gh-pages": {"repo": "owner/repo"}
	Target map[string]any `yaml:"target" json:"target"`
}

// ErrInvalidSiteCRD is returned by Validate when the CRD contains
// inconsistent or missing fields.
var ErrInvalidSiteCRD = errors.New("invalid SiteCRD")

// Validate checks the CRD for internal consistency and returns an error
// describing the first violation found, or nil if the CRD is valid.
//
// Rules enforced:
//   - metadata.name must not be empty
//   - spec.domain must not be empty
//   - canonical: true and redirect_to are mutually exclusive
//   - spec.deploy.strategy must be a known strategy identifier
//   - spec.deploy.strategy must not be empty
func (s SiteCRD) Validate() error {
	if s.Metadata.Name == "" {
		return fmt.Errorf("%w: metadata.name is required", ErrInvalidSiteCRD)
	}
	if s.Spec.Domain == "" {
		return fmt.Errorf("%w: spec.domain is required", ErrInvalidSiteCRD)
	}
	if s.Spec.Canonical && s.Spec.RedirectTo != nil {
		return fmt.Errorf("%w: canonical site %q must not set redirect_to", ErrInvalidSiteCRD, s.Metadata.Name)
	}
	if s.Spec.Deploy.Strategy == "" {
		return fmt.Errorf("%w: spec.deploy.strategy is required", ErrInvalidSiteCRD)
	}
	if !knownStrategies[s.Spec.Deploy.Strategy] {
		return fmt.Errorf("%w: unknown deploy strategy %q", ErrInvalidSiteCRD, s.Spec.Deploy.Strategy)
	}
	return nil
}
