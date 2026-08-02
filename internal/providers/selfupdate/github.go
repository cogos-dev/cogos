// github.go — throttled GitHub release resolver for self-update.
//
// ReleaseResolver.Resolve caches the resolved target release and only re-queries
// GitHub at most once per CheckInterval (default 1h). FetchLive calls Resolve on
// every reconcile tick (~30s), so without this throttle the daemon would hit the
// GitHub API every cycle — the same per-cycle-rescan discipline applied to the
// worktree-ledger cache. On a transient fetch error a cached value is served as
// stale within 2× the interval; cachedAt advances only on success so the next
// tick retries rather than waiting a full interval.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"
)

// httpClientFor is overridable in tests to inject a fake transport.
var httpClientFor = func() *http.Client {
	return &http.Client{}
}

// runningVersionForUA is the version string stamped into the User-Agent.
// Defaults to the engine build version at init time. Guarded by
// runningVersionMu (in provider.go): write only via SetRunningVersion, read
// only via userAgentVersion() — both hold the lock, so concurrent boot-time
// injection and reconcile-loop reads are race-free.
var runningVersionForUA = "dev"

// resolvedRelease is the throttle-cached result of a GitHub release lookup.
type resolvedRelease struct {
	Tag         string // e.g. "v0.16.5"
	Prerelease  bool
	AssetURL    string // download URL for cogos-<goos>-<goarch>
	AssetName   string // "cogos-darwin-arm64"
	ChecksumURL string // download URL for checksums.txt

	// SignatureURL / CertificateURL locate the Sigstore keyless signature over
	// checksums.txt and the Fulcio certificate that signed it. The release job
	// uploads both alongside checksums.txt on every signed release
	// (.github/workflows/release.yml, "Sign checksums.txt (Sigstore keyless)").
	// Releases published before provenance.FirstSignedRelease have neither; the
	// updater treats a 404 on these as "unsigned release" rather than as a
	// verification failure.
	SignatureURL   string // <base>/checksums.txt.sig
	CertificateURL string // <base>/checksums.txt.pem
}

// cacheKey distinguishes cached results by the inputs that affect resolution.
// A config change (repo/channel/pin) invalidates the cache.
type cacheKey struct {
	repo    string
	channel string
	pin     string
}

// ReleaseResolver performs throttled GitHub release lookups.
//
// Thread safety: mu guards the staleness check and the cache write so that
// concurrent reconcile ticks cannot double-fire the GitHub call. The reconcile
// daemon is serial, but Health() probes may run concurrently, so the lock is
// load-bearing. The HTTP request itself runs with the lock held — acceptable
// because the daemon's reconcile loop is single-threaded and the request has a
// 30s context timeout.
type ReleaseResolver struct {
	mu       sync.Mutex
	cached   *resolvedRelease
	cachedAt time.Time
	key      cacheKey
}

// Resolve returns the target release for cfg, served from cache when the
// throttle window has not elapsed.
func (r *ReleaseResolver) Resolve(ctx context.Context, cfg *SelfUpdateConfig) (*resolvedRelease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	k := cacheKey{repo: cfg.Repo, channel: cfg.Channel, pin: cfg.Pin}
	interval := cfg.CheckInterval
	if interval <= 0 {
		interval = defaultCheckInterval
	}

	// GATE A — throttle: serve cache within the interval when the key matches.
	if r.cached != nil && r.key == k && time.Since(r.cachedAt) < interval {
		return r.cached, nil
	}

	rel, err := r.fetch(ctx, cfg)
	if err != nil {
		// Serve stale within 2× the interval on a transient failure.
		if r.cached != nil && r.key == k && time.Since(r.cachedAt) < 2*interval {
			return r.cached, nil
		}
		return nil, err
	}

	// Update cache only on success so errors retry next tick.
	r.cached, r.cachedAt, r.key = rel, time.Now(), k
	return rel, nil
}

// ghRelease is the subset of the GitHub release JSON we consume.
type ghRelease struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
}

// fetch performs the actual GitHub API call and resolves asset URLs.
func (r *ReleaseResolver) fetch(ctx context.Context, cfg *SelfUpdateConfig) (*resolvedRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var rel ghRelease
	switch {
	case cfg.Pin != "":
		if err := r.getJSON(ctx, fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", cfg.Repo, cfg.Pin), &rel); err != nil {
			return nil, err
		}
	case cfg.Channel == channelPrerelease:
		var rels []ghRelease
		if err := r.getJSON(ctx, fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=30", cfg.Repo), &rels); err != nil {
			return nil, err
		}
		picked := false
		for _, candidate := range rels {
			if candidate.Draft {
				continue
			}
			rel = candidate
			picked = true
			break
		}
		if !picked {
			return nil, fmt.Errorf("self-update: no non-draft release found for %s", cfg.Repo)
		}
	default: // stable
		if err := r.getJSON(ctx, fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", cfg.Repo), &rel); err != nil {
			return nil, err
		}
		// /releases/latest already excludes prereleases, but guard anyway.
		if rel.Prerelease {
			return nil, fmt.Errorf("self-update: latest release %s is a prerelease on the stable channel", rel.TagName)
		}
	}

	if rel.TagName == "" {
		return nil, fmt.Errorf("self-update: resolved release has no tag")
	}

	assetName := AssetName()
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", cfg.Repo, rel.TagName)
	return &resolvedRelease{
		Tag:            rel.TagName,
		Prerelease:     rel.Prerelease,
		AssetName:      assetName,
		AssetURL:       base + "/" + assetName,
		ChecksumURL:    base + "/checksums.txt",
		SignatureURL:   base + "/checksums.txt.sig",
		CertificateURL: base + "/checksums.txt.pem",
	}, nil
}

// getJSON issues a GET with GitHub headers and decodes the JSON body into out.
func (r *ReleaseResolver) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "cogos/"+userAgentVersion())

	resp, err := httpClientFor().Do(req)
	if err != nil {
		return fmt.Errorf("self-update: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("self-update: GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("self-update: decoding %s: %w", url, err)
	}
	return nil
}

// AssetName returns the release asset name for the current platform,
// e.g. "cogos-darwin-arm64" or "cogos-windows-amd64.exe".
func AssetName() string {
	name := fmt.Sprintf("cogos-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}
