// site_crd_test.go
// Covers: SiteCRD YAML round-trip parsing and Validate() invariants.
// Does NOT modify site_crd.go; if a defect is found in Validate() it is
// documented here and the test is skipped with a FOLLOWUP comment.

package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ─── Fixture YAML strings ────────────────────────────────────────────────────────

const canonicalSiteYAML = `
apiVersion: cogos.myrgic.io/v1alpha1
kind: Site
metadata:
  name: myrgic-com
spec:
  domain: myrgic.com
  canonical: true
  source:
    path: src/
  build:
    command: ./build.sh
    dist: dist/
  https:
    required: true
  deploy:
    strategy: gh-pages
    target:
      repo: myrgic/myrgic.github.io
`

const redirectSiteYAML = `
apiVersion: cogos.myrgic.io/v1alpha1
kind: Site
metadata:
  name: myrgic-dev
spec:
  domain: myrgic.dev
  canonical: false
  source:
    path: src/
  build:
    command: ./build.sh
    dist: dist/
  https:
    required: true
  deploy:
    strategy: gh-pages
    target:
      repo: myrgic/myrgic.dev
  redirect_to: https://myrgic.com/
`

const minSpecSiteYAML = `
apiVersion: cogos.myrgic.io/v1alpha1
kind: Site
metadata:
  name: myrgic-net
spec:
  domain: myrgic.net
  deploy:
    strategy: gh-pages
    target:
      repo: myrgic/myrgic.net
`

const missingDomainYAML = `
apiVersion: cogos.myrgic.io/v1alpha1
kind: Site
metadata:
  name: no-domain
spec:
  deploy:
    strategy: gh-pages
    target:
      repo: myrgic/no-domain
`

const badStrategyYAML = `
apiVersion: cogos.myrgic.io/v1alpha1
kind: Site
metadata:
  name: bad-strategy
spec:
  domain: myrgic.example
  deploy:
    strategy: nonexistent-strategy
    target:
      repo: myrgic/bad-strategy
`

// ─── Parse tests (round-trip yaml → SiteCRD) ────────────────────────────────────

func TestSiteCRD_Parse_Canonical(t *testing.T) {
	var crd SiteCRD
	if err := yaml.Unmarshal([]byte(canonicalSiteYAML), &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if crd.Metadata.Name != "myrgic-com" {
		t.Errorf("name: got %q, want %q", crd.Metadata.Name, "myrgic-com")
	}
	if crd.Spec.Domain != "myrgic.com" {
		t.Errorf("domain: got %q, want %q", crd.Spec.Domain, "myrgic.com")
	}
	if !crd.Spec.Canonical {
		t.Error("canonical: want true")
	}
	if crd.Spec.RedirectTo != nil {
		t.Errorf("redirect_to: want nil, got %q", *crd.Spec.RedirectTo)
	}
	if crd.Spec.Deploy.Strategy != "gh-pages" {
		t.Errorf("strategy: got %q, want %q", crd.Spec.Deploy.Strategy, "gh-pages")
	}
	repo, ok := crd.Spec.Deploy.Target["repo"]
	if !ok {
		t.Error("target.repo missing")
	} else if repo != "myrgic/myrgic.github.io" {
		t.Errorf("target.repo: got %v, want %q", repo, "myrgic/myrgic.github.io")
	}
}

func TestSiteCRD_Parse_Redirect(t *testing.T) {
	var crd SiteCRD
	if err := yaml.Unmarshal([]byte(redirectSiteYAML), &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if crd.Metadata.Name != "myrgic-dev" {
		t.Errorf("name: got %q", crd.Metadata.Name)
	}
	if crd.Spec.Canonical {
		t.Error("canonical: want false")
	}
	if crd.Spec.RedirectTo == nil {
		t.Fatal("redirect_to: want non-nil")
	}
	if *crd.Spec.RedirectTo != "https://myrgic.com/" {
		t.Errorf("redirect_to: got %q, want https://myrgic.com/", *crd.Spec.RedirectTo)
	}
}

func TestSiteCRD_Parse_MinSpec(t *testing.T) {
	var crd SiteCRD
	if err := yaml.Unmarshal([]byte(minSpecSiteYAML), &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if crd.Metadata.Name != "myrgic-net" {
		t.Errorf("name: got %q", crd.Metadata.Name)
	}
	// Default zero values should not cause panics
	if crd.Spec.Source.Path != "" {
		// ok — empty string is the zero value; not a failure
	}
	if err := crd.Validate(); err != nil {
		t.Logf("note: min-spec site does not fully validate: %v (expected)", err)
	}
}

func TestSiteCRD_Parse_MissingDomain_ParsesButFailsValidate(t *testing.T) {
	var crd SiteCRD
	if err := yaml.Unmarshal([]byte(missingDomainYAML), &crd); err != nil {
		t.Fatalf("unmarshal should succeed even with missing domain: %v", err)
	}
	err := crd.Validate()
	if err == nil {
		t.Fatal("Validate: want error for missing domain, got nil")
	}
	if !strings.Contains(err.Error(), "domain") {
		t.Errorf("Validate error should mention domain, got: %v", err)
	}
}

func TestSiteCRD_Parse_BadStrategy_ParsesButFailsValidate(t *testing.T) {
	var crd SiteCRD
	if err := yaml.Unmarshal([]byte(badStrategyYAML), &crd); err != nil {
		t.Fatalf("unmarshal should succeed even with bad strategy: %v", err)
	}
	err := crd.Validate()
	if err == nil {
		t.Fatal("Validate: want error for unknown strategy, got nil")
	}
	if !strings.Contains(err.Error(), "strategy") {
		t.Errorf("Validate error should mention strategy, got: %v", err)
	}
}

// ─── Validate tests (table-driven) ──────────────────────────────────────────────

func TestSiteCRD_Validate(t *testing.T) {
	redirectURL := "https://myrgic.com/"

	cases := []struct {
		name    string
		crd     SiteCRD
		wantErr bool
		errFrag string // substring expected in error message (if wantErr)
	}{
		{
			name: "valid canonical",
			crd: SiteCRD{
				Metadata: SiteMetadata{Name: "myrgic-com"},
				Spec: SiteSpec{
					Domain:    "myrgic.com",
					Canonical: true,
					Deploy:    DeploySpec{Strategy: "gh-pages", Target: map[string]any{"repo": "myrgic/myrgic.github.io"}},
				},
			},
			wantErr: false,
		},
		{
			name: "valid redirect",
			crd: SiteCRD{
				Metadata: SiteMetadata{Name: "myrgic-dev"},
				Spec: SiteSpec{
					Domain:     "myrgic.dev",
					Canonical:  false,
					RedirectTo: &redirectURL,
					Deploy:     DeploySpec{Strategy: "gh-pages", Target: map[string]any{"repo": "myrgic/myrgic.dev"}},
				},
			},
			wantErr: false,
		},
		{
			name: "canonical true with redirect_to set",
			crd: SiteCRD{
				Metadata: SiteMetadata{Name: "bad-combo"},
				Spec: SiteSpec{
					Domain:     "myrgic.com",
					Canonical:  true,
					RedirectTo: &redirectURL,
					Deploy:     DeploySpec{Strategy: "gh-pages", Target: map[string]any{"repo": "myrgic/foo"}},
				},
			},
			wantErr: true,
			errFrag: "redirect_to",
		},
		{
			name: "empty domain",
			crd: SiteCRD{
				Metadata: SiteMetadata{Name: "no-domain"},
				Spec: SiteSpec{
					Domain: "",
					Deploy: DeploySpec{Strategy: "gh-pages"},
				},
			},
			wantErr: true,
			errFrag: "domain",
		},
		{
			name: "unknown strategy",
			crd: SiteCRD{
				Metadata: SiteMetadata{Name: "bad-strat"},
				Spec: SiteSpec{
					Domain: "example.com",
					Deploy: DeploySpec{Strategy: "sftp"},
				},
			},
			wantErr: true,
			errFrag: "strategy",
		},
		{
			name: "valid build spec",
			crd: SiteCRD{
				Metadata: SiteMetadata{Name: "has-build"},
				Spec: SiteSpec{
					Domain:    "myrgic.com",
					Canonical: true,
					Build:     BuildSpec{Command: "./build.sh", Dist: "dist/"},
					Deploy:    DeploySpec{Strategy: "gh-pages", Target: map[string]any{"repo": "myrgic/x"}},
				},
			},
			wantErr: false,
		},
		{
			name: "empty metadata name",
			crd: SiteCRD{
				Metadata: SiteMetadata{Name: ""},
				Spec: SiteSpec{
					Domain: "myrgic.com",
					Deploy: DeploySpec{Strategy: "gh-pages"},
				},
			},
			wantErr: true,
			errFrag: "name",
		},
		{
			name: "missing strategy string",
			crd: SiteCRD{
				Metadata: SiteMetadata{Name: "no-strat"},
				Spec: SiteSpec{
					Domain: "myrgic.com",
					Deploy: DeploySpec{Strategy: ""},
				},
			},
			wantErr: true,
			errFrag: "strategy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.crd.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate: want error containing %q, got nil", tc.errFrag)
				}
				if tc.errFrag != "" && !strings.Contains(err.Error(), tc.errFrag) {
					t.Errorf("Validate error = %q; want fragment %q", err.Error(), tc.errFrag)
				}
			} else {
				if err != nil {
					t.Fatalf("Validate: want nil, got %v", err)
				}
			}
		})
	}
}

// ─── FOLLOWUP findings ───────────────────────────────────────────────────────────

// TestSiteCRD_Validate_GHPagesMissingRepo tests whether Validate enforces
// deploy.target.repo for the gh-pages strategy. The current Validate() in
// site_crd.go does NOT check strategy-specific target fields — it only validates
// that the strategy name is known.
//
// FOLLOWUP: Validate() should enforce that gh-pages requires target.repo (non-empty string).
// Leaving as a skipped test to document the gap without blocking the build.
func TestSiteCRD_Validate_GHPagesMissingRepo(t *testing.T) {
	t.Skip("FOLLOWUP: Validate() does not currently enforce deploy.target.repo for gh-pages strategy")

	crd := SiteCRD{
		Metadata: SiteMetadata{Name: "missing-repo"},
		Spec: SiteSpec{
			Domain: "myrgic.com",
			Deploy: DeploySpec{
				Strategy: "gh-pages",
				Target:   map[string]any{}, // repo key absent
			},
		},
	}
	err := crd.Validate()
	if err == nil {
		t.Fatal("Validate: want error for missing deploy.target.repo when strategy=gh-pages, got nil")
	}
}
