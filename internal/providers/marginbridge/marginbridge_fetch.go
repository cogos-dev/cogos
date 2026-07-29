// marginbridge_fetch.go — FetchLive and the FetchLive-side self-throttle
// cache.
//
// Mismatch with the prototype: the prototype's POLL_S=40 was a sleep
// interval in its own poll loop. The kernel's ReconcileDaemon has one shared
// ticker for every registered provider (PollInterval, default 30s,
// internal/engine/reconcile_daemon.go:54-56) — there is no per-provider
// interval knob. margin-bridge therefore self-throttles inside FetchLive:
// it persists last_polled_at + a snapshot of what it last actually observed
// in reconcile.State.Metadata (written by BuildState/WriteState), and on
// each call checks whether poll_min_interval_s has elapsed before shelling
// out to `gh api` again. If not, it replays the cached snapshot — same
// shape as internal/conversations/provider.go's coverageCache ("unchanged
// sources served from cache instead of re-parsed every cycle").
//
// Known limitation (documented, not fixed): the reconcile daemon only calls
// BuildState/WriteState when a cycle's plan has at least one non-skip
// action (internal/engine/reconcile_daemon.go's runOneCycle returns early on
// !plan.Summary.HasChanges()). On a cycle where poll_min_interval_s elapsed,
// a real `gh api` fetch happened, but nothing changed (pure ActionSkip
// plan), last_polled_at is NOT refreshed on disk. The next cycle's throttle
// check then sees a stale timestamp and may fetch again sooner than
// poll_min_interval_s. This only makes margin-bridge poll *more* often than
// configured during a quiet steady state — never less often, and never in a
// way that drops or delays a real wake — so it is left as a documented
// efficiency tradeoff rather than added machinery to force a state write on
// every real fetch.
package marginbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// FetchLive retrieves the current live GitHub state: directory listings for
// every watched inbox dir, and content sha for every watched file. Disabled
// configs and self-throttled cache hits both return cheaply without
// shelling out.
func (p *Provider) FetchLive(ctx context.Context, config any) (any, error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("margin-bridge: FetchLive: unexpected config type %T", config)
	}

	if !cfg.Enabled {
		snap := newLiveSnapshot()
		snap.FetchedAt = time.Now().UTC()
		p.cacheSnapshot(snap)
		return snap, nil
	}

	p.mu.Lock()
	root := p.root
	p.mu.Unlock()
	gh := p.ghClient()

	if st, _ := reconcile.LoadState(root, Type); st != nil {
		if cached := throttledSnapshot(st, cfg); cached != nil {
			p.cacheSnapshot(cached)
			return cached, nil
		}
	}

	snap := newLiveSnapshot()
	snap.FetchedAt = time.Now().UTC()

	for _, dir := range cfg.WatchDirs {
		entries, err := listDir(ctx, gh, cfg.Repo, dir)
		if err != nil {
			// Absent dir or transient gh error: skip this dir for this
			// cycle, matching the prototype's `continue` on RuntimeError.
			// Next cycle retries. Recorded in FailedDirs (never silent) so
			// BuildState carries this dir's previously-tracked resources
			// forward instead of dropping them.
			//
			// Throttled (cog-review, PR #496 fourth pass): FetchLive always
			// returns (snap, nil) overall — one broken watch dir must not
			// fail FetchLive for every other dir — so a persistently
			// unreachable dir otherwise repeats this exact warning every
			// ~30s tick forever, invisible to reconcile_daemon.go's
			// phase-level throttle around the FetchLive call.
			snap.FailedDirs[dir] = true
			p.logThrottled("fetchlive-dir:"+dir, err.Error(), slog.LevelWarn,
				"margin-bridge: FetchLive listDir failed", "dir", dir, "err", err)
			continue
		}
		if len(entries) > 0 {
			snap.InboxEntries[dir] = entries
		}
	}

	for _, wf := range cfg.WatchFiles {
		sha, err := fetchContentSHA(ctx, gh, cfg.Repo, wf)
		if err != nil {
			// Absent/transient: treated as "no observation" this cycle, not
			// a change. ComputePlan skips watch files missing from the
			// snapshot entirely. Recorded in FailedFiles (never silent) so
			// BuildState carries this file's previously-tracked resource
			// forward instead of dropping it.
			//
			// Throttled for the same reason as the listDir failure above.
			snap.FailedFiles[wf] = true
			p.logThrottled("fetchlive-file:"+wf, err.Error(), slog.LevelWarn,
				"margin-bridge: FetchLive fetchContentSHA failed", "file", wf, "err", err)
			continue
		}
		snap.WatchFiles[wf] = sha
	}

	p.cacheSnapshot(snap)
	return snap, nil
}

func (p *Provider) cacheSnapshot(snap *liveSnapshot) {
	p.mu.Lock()
	p.lastSnapshot = snap
	p.lastFetchTime = snap.FetchedAt
	p.mu.Unlock()
}

// listDir fetches repos/{repo}/contents/{dir} and returns the file entries,
// skipping subdirectories and housekeeping filenames.
func listDir(ctx context.Context, gh ghClient, repo, dir string) ([]liveEntry, error) {
	data, err := gh.api(ctx, fmt.Sprintf("repos/%s/contents/%s", encodeGHPath(repo), encodeGHPath(dir)))
	if err != nil {
		return nil, err
	}
	var listing []struct {
		Type string `json:"type"`
		Name string `json:"name"`
		Path string `json:"path"`
		SHA  string `json:"sha"`
	}
	if err := json.Unmarshal(data, &listing); err != nil {
		// Not a directory listing (e.g. `dir` resolved to a single file) —
		// nothing to watch as an inbox here.
		return nil, nil
	}
	var out []liveEntry
	for _, f := range listing {
		if f.Type != "file" || ignoredEntryNames[f.Name] {
			continue
		}
		out = append(out, liveEntry{Path: f.Path, Name: f.Name, SHA: f.SHA})
	}
	return out, nil
}

// throttledSnapshot returns the cached snapshot from st.Metadata if
// poll_min_interval_s has not yet elapsed since last_polled_at, or nil if a
// real fetch should happen.
func throttledSnapshot(st *reconcile.State, cfg *Config) *liveSnapshot {
	if st == nil || st.Metadata == nil {
		return nil
	}
	lastStr, _ := st.Metadata["last_polled_at"].(string)
	if lastStr == "" {
		return nil
	}
	lastT, err := time.Parse(time.RFC3339, lastStr)
	if err != nil {
		return nil
	}
	if time.Since(lastT) >= cfg.pollInterval() {
		return nil
	}
	snapJSON, _ := st.Metadata["snapshot_json"].(string)
	if snapJSON == "" {
		return nil
	}
	var snap liveSnapshot
	if err := json.Unmarshal([]byte(snapJSON), &snap); err != nil {
		return nil
	}
	if snap.InboxEntries == nil {
		snap.InboxEntries = make(map[string][]liveEntry)
	}
	if snap.WatchFiles == nil {
		snap.WatchFiles = make(map[string]string)
	}
	if snap.FailedDirs == nil {
		snap.FailedDirs = make(map[string]bool)
	}
	if snap.FailedFiles == nil {
		snap.FailedFiles = make(map[string]bool)
	}
	return &snap
}
