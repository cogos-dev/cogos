// site_strategies.go — DeployStrategy interface and supporting types for site deployments.
//
// DeployStrategy is the extension point for site deployment backends. Each strategy
// (gh-pages, s3, etc.) implements this interface so that SiteProvider can delegate
// the live-state fetch and artifact deployment steps without knowing strategy internals.
//
// Design rationale: SiteProvider owns the reconcile lifecycle (plan / apply / state);
// DeployStrategy owns the I/O against the external deployment target. The two surfaces
// are deliberately separate so that adding a new strategy does not require modifying
// SiteProvider.

package main

import (
	"context"
	"errors"
)

// ErrUnsupportedStrategy is returned by strategy registries or dispatch functions
// when the requested strategy identifier is not registered.
var ErrUnsupportedStrategy = errors.New("unsupported deploy strategy")

// LiveSiteState captures the observable state of a deployed site as fetched from the
// deployment target. It is used by SiteProvider.FetchLive to compare declared config
// against live state and decide whether a deployment is needed.
type LiveSiteState struct {
	// ArtifactSHA is the content-addressable SHA of the last deployed artifact.
	// For gh-pages this is the HEAD commit SHA of the gh-pages branch.
	ArtifactSHA string `json:"artifact_sha"`

	// CNAMEContent is the content of the CNAME file at the deployment target,
	// if present. Empty string if no CNAME file exists.
	CNAMEContent string `json:"cname_content"`

	// ResolvedFromTarget indicates whether the live state was fetched from the
	// deployment target (true) or inferred from local state only (false).
	ResolvedFromTarget bool `json:"resolved_from_target"`

	// Metadata holds strategy-specific supplemental data (e.g. branch protection
	// status, Pages endpoint URL, S3 bucket region). Keys are strategy-defined.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// DeployStrategy is the contract implemented by each site deployment backend.
//
// Implementations must be safe to call concurrently if SiteProvider is reconciling
// multiple sites in parallel.
type DeployStrategy interface {
	// FetchLive retrieves the current deployed state of the site from the external
	// target (e.g. the gh-pages branch HEAD, S3 bucket metadata). It must not
	// mutate any external state.
	FetchLive(ctx context.Context, app SiteCRD) (LiveSiteState, error)

	// Deploy publishes the artifact directory to the deployment target.
	// artifactDir is the absolute path to the directory produced by the build step
	// (i.e. the resolved path of BuildSpec.Dist). Deploy is responsible for all
	// target-specific upload, branch-push, or sync operations.
	Deploy(ctx context.Context, app SiteCRD, artifactDir string) error
}
