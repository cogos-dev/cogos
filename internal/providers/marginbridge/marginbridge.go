// Package marginbridge provides the Reconcilable provider for the
// margin-bridge watcher: outbound-only GitHub polling that turns a book
// repo's signal inboxes and settlement ledger into kernel-native wake
// events for an attached seat.
//
// This package is the kernel-native replacement for the Python prototype at
// cog://mem/working/2026-07-17-nervous-system/scripts/bridge_github.py. It
// follows internal/providers/site (site.go) as its structural template: a
// real, non-stub Reconcilable that shells out to `gh api`/`gh` and plugs into
// pkg/substrate/reconcile's global registry via a blank import.
//
// Semantics preserved from the prototype (see PR description for the full
// mapping):
//   - Outbound-only polling. No listeners, no ports.
//   - Two afferents: inbox dirs (new-file wake) and watched files
//     (sha-change wake, e.g. the comments ledger settling).
//   - Trust tiers by commit author: operator (== the authenticated gh login)
//     vs untrusted. A signal earns the wake only, never elevated intent.
//   - Echo suppression: a watched-file change authored by the operator's own
//     login does not wake (it is the seat's own settle-push reflecting back).
//   - Persistent cursor (pkg/reconcile.State) so restarts baseline instead of
//     replaying history.
//   - Wakes are emitted through the kernel's native event path (ledger +
//     bus), not an external process-level side channel.
//
// Deliberate deviations from the prototype are documented inline at the
// point where they occur (baseline-forces-state-write, per-key baseline for
// both afferent kinds, self-throttle cache scoping).
package marginbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
	"gopkg.in/yaml.v3"
)

// Type is the resource type identifier registered with pkg/reconcile.
const Type = "margin-bridge"

// defaultPollMinIntervalS is used when the config omits poll_min_interval_s.
const defaultPollMinIntervalS = 40

// ignoredEntryNames mirrors the prototype's directory-listing skip list —
// these are housekeeping files in the watched dirs, never signals.
var ignoredEntryNames = map[string]bool{
	".gitkeep":  true,
	"README.md": true,
}

func init() {
	reconcile.RegisterProvider(Type, NewProvider())
}

// ─── Config ───────────────────────────────────────────────────────────────

// Config is the declared shape of .cog/config/margin-bridge.yaml.
type Config struct {
	// Enabled is true only when the config file was found. LoadConfig
	// returns a zero-value, Enabled=false Config (not an error) when the
	// file is absent, so workspaces without it get Health()==Suspended,
	// matching the graceful-absence pattern used by mlx-inference/discord.
	Enabled bool `yaml:"-"`

	Repo             string   `yaml:"repo"`
	OperatorGHLogin  string   `yaml:"operator_gh_login"`
	WatchDirs        []string `yaml:"watch_dirs"`
	WatchFiles       []string `yaml:"watch_files"`
	PollMinIntervalS int      `yaml:"poll_min_interval_s"`
}

func (c *Config) pollInterval() time.Duration {
	s := c.PollMinIntervalS
	if s <= 0 {
		s = defaultPollMinIntervalS
	}
	return time.Duration(s) * time.Second
}

// configPath returns the fixed per-workspace config location.
func configPath(root string) string {
	return filepath.Join(root, ".cog", "config", "margin-bridge.yaml")
}

// ─── Live snapshot types ────────────────────────────────────────────────────

// liveEntry is one file within a watched inbox directory.
type liveEntry struct {
	Path string `json:"path"`
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

// liveSnapshot is the FetchLive return value: everything observed on GitHub
// this cycle (or replayed from the self-throttle cache).
type liveSnapshot struct {
	// InboxEntries is keyed by watched dir path.
	InboxEntries map[string][]liveEntry `json:"inbox_entries"`
	// WatchFiles maps watched file path -> current content sha.
	// A path absent from this map means the file could not be fetched this
	// cycle (e.g. 404 or transient gh error) and is treated as "no change".
	WatchFiles map[string]string `json:"watch_files"`
	FetchedAt  time.Time         `json:"fetched_at"`
}

func newLiveSnapshot() *liveSnapshot {
	return &liveSnapshot{
		InboxEntries: make(map[string][]liveEntry),
		WatchFiles:   make(map[string]string),
	}
}

// totalInboxEntries counts all currently-listed inbox entries across every
// watched dir — this is the "unclaimed signals" count Health() reports.
func (s *liveSnapshot) totalInboxEntries() int {
	if s == nil {
		return 0
	}
	n := 0
	for _, entries := range s.InboxEntries {
		n += len(entries)
	}
	return n
}

// ─── EventSink ──────────────────────────────────────────────────────────────

// EventSink is the injection seam for kernel-native event delivery. Defined
// here (not imported from internal/engine) so this leaf package stays
// decoupled from the kernel core per ADR-085 — the same shape as
// internal/providers/vllm's BusEmitter. Production wiring is installed by
// internal/providers/all.Register via Provider.SetEventSink, backed by
// *engine.Process.EmitEvent (ledger) and *engine.BusSessionManager.AppendEvent
// + *engine.BusEventBroker.Publish (SSE bus). Tests inject a mock.
type EventSink interface {
	// EmitLedgerEvent appends a single kernel ledger event (hash-chained,
	// visible to cog_read_events/cog_tail_events).
	EmitLedgerEvent(eventType string, data map[string]interface{}) error

	// EmitBusEvent appends to a named bus (SSE-visible to attached
	// dashboard/Mod3-class consumers) and publishes it to live subscribers.
	EmitBusEvent(busID, eventType, from string, payload map[string]interface{}) error
}

// BusMarginBridge is the bus id margin-bridge signals are appended to, for
// the SSE-attached class of consumer (dashboard, Mod3).
const BusMarginBridge = "bus_margin_bridge"

// EventMarginSignal is the ledger/bus event type emitted for a real
// (non-baseline, non-echo-suppressed) wake.
const EventMarginSignal = "margin.signal"

// ─── Provider ───────────────────────────────────────────────────────────────

// Provider implements reconcile.Reconcilable for margin-bridge.
type Provider struct {
	mu sync.Mutex

	root   string
	config *Config

	sink EventSink

	// gh is the injection seam for `gh api` calls. Defaults to the real
	// exec-backed client (execGHClient); tests substitute a mock via
	// setGHClient so ComputePlan-adjacent afferent/tier/echo-suppression
	// logic in ApplyPlan is exercised without shelling out or touching the
	// network.
	gh ghClient

	// selfLogin caches the resolved `gh api user` login so repeated
	// ApplyPlan cycles don't re-resolve it. Empty until first resolved (or
	// permanently set from config.OperatorGHLogin).
	selfLogin       string
	selfLoginResolv bool

	// lastSnapshot/lastFetchTime back Health()'s cheap, in-memory,
	// zero-I/O status report — updated at the end of every FetchLive call.
	lastSnapshot  *liveSnapshot
	lastFetchTime time.Time
}

// NewProvider constructs an unwired Provider. Call SetEventSink once the
// kernel-side handles (Process/BusSessionManager/BusEventBroker) exist.
func NewProvider() *Provider {
	return &Provider{gh: execGHClient{}}
}

// setGHClient overrides the gh api injection seam. Test-only (unexported);
// production always uses the exec-backed execGHClient installed by
// NewProvider.
func (p *Provider) setGHClient(c ghClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gh = c
}

func (p *Provider) ghClient() ghClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gh == nil {
		return execGHClient{}
	}
	return p.gh
}

// SetEventSink wires the production (or test-mock) event delivery seam.
// Nil-safe: ApplyPlan degrades to log-only (no wake) when no sink is set,
// rather than failing the reconcile cycle.
func (p *Provider) SetEventSink(sink EventSink) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sink = sink
}

func (p *Provider) eventSink() EventSink {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sink
}

// Type returns the resource type identifier.
func (p *Provider) Type() string { return Type }

// ─── LoadConfig ─────────────────────────────────────────────────────────────

// LoadConfig reads .cog/config/margin-bridge.yaml. A missing file is not an
// error: it returns a disabled zero-value Config so Health() reports
// Suspended (opt-in feature, graceful absence) matching mlx-inference/discord.
func (p *Provider) LoadConfig(root string) (any, error) {
	p.mu.Lock()
	p.root = root
	p.mu.Unlock()

	data, err := os.ReadFile(configPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			cfg := &Config{Enabled: false}
			p.mu.Lock()
			p.config = cfg
			p.mu.Unlock()
			return cfg, nil
		}
		return nil, fmt.Errorf("margin-bridge: read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("margin-bridge: parse config: %w", err)
	}
	if cfg.Repo == "" {
		return nil, fmt.Errorf("margin-bridge: config: repo is required")
	}
	cfg.Enabled = true

	p.mu.Lock()
	p.config = &cfg
	if cfg.OperatorGHLogin != "" {
		p.selfLogin = cfg.OperatorGHLogin
		p.selfLoginResolv = true
	}
	p.mu.Unlock()

	return &cfg, nil
}

// ─── Health ──────────────────────────────────────────────────────────────

// Health reports the cached last-cycle status. Zero I/O, safe for the
// buildHealthBlock proprioception probe's 200ms budget: it reads fields
// populated by the most recent FetchLive/LoadConfig calls only.
func (p *Provider) Health() reconcile.ResourceStatus {
	p.mu.Lock()
	cfg := p.config
	snap := p.lastSnapshot
	fetchedAt := p.lastFetchTime
	p.mu.Unlock()

	if cfg == nil || !cfg.Enabled {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthSuspended,
			Operation: reconcile.OperationIdle,
			Message:   "margin-bridge: no .cog/config/margin-bridge.yaml — opt-in feature",
		}
	}

	if snap == nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthProgressing,
			Operation: reconcile.OperationIdle,
			Message:   "margin-bridge: awaiting first poll",
		}
	}

	since := time.Since(fetchedAt).Truncate(time.Second)
	n := snap.totalInboxEntries()
	if n == 0 {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusSynced,
			Health:    reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("no unclaimed signals, last poll %s ago", since),
		}
	}
	noun := "signal"
	if n != 1 {
		noun = "signals"
	}
	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusOutOfSync,
		Health:    reconcile.HealthDegraded,
		Operation: reconcile.OperationIdle,
		Message:   fmt.Sprintf("%d unclaimed %s, last poll %s ago", n, noun, since),
	}
}

// ─── gh CLI client ──────────────────────────────────────────────────────────

// ghClient is the injection seam for `gh api` calls. execGHClient is the
// production implementation (shells to the real gh CLI); tests substitute a
// mock via Provider.setGHClient so the afferent/tier/echo-suppression logic
// in ApplyPlan is exercised without touching the network.
type ghClient interface {
	api(ctx context.Context, path string) ([]byte, error)
}

// execGHClient shells to `gh api <path>` and returns the raw JSON stdout.
// Mirrors internal/providers/site's private ghAPI helper
// (site.go:442-451) — per ADR-085 leaf-package discipline, duplicated here
// rather than lifted into a shared package for one 10-line helper.
type execGHClient struct{}

func (execGHClient) api(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", path)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("gh api %s: %w: %s", path, err, stderr)
	}
	return out, nil
}

// fetchContentSHA fetches repos/{repo}/contents/{path} and returns its sha.
func fetchContentSHA(ctx context.Context, gh ghClient, repo, path string) (string, error) {
	data, err := gh.api(ctx, fmt.Sprintf("repos/%s/contents/%s", repo, path))
	if err != nil {
		return "", err
	}
	var resp struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("margin-bridge: parse contents response for %s: %w", path, err)
	}
	return resp.SHA, nil
}

// fetchContentText decodes the base64 `content` field of a contents API
// response into a string. Used for building the human-readable wake line.
func fetchContentText(ctx context.Context, gh ghClient, repo, path string) (string, error) {
	data, err := gh.api(ctx, fmt.Sprintf("repos/%s/contents/%s", repo, path))
	if err != nil {
		return "", err
	}
	var resp struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("margin-bridge: parse contents response for %s: %w", path, err)
	}
	if resp.Encoding != "base64" {
		return "", fmt.Errorf("margin-bridge: unexpected content encoding %q for %s", resp.Encoding, path)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(resp.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("margin-bridge: decode content for %s: %w", path, err)
	}
	return string(decoded), nil
}

// commitAuthor is the (login, verified) pair for the latest commit touching
// a path, matching the prototype's _author() helper. Returns ("unknown",
// false) on any error — a signal whose provenance can't be resolved still
// gets the wake, just tagged untrusted/unverified (never silently dropped).
func commitAuthor(ctx context.Context, gh ghClient, repo, path string) (login string, verified bool) {
	data, err := gh.api(ctx, fmt.Sprintf("repos/%s/commits?path=%s&per_page=1", repo, path))
	if err != nil {
		return "unknown", false
	}
	var commits []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Commit struct {
			Author struct {
				Name string `json:"name"`
			} `json:"author"`
			Verification struct {
				Verified bool `json:"verified"`
			} `json:"verification"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(data, &commits); err != nil || len(commits) == 0 {
		return "unknown", false
	}
	c := commits[0]
	if c.Author.Login != "" {
		return c.Author.Login, c.Commit.Verification.Verified
	}
	if c.Commit.Author.Name != "" {
		return c.Commit.Author.Name, c.Commit.Verification.Verified
	}
	return "unknown", false
}

// resolveSelfLogin returns the operator's gh login, resolving and caching it
// via `gh api user` on first use if config.OperatorGHLogin was left empty.
func (p *Provider) resolveSelfLogin(ctx context.Context) string {
	p.mu.Lock()
	if p.selfLoginResolv {
		login := p.selfLogin
		p.mu.Unlock()
		return login
	}
	gh := p.gh
	p.mu.Unlock()

	login := ""
	if data, err := gh.api(ctx, "user"); err == nil {
		var resp struct {
			Login string `json:"login"`
		}
		if json.Unmarshal(data, &resp) == nil {
			login = resp.Login
		}
	}

	p.mu.Lock()
	p.selfLogin = login
	p.selfLoginResolv = true
	p.mu.Unlock()
	return login
}
