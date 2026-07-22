package marginbridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// ─── mock ghClient ──────────────────────────────────────────────────────────

// mockGH is a scriptable ghClient: responses keyed by exact `gh api <path>`
// argument. Unregistered paths return a 404-shaped error, matching gh CLI's
// own error text for a missing resource.
type mockGH struct {
	responses map[string][]byte
	calls     []string

	// errQueue lets a test script N consecutive failures for a given path
	// before falling through to any registered response (or the default
	// 404) — used to exercise transient-failure-then-recovery paths like
	// resolveSelfLogin's first `gh api user` attempt erroring.
	errQueue map[string][]error
}

func newMockGH() *mockGH {
	return &mockGH{responses: make(map[string][]byte)}
}

func (m *mockGH) api(_ context.Context, path string) ([]byte, error) {
	m.calls = append(m.calls, path)
	if errs, ok := m.errQueue[path]; ok && len(errs) > 0 {
		err := errs[0]
		m.errQueue[path] = errs[1:]
		return nil, err
	}
	if data, ok := m.responses[path]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("gh api %s: exit status 1: HTTP 404: Not Found", path)
}

// queueError makes the next n calls to path return err before falling
// through to any registered response.
func (m *mockGH) queueError(path string, err error, n int) {
	if m.errQueue == nil {
		m.errQueue = make(map[string][]error)
	}
	for i := 0; i < n; i++ {
		m.errQueue[path] = append(m.errQueue[path], err)
	}
}

func (m *mockGH) setJSON(path string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	m.responses[path] = data
}

// setContent registers a repos/{repo}/contents/{path} response with base64
// content and sha, mirroring the real gh api contents endpoint shape.
func (m *mockGH) setContent(repo, path, sha, content string) {
	m.setJSON(fmt.Sprintf("repos/%s/contents/%s", repo, path), map[string]any{
		"sha":      sha,
		"content":  base64.StdEncoding.EncodeToString([]byte(content)),
		"encoding": "base64",
	})
}

// setDirListing registers a repos/{repo}/contents/{dir} directory listing.
func (m *mockGH) setDirListing(repo, dir string, entries []dirEntry) {
	var listing []map[string]any
	for _, e := range entries {
		listing = append(listing, map[string]any{
			"type": "file",
			"name": e.name,
			"path": dir + "/" + e.name,
			"sha":  e.sha,
		})
	}
	m.setJSON(fmt.Sprintf("repos/%s/contents/%s", repo, dir), listing)
}

type dirEntry struct {
	name string
	sha  string
}

// setCommitAuthor registers the commits?path=... response used by
// commitAuthor. Keyed with the same repo/path encoding commitAuthor itself
// applies (path is a query value, so it is fully query-escaped; repo is a
// REST path component, so only its "/"-separated segments are escaped).
func (m *mockGH) setCommitAuthor(repo, path, login string, verified bool) {
	m.setJSON(fmt.Sprintf("repos/%s/commits?path=%s&per_page=1", encodeGHPath(repo), url.QueryEscape(path)), []map[string]any{
		{
			"author": map[string]any{"login": login},
			"commit": map[string]any{
				"author":       map[string]any{"name": login},
				"verification": map[string]any{"verified": verified},
			},
		},
	})
}

func (m *mockGH) setSelfLogin(login string) {
	m.setJSON("user", map[string]any{"login": login})
}

// ─── mock EventSink ─────────────────────────────────────────────────────────

type emittedEvent struct {
	kind    string // "ledger" or "bus"
	busID   string
	evtType string
	from    string
	data    map[string]interface{}
}

type mockSink struct {
	events []emittedEvent
}

func (s *mockSink) EmitLedgerEvent(eventType string, data map[string]interface{}) error {
	s.events = append(s.events, emittedEvent{kind: "ledger", evtType: eventType, data: data})
	return nil
}

func (s *mockSink) EmitBusEvent(busID, eventType, from string, payload map[string]interface{}) error {
	s.events = append(s.events, emittedEvent{kind: "bus", busID: busID, evtType: eventType, from: from, data: payload})
	return nil
}

// ─── test helpers ───────────────────────────────────────────────────────────

func newTestProvider(gh ghClient) *Provider {
	p := NewProvider()
	p.setGHClient(gh)
	return p
}

func writeConfig(t *testing.T, root string, yamlBody string) {
	t.Helper()
	dir := filepath.Join(root, ".cog", "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "margin-bridge.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

const testYAML = `
repo: myrgic/thinking-through-distinction-internal
operator_gh_login: chazmaniandinkle
watch_dirs: [comments/inbox, signals/inbox]
watch_files: [comments/ledger.json]
poll_min_interval_s: 40
`

// ─── LoadConfig ─────────────────────────────────────────────────────────────

func TestLoadConfig_AbsentFileIsDisabledNotError(t *testing.T) {
	root := t.TempDir()
	p := NewProvider()
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: unexpected error: %v", err)
	}
	c, ok := cfg.(*Config)
	if !ok {
		t.Fatalf("LoadConfig: expected *Config, got %T", cfg)
	}
	if c.Enabled {
		t.Errorf("LoadConfig: expected Enabled=false for absent config file")
	}
	status := p.Health()
	if status.Health != reconcile.HealthSuspended {
		t.Errorf("Health: got %v; want Suspended for absent config", status.Health)
	}
}

func TestLoadConfig_PresentFileEnablesAndDefaults(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "repo: my/repo\nwatch_dirs: [signals/inbox]\n")
	p := NewProvider()
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	c := cfg.(*Config)
	if !c.Enabled {
		t.Fatalf("LoadConfig: expected Enabled=true")
	}
	if got := c.pollInterval().Seconds(); got != defaultPollMinIntervalS {
		t.Errorf("pollInterval default: got %v; want %v", got, defaultPollMinIntervalS)
	}
}

func TestLoadConfig_MissingRepoIsError(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "watch_dirs: [signals/inbox]\n")
	p := NewProvider()
	if _, err := p.LoadConfig(root); err == nil {
		t.Fatalf("LoadConfig: expected error for missing repo")
	}
}

// ─── ComputePlan: baseline / skip / create diffing ─────────────────────────

func TestComputePlan_FirstRunBaselinesEverythingAsUpdate(t *testing.T) {
	p := newTestProvider(newMockGH())
	root := t.TempDir()
	writeConfig(t, root, testYAML)
	cfgAny, _ := p.LoadConfig(root)

	snap := newLiveSnapshot()
	snap.InboxEntries["comments/inbox"] = []liveEntry{{Path: "comments/inbox/a.json", Name: "a.json", SHA: "sha-a"}}
	snap.WatchFiles["comments/ledger.json"] = "sha-ledger-1"

	plan, err := p.ComputePlan(cfgAny, snap, nil) // state == nil: very first run
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Creates != 0 {
		t.Errorf("first run: expected 0 creates (baseline, no wake); got %d", plan.Summary.Creates)
	}
	if plan.Summary.Updates != 2 {
		t.Errorf("first run: expected 2 baseline updates (1 inbox + 1 watch); got %d", plan.Summary.Updates)
	}
	for _, a := range plan.Actions {
		if a.Action != reconcile.ActionUpdate {
			t.Errorf("first run: expected all actions to be ActionUpdate; got %v for %s", a.Action, a.Name)
		}
		if baseline, _ := a.Details["baseline"].(bool); !baseline {
			t.Errorf("first run: expected baseline=true in details for %s", a.Name)
		}
	}
}

func TestComputePlan_UnchangedShaIsSkip(t *testing.T) {
	p := newTestProvider(newMockGH())
	state := &reconcile.State{
		Resources: []reconcile.Resource{
			{Address: "inbox:comments/inbox/a.json", ExternalID: "sha-a"},
		},
	}
	snap := newLiveSnapshot()
	snap.InboxEntries["comments/inbox"] = []liveEntry{{Path: "comments/inbox/a.json", Name: "a.json", SHA: "sha-a"}}

	cfg := &Config{Enabled: true, Repo: "my/repo", WatchDirs: []string{"comments/inbox"}}
	plan, err := p.ComputePlan(cfg, snap, state)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Skipped != 1 || plan.Summary.Creates != 0 || plan.Summary.Updates != 0 {
		t.Errorf("unchanged sha: expected 1 skip only; got %+v", plan.Summary)
	}
}

func TestComputePlan_ChangedShaIsCreate(t *testing.T) {
	p := newTestProvider(newMockGH())
	state := &reconcile.State{
		Resources: []reconcile.Resource{
			{Address: "watch:comments/ledger.json", ExternalID: "sha-old"},
		},
	}
	snap := newLiveSnapshot()
	snap.WatchFiles["comments/ledger.json"] = "sha-new"

	cfg := &Config{Enabled: true, Repo: "my/repo", WatchFiles: []string{"comments/ledger.json"}}
	plan, err := p.ComputePlan(cfg, snap, state)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Creates != 1 {
		t.Fatalf("changed sha: expected 1 create; got %+v", plan.Summary)
	}
	if plan.Actions[0].Details["kind"] != "watch" {
		t.Errorf("changed sha: expected kind=watch; got %v", plan.Actions[0].Details["kind"])
	}
}

func TestComputePlan_NewKeyAfterOthersAlreadyTrackedIsBaselineNotCreate(t *testing.T) {
	// Deliberate deviation from the prototype: a never-before-seen address
	// baselines silently even when state is non-nil (i.e. even when this is
	// NOT the very first cycle overall) — see marginbridge_plan.go doc
	// comment. This guards against a flood of false wakes when a new
	// watch_dir/watch_file is added to an already-running config.
	p := newTestProvider(newMockGH())
	state := &reconcile.State{
		Resources: []reconcile.Resource{
			{Address: "watch:comments/ledger.json", ExternalID: "sha-old"},
		},
	}
	snap := newLiveSnapshot()
	snap.WatchFiles["comments/ledger.json"] = "sha-old" // unchanged
	snap.InboxEntries["signals/inbox"] = []liveEntry{{Path: "signals/inbox/new.json", Name: "new.json", SHA: "sha-x"}}

	cfg := &Config{Enabled: true, Repo: "my/repo", WatchFiles: []string{"comments/ledger.json"}, WatchDirs: []string{"signals/inbox"}}
	plan, err := p.ComputePlan(cfg, snap, state)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Creates != 0 {
		t.Errorf("new key: expected 0 creates (baseline); got %d", plan.Summary.Creates)
	}
	if plan.Summary.Updates != 1 {
		t.Errorf("new key: expected 1 baseline update; got %d", plan.Summary.Updates)
	}
	if plan.Summary.Skipped != 1 {
		t.Errorf("unchanged watch file: expected 1 skip; got %d", plan.Summary.Skipped)
	}
}

func TestComputePlan_DisabledConfigProducesEmptyPlan(t *testing.T) {
	p := newTestProvider(newMockGH())
	cfg := &Config{Enabled: false}
	plan, err := p.ComputePlan(cfg, newLiveSnapshot(), nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if len(plan.Actions) != 0 {
		t.Errorf("disabled config: expected no actions; got %d", len(plan.Actions))
	}
}

// ─── ApplyPlan: tier / echo-suppression / wake emission ────────────────────

func TestApplyPlan_BaselineActionIsSilentNoOp(t *testing.T) {
	gh := newMockGH()
	p := newTestProvider(gh)
	sink := &mockSink{}
	p.SetEventSink(sink)

	plan := &reconcile.Plan{
		ResourceType: Type,
		Actions: []reconcile.Action{
			{Action: reconcile.ActionUpdate, Name: "inbox:signals/inbox/a.json", Details: map[string]any{
				"baseline": true, "repo": "my/repo", "path": "signals/inbox/a.json", "kind": "inbox", "sha": "s1",
			}},
		},
	}
	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != reconcile.ApplySucceeded {
		t.Fatalf("baseline action: expected 1 succeeded result; got %+v", results)
	}
	if len(sink.events) != 0 {
		t.Errorf("baseline action: expected zero emitted events; got %d", len(sink.events))
	}
	if len(gh.calls) != 0 {
		t.Errorf("baseline action: expected zero gh api calls; got %v", gh.calls)
	}
}

func TestApplyPlan_UntrustedInboxSignalWakes(t *testing.T) {
	gh := newMockGH()
	gh.setSelfLogin("chazmaniandinkle")
	gh.setCommitAuthor("my/repo", "signals/inbox/a.json", "some-bot", false)
	gh.setContent("my/repo", "signals/inbox/a.json", "s1", `{"text":"hello from a bot"}`)

	p := newTestProvider(gh)
	sink := &mockSink{}
	p.SetEventSink(sink)

	plan := &reconcile.Plan{
		Actions: []reconcile.Action{
			{Action: reconcile.ActionCreate, Name: "inbox:signals/inbox/a.json", Details: map[string]any{
				"repo": "my/repo", "path": "signals/inbox/a.json", "kind": "inbox", "sha": "s1", "name": "a.json",
			}},
		},
	}
	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if results[0].Status != reconcile.ApplySucceeded {
		t.Fatalf("expected succeeded; got %+v", results[0])
	}
	if len(sink.events) != 2 {
		t.Fatalf("expected 2 emitted events (ledger+bus); got %d: %+v", len(sink.events), sink.events)
	}
	payload := sink.events[0].data
	if payload["tier"] != "untrusted" {
		t.Errorf("expected tier=untrusted for non-operator author; got %v", payload["tier"])
	}
	if payload["author"] != "gh:some-bot" {
		t.Errorf("expected author=gh:some-bot; got %v", payload["author"])
	}
	if payload["text"] != "hello from a bot" {
		t.Errorf("expected extracted text field; got %v", payload["text"])
	}
}

func TestApplyPlan_OperatorInboxSignalStillWakes(t *testing.T) {
	// Inbox entries always wake when not baseline, regardless of author tier
	// — echo suppression is watch-file-specific (bridge_github.py lines
	// 139-146: "phone receipts ride the inbox afferent regardless").
	gh := newMockGH()
	gh.setSelfLogin("chazmaniandinkle")
	gh.setCommitAuthor("my/repo", "comments/inbox/receipt.json", "chazmaniandinkle", true)
	gh.setContent("my/repo", "comments/inbox/receipt.json", "s2", `[{"c":1},{"c":2}]`)

	p := newTestProvider(gh)
	sink := &mockSink{}
	p.SetEventSink(sink)

	plan := &reconcile.Plan{
		Actions: []reconcile.Action{
			{Action: reconcile.ActionCreate, Name: "inbox:comments/inbox/receipt.json", Details: map[string]any{
				"repo": "my/repo", "path": "comments/inbox/receipt.json", "kind": "inbox", "sha": "s2", "name": "receipt.json",
			}},
		},
	}
	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(sink.events) == 0 {
		t.Fatalf("operator-authored inbox receipt: expected a wake, got none")
	}
	payload := sink.events[0].data
	if payload["tier"] != "operator" {
		t.Errorf("expected tier=operator; got %v", payload["tier"])
	}
	if payload["text"] != "margin receipt receipt.json (2 entries)" {
		t.Errorf("expected margin receipt count text; got %v", payload["text"])
	}
	_ = results
}

func TestApplyPlan_WatchFileEchoFromOperatorIsSuppressed(t *testing.T) {
	gh := newMockGH()
	gh.setSelfLogin("chazmaniandinkle")
	gh.setCommitAuthor("my/repo", "comments/ledger.json", "chazmaniandinkle", true)

	p := newTestProvider(gh)
	sink := &mockSink{}
	p.SetEventSink(sink)

	plan := &reconcile.Plan{
		Actions: []reconcile.Action{
			{Action: reconcile.ActionCreate, Name: "watch:comments/ledger.json", Details: map[string]any{
				"repo": "my/repo", "path": "comments/ledger.json", "kind": "watch", "sha": "abcdef1234567",
			}},
		},
	}
	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if results[0].Status != reconcile.ApplySucceeded {
		t.Fatalf("echo-suppressed action should still succeed; got %+v", results[0])
	}
	if len(sink.events) != 0 {
		t.Errorf("echo suppression: expected zero emitted events for operator-authored settle; got %d", len(sink.events))
	}
}

func TestApplyPlan_WatchFileFromUntrustedAuthorWakes(t *testing.T) {
	gh := newMockGH()
	gh.setSelfLogin("chazmaniandinkle")
	gh.setCommitAuthor("my/repo", "comments/ledger.json", "someone-else", false)

	p := newTestProvider(gh)
	sink := &mockSink{}
	p.SetEventSink(sink)

	plan := &reconcile.Plan{
		Actions: []reconcile.Action{
			{Action: reconcile.ActionCreate, Name: "watch:comments/ledger.json", Details: map[string]any{
				"repo": "my/repo", "path": "comments/ledger.json", "kind": "watch", "sha": "abcdef1234567",
			}},
		},
	}
	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if results[0].Status != reconcile.ApplySucceeded {
		t.Fatalf("expected succeeded; got %+v", results[0])
	}
	if len(sink.events) != 2 {
		t.Fatalf("expected wake (ledger+bus) for non-operator settle; got %d", len(sink.events))
	}
	payload := sink.events[0].data
	if payload["text"] != "settled: comments/ledger.json @ abcdef1" {
		t.Errorf("expected truncated-sha settle text; got %v", payload["text"])
	}
	if payload["tier"] != "untrusted" {
		t.Errorf("expected tier=untrusted; got %v", payload["tier"])
	}
}

func TestResolveSelfLogin_TransientErrorDoesNotPermanentlyCacheEmptyLogin(t *testing.T) {
	gh := newMockGH()
	// The first `gh api user` attempt fails transiently (rate limit,
	// network blip, auth hiccup during startup); a later attempt succeeds
	// and returns the real operator login.
	gh.queueError("user", fmt.Errorf("gh api user: exit status 1: HTTP 500: rate limited"), 1)
	gh.setSelfLogin("chazmaniandinkle")
	gh.setCommitAuthor("my/repo", "comments/ledger.json", "chazmaniandinkle", true)

	p := newTestProvider(gh)
	sink := &mockSink{}
	p.SetEventSink(sink)

	watchAction := reconcile.Action{
		Action: reconcile.ActionCreate, Name: "watch:comments/ledger.json", Details: map[string]any{
			"repo": "my/repo", "path": "comments/ledger.json", "kind": "watch", "sha": "abcdef1234567",
		},
	}

	// Evaluation 1: resolveSelfLogin's `gh api user` call errors. The
	// erroring attempt must NOT be cached as resolved: tier stays
	// "untrusted" for this evaluation (the operator-authored settlement
	// wakes as if external, since self-login couldn't be confirmed), but
	// selfLoginResolv must remain false so the NEXT call retries instead of
	// being stuck untrusted for the rest of the process's lifetime.
	results, err := p.ApplyPlan(context.Background(), &reconcile.Plan{Actions: []reconcile.Action{watchAction}})
	if err != nil {
		t.Fatalf("ApplyPlan (evaluation 1): %v", err)
	}
	if results[0].Status != reconcile.ApplySucceeded {
		t.Fatalf("evaluation 1: expected succeeded; got %+v", results[0])
	}
	if len(sink.events) != 2 {
		t.Fatalf("evaluation 1: expected a wake (tier untrusted while self-login is unresolved); got %d events", len(sink.events))
	}
	if got := sink.events[0].data["tier"]; got != "untrusted" {
		t.Errorf("evaluation 1: expected tier=untrusted while self-login is unresolved; got %v", got)
	}

	p.mu.Lock()
	resolved := p.selfLoginResolv
	p.mu.Unlock()
	if resolved {
		t.Fatalf("resolveSelfLogin: an erroring attempt must not set selfLoginResolv=true (would permanently disable echo suppression)")
	}

	// Evaluation 2: a later call retries and `gh api user` now succeeds.
	// selfLogin resolves to the operator's login and the echo-suppression
	// branch fires for the same watch-file settlement.
	sink.events = nil
	results, err = p.ApplyPlan(context.Background(), &reconcile.Plan{Actions: []reconcile.Action{watchAction}})
	if err != nil {
		t.Fatalf("ApplyPlan (evaluation 2): %v", err)
	}
	if results[0].Status != reconcile.ApplySucceeded {
		t.Fatalf("evaluation 2: expected succeeded; got %+v", results[0])
	}
	if len(sink.events) != 0 {
		t.Errorf("evaluation 2: expected echo suppression once self-login resolves to the operator; got %d events: %+v", len(sink.events), sink.events)
	}

	p.mu.Lock()
	resolved = p.selfLoginResolv
	login := p.selfLogin
	p.mu.Unlock()
	if !resolved || login != "chazmaniandinkle" {
		t.Errorf("resolveSelfLogin: expected resolved=true login=chazmaniandinkle after the successful retry; got resolved=%v login=%q", resolved, login)
	}
}

func TestApplyPlan_SkipActionsAreNotApplied(t *testing.T) {
	gh := newMockGH()
	p := newTestProvider(gh)
	sink := &mockSink{}
	p.SetEventSink(sink)

	plan := &reconcile.Plan{
		Actions: []reconcile.Action{
			{Action: reconcile.ActionSkip, Name: "watch:comments/ledger.json", Details: map[string]any{}},
		},
	}
	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("skip actions: expected zero results; got %d", len(results))
	}
	if len(gh.calls) != 0 {
		t.Errorf("skip actions: expected zero gh api calls; got %v", gh.calls)
	}
}

// ─── Health ─────────────────────────────────────────────────────────────────

func TestHealth_ReportsUnclaimedSignalCount(t *testing.T) {
	p := newTestProvider(newMockGH())
	root := t.TempDir()
	writeConfig(t, root, testYAML)
	cfgAny, _ := p.LoadConfig(root)
	cfg := cfgAny.(*Config)
	cfg.WatchDirs = []string{"signals/inbox"}

	snap := newLiveSnapshot()
	snap.InboxEntries["signals/inbox"] = []liveEntry{
		{Path: "signals/inbox/a.json", Name: "a.json", SHA: "s1"},
		{Path: "signals/inbox/b.json", Name: "b.json", SHA: "s2"},
	}
	if _, err := p.FetchLive(context.Background(), cfg); err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	// FetchLive above hit the mock (empty dir listing since not registered);
	// directly seed the cache to test Health()'s reporting in isolation.
	p.cacheSnapshot(snap)

	status := p.Health()
	if status.Health != reconcile.HealthDegraded {
		t.Errorf("Health with unclaimed signals: got %v; want Degraded", status.Health)
	}
	if status.Sync != reconcile.SyncStatusOutOfSync {
		t.Errorf("Health with unclaimed signals: got sync %v; want OutOfSync", status.Sync)
	}
}

func TestHealth_QuietWhenNoUnclaimedSignals(t *testing.T) {
	p := newTestProvider(newMockGH())
	root := t.TempDir()
	writeConfig(t, root, testYAML)
	cfgAny, _ := p.LoadConfig(root)
	_ = cfgAny

	p.cacheSnapshot(newLiveSnapshot())
	status := p.Health()
	if status.Health != reconcile.HealthHealthy {
		t.Errorf("Health with no signals: got %v; want Healthy", status.Health)
	}
}

// ─── BuildState / self-throttle round trip ─────────────────────────────────

func TestBuildStateAndThrottledSnapshotRoundTrip(t *testing.T) {
	p := newTestProvider(newMockGH())
	cfg := &Config{Enabled: true, Repo: "my/repo", PollMinIntervalS: 40}

	snap := newLiveSnapshot()
	snap.FetchedAt = time.Now().UTC()
	snap.InboxEntries["comments/inbox"] = []liveEntry{{Path: "comments/inbox/a.json", Name: "a.json", SHA: "sha-a"}}
	snap.WatchFiles["comments/ledger.json"] = "sha-ledger"

	state, err := p.BuildState(cfg, snap, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if len(state.Resources) != 2 {
		t.Fatalf("BuildState: expected 2 resources; got %d", len(state.Resources))
	}

	cached := throttledSnapshot(state, cfg)
	if cached == nil {
		t.Fatalf("throttledSnapshot: expected a cache hit immediately after BuildState")
	}
	if cached.WatchFiles["comments/ledger.json"] != "sha-ledger" {
		t.Errorf("throttledSnapshot: cached snapshot missing watch file sha")
	}
}

// ─── Partial/failed fetch: carry-forward, never silently drop ─────────────
//
// Regression coverage for the PR #468 cog-review CHANGES_REQUESTED finding:
// a transient `gh api` error on one watched dir/file (FetchLive) combined
// with BuildState rebuilding state.Resources solely from the current
// snapshot (no merge against existing) permanently dropped that scope's
// tracked history the moment any other change in the same cycle forced a
// state write. The next successful fetch then treated every address under
// the dropped scope as never-before-seen and silently re-baselined it —
// swallowing a real signal with zero observability. These tests exercise
// both affected afferent kinds (inbox dir, watch file).

func TestBuildState_FailedScopesCarryForwardButLegitimateEmptyDoesNot(t *testing.T) {
	p := newTestProvider(newMockGH())
	existing := &reconcile.State{
		Lineage: "margin-bridge-existing",
		Resources: []reconcile.Resource{
			{
				Address: "inbox:signals/inbox/a.json", Type: Type, ExternalID: "sha-a",
				Attributes: map[string]any{"kind": "inbox", "dir": "signals/inbox", "path": "signals/inbox/a.json"},
			},
			{
				Address: "inbox:comments/inbox/old.json", Type: Type, ExternalID: "sha-old",
				Attributes: map[string]any{"kind": "inbox", "dir": "comments/inbox", "path": "comments/inbox/old.json"},
			},
			{
				Address: "watch:comments/ledger.json", Type: Type, ExternalID: "sha-ledger-1",
				Attributes: map[string]any{"kind": "watch", "path": "comments/ledger.json"},
			},
		},
	}

	snap := newLiveSnapshot()
	snap.FetchedAt = time.Now().UTC()
	// signals/inbox's listing failed this cycle (transient gh error) —
	// a.json must be carried forward.
	snap.FailedDirs["signals/inbox"] = true
	// comments/inbox fetched successfully and is now genuinely empty
	// (old.json was claimed/removed by the consuming seat) — NOT a failed
	// scope, so old.json must be dropped, not carried forward.
	// comments/ledger.json's content-sha fetch failed this cycle — its
	// resource must be carried forward.
	snap.FailedFiles["comments/ledger.json"] = true

	cfg := &Config{Enabled: true, Repo: "my/repo", WatchDirs: []string{"signals/inbox", "comments/inbox"}, WatchFiles: []string{"comments/ledger.json"}}
	state, err := p.BuildState(cfg, snap, existing)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}

	idx := reconcile.ResourceIndex(state)
	if r, ok := idx["inbox:signals/inbox/a.json"]; !ok || r.ExternalID != "sha-a" {
		t.Errorf("expected a.json's resource to be carried forward from the failed dir; got %+v (ok=%v)", r, ok)
	}
	if _, ok := idx["inbox:comments/inbox/old.json"]; ok {
		t.Errorf("expected old.json's resource to be dropped: comments/inbox fetched successfully and is legitimately empty, not a failed scope")
	}
	if r, ok := idx["watch:comments/ledger.json"]; !ok || r.ExternalID != "sha-ledger-1" {
		t.Errorf("expected the ledger's resource to be carried forward from the failed file fetch; got %+v (ok=%v)", r, ok)
	}
	if len(state.Resources) != 2 {
		t.Errorf("expected exactly 2 resources (a.json + ledger carried forward, old.json dropped); got %d: %+v", len(state.Resources), state.Resources)
	}
}

func TestFetchLive_TransientErrorRecordsFailedScopeAndLogs(t *testing.T) {
	gh := newMockGH()
	p := newTestProvider(gh)
	root := t.TempDir()
	writeConfig(t, root, "repo: my/repo\nwatch_dirs: [signals/inbox]\nwatch_files: [comments/ledger.json]\n")
	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg := cfgAny.(*Config)

	dirPath := "repos/my/repo/contents/signals/inbox"
	filePath := "repos/my/repo/contents/comments/ledger.json"
	gh.queueError(dirPath, fmt.Errorf("gh api %s: exit status 1: HTTP 500: rate limited", dirPath), 1)
	gh.queueError(filePath, fmt.Errorf("gh api %s: exit status 1: HTTP 500: rate limited", filePath), 1)

	oldOutput := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(oldOutput)

	liveAny, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	snap, ok := liveAny.(*liveSnapshot)
	if !ok {
		t.Fatalf("FetchLive: expected *liveSnapshot; got %T", liveAny)
	}
	if !snap.FailedDirs["signals/inbox"] {
		t.Errorf("expected signals/inbox to be recorded in FailedDirs after a transient gh error")
	}
	if !snap.FailedFiles["comments/ledger.json"] {
		t.Errorf("expected comments/ledger.json to be recorded in FailedFiles after a transient gh error")
	}
	if _, ok := snap.InboxEntries["signals/inbox"]; ok {
		t.Errorf("expected no InboxEntries for a dir whose listing failed")
	}
	if _, ok := snap.WatchFiles["comments/ledger.json"]; ok {
		t.Errorf("expected no WatchFiles entry for a file whose fetch failed")
	}

	logged := buf.String()
	if !strings.Contains(logged, "signals/inbox") {
		t.Errorf("expected the dir fetch failure to be logged (never silent); got log output: %q", logged)
	}
	if !strings.Contains(logged, "comments/ledger.json") {
		t.Errorf("expected the file fetch failure to be logged (never silent); got log output: %q", logged)
	}
}

func TestPartialFetchFailure_InboxDirCarriesForwardAndWakesOnLaterChange(t *testing.T) {
	gh := newMockGH()
	gh.setDirListing("my/repo", "signals/inbox", []dirEntry{{name: "a.json", sha: "sha-a1"}})
	p := newTestProvider(gh)
	root := t.TempDir()
	writeConfig(t, root, "repo: my/repo\nwatch_dirs: [signals/inbox]\n")
	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg := cfgAny.(*Config)

	// Cycle 1 (healthy): a.json baselines into state — first sight, no wake.
	live1, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive (cycle 1): %v", err)
	}
	plan1, err := p.ComputePlan(cfg, live1, nil)
	if err != nil {
		t.Fatalf("ComputePlan (cycle 1): %v", err)
	}
	if plan1.Summary.Updates != 1 {
		t.Fatalf("cycle 1: expected 1 baseline update; got %+v", plan1.Summary)
	}
	state1, err := p.BuildState(cfg, live1, nil)
	if err != nil {
		t.Fatalf("BuildState (cycle 1): %v", err)
	}
	if r, ok := reconcile.ResourceIndex(state1)["inbox:signals/inbox/a.json"]; !ok || r.ExternalID != "sha-a1" {
		t.Fatalf("cycle 1: expected a.json tracked at sha-a1; got %+v (ok=%v)", r, ok)
	}

	// Cycle 2: signals/inbox's listing transiently errors (rate limit,
	// network blip). In the real daemon, some other change elsewhere in the
	// same cycle would force BuildState/WriteState to run regardless
	// (reconcile_daemon only skips the write when the whole plan has zero
	// changes) — BuildState is exercised unconditionally here to match that.
	dirPath := "repos/my/repo/contents/signals/inbox"
	gh.queueError(dirPath, fmt.Errorf("gh api %s: exit status 1: HTTP 500: rate limited", dirPath), 1)
	live2, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive (cycle 2): %v", err)
	}
	snap2 := live2.(*liveSnapshot)
	if !snap2.FailedDirs["signals/inbox"] {
		t.Fatalf("cycle 2: expected signals/inbox recorded as a failed dir")
	}
	state2, err := p.BuildState(cfg, live2, state1)
	if err != nil {
		t.Fatalf("BuildState (cycle 2): %v", err)
	}
	r2, ok := reconcile.ResourceIndex(state2)["inbox:signals/inbox/a.json"]
	if !ok {
		t.Fatalf("cycle 2: a.json's tracked resource was dropped after a transient fetch failure — the defect this test guards against")
	}
	if r2.ExternalID != "sha-a1" {
		t.Errorf("cycle 2: expected a.json's carried-forward sha to be unchanged (sha-a1); got %s", r2.ExternalID)
	}

	// Cycle 3: fetch recovers. a.json's real content changed while we
	// couldn't observe it — a genuine signal that arrived during the gap.
	gh.setDirListing("my/repo", "signals/inbox", []dirEntry{{name: "a.json", sha: "sha-a2"}})
	live3, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive (cycle 3): %v", err)
	}
	plan3, err := p.ComputePlan(cfg, live3, state2)
	if err != nil {
		t.Fatalf("ComputePlan (cycle 3): %v", err)
	}
	if plan3.Summary.Creates != 1 {
		t.Fatalf("cycle 3: expected a.json's real content change to wake (ActionCreate) via the preserved cursor; got %+v", plan3.Summary)
	}
	action := plan3.Actions[0]
	if action.Action != reconcile.ActionCreate {
		t.Errorf("cycle 3: expected ActionCreate; got %v", action.Action)
	}
	if baseline, _ := action.Details["baseline"].(bool); baseline {
		t.Errorf("cycle 3: a.json's real change was silently re-baselined instead of waking — the defect this test guards against")
	}
}

func TestPartialFetchFailure_WatchFileCarriesForwardAndWakesOnLaterChange(t *testing.T) {
	gh := newMockGH()
	gh.setContent("my/repo", "comments/ledger.json", "sha-l1", `{"comments":[]}`)
	p := newTestProvider(gh)
	root := t.TempDir()
	writeConfig(t, root, "repo: my/repo\nwatch_files: [comments/ledger.json]\n")
	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg := cfgAny.(*Config)

	// Cycle 1 (healthy): the ledger baselines into state.
	live1, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive (cycle 1): %v", err)
	}
	plan1, err := p.ComputePlan(cfg, live1, nil)
	if err != nil {
		t.Fatalf("ComputePlan (cycle 1): %v", err)
	}
	if plan1.Summary.Updates != 1 {
		t.Fatalf("cycle 1: expected 1 baseline update; got %+v", plan1.Summary)
	}
	state1, err := p.BuildState(cfg, live1, nil)
	if err != nil {
		t.Fatalf("BuildState (cycle 1): %v", err)
	}
	if r, ok := reconcile.ResourceIndex(state1)["watch:comments/ledger.json"]; !ok || r.ExternalID != "sha-l1" {
		t.Fatalf("cycle 1: expected ledger tracked at sha-l1; got %+v (ok=%v)", r, ok)
	}

	// Cycle 2: the ledger's content-sha fetch transiently errors.
	filePath := "repos/my/repo/contents/comments/ledger.json"
	gh.queueError(filePath, fmt.Errorf("gh api %s: exit status 1: HTTP 500: rate limited", filePath), 1)
	live2, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive (cycle 2): %v", err)
	}
	snap2 := live2.(*liveSnapshot)
	if !snap2.FailedFiles["comments/ledger.json"] {
		t.Fatalf("cycle 2: expected comments/ledger.json recorded as a failed file")
	}
	state2, err := p.BuildState(cfg, live2, state1)
	if err != nil {
		t.Fatalf("BuildState (cycle 2): %v", err)
	}
	r2, ok := reconcile.ResourceIndex(state2)["watch:comments/ledger.json"]
	if !ok {
		t.Fatalf("cycle 2: ledger's tracked resource was dropped after a transient fetch failure — the defect this test guards against")
	}
	if r2.ExternalID != "sha-l1" {
		t.Errorf("cycle 2: expected the carried-forward sha to be unchanged (sha-l1); got %s", r2.ExternalID)
	}

	// Cycle 3: fetch recovers. The ledger's real content (a settlement)
	// changed while we couldn't observe it.
	gh.setContent("my/repo", "comments/ledger.json", "sha-l2", `{"comments":[{"a":1}]}`)
	live3, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive (cycle 3): %v", err)
	}
	plan3, err := p.ComputePlan(cfg, live3, state2)
	if err != nil {
		t.Fatalf("ComputePlan (cycle 3): %v", err)
	}
	if plan3.Summary.Creates != 1 {
		t.Fatalf("cycle 3: expected the ledger's real change to wake (ActionCreate) via the preserved cursor; got %+v", plan3.Summary)
	}
	action := plan3.Actions[0]
	if action.Action != reconcile.ActionCreate {
		t.Errorf("cycle 3: expected ActionCreate; got %v", action.Action)
	}
	if baseline, _ := action.Details["baseline"].(bool); baseline {
		t.Errorf("cycle 3: the ledger's real change was silently re-baselined instead of waking — the defect this test guards against")
	}
}

func TestCountEntriesAndExtractTextField(t *testing.T) {
	if n := countEntries(`[{"a":1},{"a":2},{"a":3}]`); n != 3 {
		t.Errorf("countEntries array: got %d; want 3", n)
	}
	if n := countEntries(`{"comments":[{"a":1}]}`); n != 1 {
		t.Errorf("countEntries object.comments: got %d; want 1", n)
	}
	if got := extractTextField(`{"text":"hi there"}`); got != "hi there" {
		t.Errorf("extractTextField: got %q; want %q", got, "hi there")
	}
	if got := extractTextField(`not json`); got != "" {
		t.Errorf("extractTextField invalid json: got %q; want empty", got)
	}
}
