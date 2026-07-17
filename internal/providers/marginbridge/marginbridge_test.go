package marginbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
}

func newMockGH() *mockGH {
	return &mockGH{responses: make(map[string][]byte)}
}

func (m *mockGH) api(_ context.Context, path string) ([]byte, error) {
	m.calls = append(m.calls, path)
	if data, ok := m.responses[path]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("gh api %s: exit status 1: HTTP 404: Not Found", path)
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
// commitAuthor.
func (m *mockGH) setCommitAuthor(repo, path, login string, verified bool) {
	m.setJSON(fmt.Sprintf("repos/%s/commits?path=%s&per_page=1", repo, path), []map[string]any{
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
