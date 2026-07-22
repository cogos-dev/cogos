// marginbridge_plan.go — ComputePlan: the pure diff between live GitHub
// state and the persisted cursor (pkg/reconcile.State), replacing the
// prototype's `seen` dict + _load_cursor/_save_cursor (bridge_github.py
// lines 41-53, 95-115).
//
// ComputePlan does no network I/O — author/tier resolution and the
// echo-suppression check both require a `gh api commits` call, and per the
// Reconcilable contract's documented determinism requirement
// (reconcile_daemon.go's "ComputePlan — pure function, deterministic")
// those live in ApplyPlan instead, matching where the prototype itself
// resolves them (inline in its poll loop, not in a separate planning pass).
package marginbridge

import (
	"fmt"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// ComputePlan diffs the live snapshot against state.Resources by Address:
//
//   - address never before recorded  -> baseline: record only, no wake.
//     Deliberate deviation from the prototype: the script gates inbox-dir
//     baseline on a GLOBAL "seen dict was empty at process start" flag, but
//     gates watch-file baseline per-key ("prev is None", bridge_github.py
//     line 136). The kernel config can change at runtime (a watch_dir or
//     watch_file added later, after other addresses already have state), so
//     this port gates BOTH afferent kinds uniformly on "never seen this
//     specific address before" — strictly safer than the prototype's
//     asymmetric gating (it silently baselines newly-added paths instead of
//     flooding wakes for their pre-existing contents), and behaviorally
//     identical to the prototype for the case that actually exercises it
//     today: a config that never changes after first run.
//   - address recorded, sha unchanged -> skip.
//   - address recorded, sha changed   -> real change; ActionCreate (author
//     resolution + echo-suppression happens in ApplyPlan).
//
// A baseline entry is emitted as ActionUpdate (never ActionSkip). This is a
// second deliberate deviation from the design's literal instruction to use
// ActionSkip for the baseline pass: the reconcile daemon's runOneCycle skips
// BuildState/WriteState entirely when a plan has zero Creates+Updates+
// Deletes (plan.Summary.HasChanges(), reconcile_daemon.go). A plan
// containing only ActionSkip actions on the very first run (state == nil)
// would therefore NEVER persist state — every subsequent tick would reload
// state == nil, ComputePlan would treat it as the first run again, and the
// cursor would never advance past baseline. Tagging baseline entries as
// ActionUpdate (counted in Summary.Updates) forces the state write that
// establishes the cursor, while ApplyPlan treats baseline actions as pure,
// silent no-ops (no gh api call, no wake) so the observable behavior still
// matches "first sight never wakes."
func (p *Provider) ComputePlan(config any, live any, state *reconcile.State) (*reconcile.Plan, error) {
	cfg, ok := config.(*Config)
	if !ok || cfg == nil {
		return nil, fmt.Errorf("margin-bridge: ComputePlan: unexpected config type %T", config)
	}

	p.mu.Lock()
	root := p.root
	p.mu.Unlock()

	plan := &reconcile.Plan{
		ResourceType: Type,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath:   configPath(root),
	}

	if !cfg.Enabled {
		return plan, nil
	}

	snap, ok := live.(*liveSnapshot)
	if !ok {
		return nil, fmt.Errorf("margin-bridge: ComputePlan: unexpected live type %T", live)
	}

	idx := reconcile.ResourceIndex(state) // nil-safe: reading a nil map is legal in Go

	addAction := func(addr, kind, path, name, sha, dir string) {
		details := map[string]any{
			"address": addr,
			"kind":    kind,
			"path":    path,
			"sha":     sha,
			"repo":    cfg.Repo,
		}
		if name != "" {
			details["name"] = name
		}
		if dir != "" {
			details["dir"] = dir
		}

		prev, seen := idx[addr]
		switch {
		case !seen:
			details["baseline"] = true
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action: reconcile.ActionUpdate, ResourceType: Type, Name: addr, Details: details,
			})
			plan.Summary.Updates++
		case prev.ExternalID == sha:
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action: reconcile.ActionSkip, ResourceType: Type, Name: addr, Details: details,
			})
			plan.Summary.Skipped++
		default:
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action: reconcile.ActionCreate, ResourceType: Type, Name: addr, Details: details,
			})
			plan.Summary.Creates++
		}
	}

	for _, dir := range cfg.WatchDirs {
		for _, e := range snap.InboxEntries[dir] {
			addAction("inbox:"+e.Path, "inbox", e.Path, e.Name, e.SHA, dir)
		}
	}
	for _, wf := range cfg.WatchFiles {
		sha, observed := snap.WatchFiles[wf]
		if !observed {
			// Could not be fetched this cycle (absent/transient) — no
			// observation, not a change. Leave any existing state entry
			// untouched (no action emitted for this address this cycle).
			continue
		}
		addAction("watch:"+wf, "watch", wf, "", sha, "")
	}

	return plan, nil
}
