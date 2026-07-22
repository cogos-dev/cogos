// marginbridge_apply.go — ApplyPlan (author-resolve, echo-suppress, wake
// emission) and BuildState (cursor persistence).
//
// ApplyPlan is read-only against GitHub: it only issues `gh api` GETs
// (commit-author lookup, content fetch for the wake text). Per
// signals/README.md, claiming a signal (moving it to claimed/) is the
// consuming seat's job using git push atomicity as the compare-and-swap —
// this provider only observes and wakes, matching the prototype's own
// posture. Unlike internal/providers/site's ApplyPlan/Deploy (which pushes
// to GH Pages), no PUT/push side effect belongs here.
package marginbridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// ApplyPlan resolves commit authorship for every real (non-baseline,
// non-skip) action, applies echo suppression for watch-file settlements
// authored by the operator's own login, and emits a wake (ledger + bus)
// for everything else.
func (p *Provider) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	var results []reconcile.Result
	for _, action := range plan.Actions {
		if action.Action == reconcile.ActionSkip {
			continue
		}
		results = append(results, p.applyAction(ctx, action))
	}
	return results, nil
}

func (p *Provider) applyAction(ctx context.Context, action reconcile.Action) reconcile.Result {
	base := reconcile.Result{Phase: Type, Action: string(action.Action), Name: action.Name}

	repo, _ := action.Details["repo"].(string)
	path, _ := action.Details["path"].(string)
	kind, _ := action.Details["kind"].(string)
	sha, _ := action.Details["sha"].(string)

	if baseline, _ := action.Details["baseline"].(bool); baseline {
		// First sight of this address: record only, no gh api call, no wake.
		base.Status = reconcile.ApplySucceeded
		return base
	}

	if repo == "" || path == "" {
		base.Status = reconcile.ApplyFailed
		base.Error = "margin-bridge: missing repo/path in action details"
		return base
	}

	gh := p.ghClient()
	login, verified := commitAuthor(ctx, gh, repo, path)
	selfLogin := p.resolveSelfLogin(ctx)
	tier := "untrusted"
	if selfLogin != "" && login == selfLogin {
		tier = "operator"
	}

	// Echo suppression: a watch-file (settlement) change authored by the
	// operator's own login is the seat's own ack push reflecting back, not
	// an inbound signal. Real inbound settlements are Action-authored;
	// inbox entries ride the afferent regardless of tier (matching
	// bridge_github.py lines 139-146 exactly — echo suppression is
	// watch-file-specific, inbox entries always wake when not baseline).
	if kind == "watch" && tier == "operator" {
		base.Status = reconcile.ApplySucceeded
		base.Error = ""
		return base
	}

	text := p.buildWakeText(ctx, gh, repo, kind, path, sha, action.Details)
	sid := fmt.Sprintf("%d", time.Now().UnixMilli())

	payload := map[string]interface{}{
		"kind":     "signal",
		"id":       sid,
		"author":   "gh:" + login,
		"origin":   "github",
		"tier":     tier,
		"verified": verified,
		"path":     path,
		"text":     text,
		"repo":     repo,
	}

	if sink := p.eventSink(); sink != nil {
		if err := sink.EmitLedgerEvent(EventMarginSignal, payload); err != nil {
			base.Status = reconcile.ApplyFailed
			base.Error = fmt.Sprintf("emit ledger event: %v", err)
			return base
		}
		if err := sink.EmitBusEvent(BusMarginBridge, EventMarginSignal, Type, payload); err != nil {
			// Ledger event already landed — the wake happened via
			// cog_tail_events regardless. Bus append failure degrades the
			// SSE-attached class of consumer only; don't fail the cycle.
			base.Error = fmt.Sprintf("emit bus event (non-fatal): %v", err)
		}
	}

	base.Status = reconcile.ApplySucceeded
	base.CreatedID = Type + ":" + action.Name
	return base
}

// buildWakeText builds the human-readable line for a wake, matching the
// prototype's _text_for(): margin receipts get a count, not contents (the
// seat reads the file itself after waking); other inbox signals surface
// their `.text` field; watch-file settlements report the truncated sha.
func (p *Provider) buildWakeText(ctx context.Context, gh ghClient, repo, kind, path, sha string, details map[string]any) string {
	if kind == "watch" {
		short := sha
		if len(short) > 7 {
			short = short[:7]
		}
		return fmt.Sprintf("settled: %s @ %s", path, short)
	}

	name, _ := details["name"].(string)

	if strings.HasPrefix(path, "comments/") {
		text, err := fetchContentText(ctx, gh, repo, path)
		if err != nil {
			return fmt.Sprintf("margin receipt %s", name)
		}
		n := countEntries(text)
		return fmt.Sprintf("margin receipt %s (%d entries)", name, n)
	}

	text, err := fetchContentText(ctx, gh, repo, path)
	if err != nil {
		return fmt.Sprintf("signal %s", name)
	}
	extracted := extractTextField(text)
	if extracted == "" {
		return fmt.Sprintf("signal %s", name)
	}
	if len(extracted) > 200 {
		extracted = extracted[:200]
	}
	return extracted
}

// BuildState mirrors the live snapshot 1:1 into reconcile.State, plus the
// self-throttle bookkeeping (last_polled_at / snapshot_json) in
// state.Metadata consumed by FetchLive's throttledSnapshot.
func (p *Provider) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error) {
	cfg, ok := config.(*Config)
	if !ok || cfg == nil {
		return nil, fmt.Errorf("margin-bridge: BuildState: unexpected config type %T", config)
	}
	if !cfg.Enabled {
		return existing, nil
	}
	snap, ok := live.(*liveSnapshot)
	if !ok {
		return nil, fmt.Errorf("margin-bridge: BuildState: unexpected live type %T", live)
	}

	state := &reconcile.State{Version: 1, ResourceType: Type}
	if existing != nil {
		state.Lineage = existing.Lineage
	} else {
		state.Lineage = Type + "-" + time.Now().UTC().Format("20060102T150405Z")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for dir, entries := range snap.InboxEntries {
		for _, e := range entries {
			state.Resources = append(state.Resources, reconcile.Resource{
				Address:       "inbox:" + e.Path,
				Type:          Type,
				Mode:          reconcile.ModeManaged,
				ExternalID:    e.SHA,
				Name:          e.Name,
				LastRefreshed: now,
				Attributes: map[string]any{
					"kind": "inbox",
					"dir":  dir,
					"path": e.Path,
				},
			})
		}
	}
	for path, sha := range snap.WatchFiles {
		state.Resources = append(state.Resources, reconcile.Resource{
			Address:       "watch:" + path,
			Type:          Type,
			Mode:          reconcile.ModeManaged,
			ExternalID:    sha,
			Name:          path,
			LastRefreshed: now,
			Attributes: map[string]any{
				"kind": "watch",
				"path": path,
			},
		})
	}

	snapJSON, _ := marshalSnapshot(snap)
	state.Metadata = map[string]any{
		"last_polled_at": snap.FetchedAt.Format(time.RFC3339),
		"snapshot_json":  snapJSON,
	}

	return state, nil
}
