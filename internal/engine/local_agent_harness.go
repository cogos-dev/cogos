package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultHarnessScopeName is the scope used when no scope is requested.
// Callers that don't specify a scope get exactly the tool set they always have.
const defaultHarnessScopeName = "consolidation"

// harnessToolScopes is the named-scope catalog for harness dispatches.
// Each scope is a named set of tool names the harness may use.
//
// "consolidation" — substrate-only tools: memory, observability, identity.
//   This is the default scope (unchanged from the original tool set).
//   The harness operates on cogdocs, the field, and coherence only.
//
// "audit" — consolidation tools PLUS read-only filesystem access.
//   Use this scope when the harness needs to inspect source, configs,
//   or workspace files without mutating anything.
//
// Future scopes (add entries here when the underlying mechanisms land):
//   "maintenance" — read+write filesystem tools gated behind per-dispatch
//                   worktree isolation (see ADR-081 §8 worktree work).
//   "introspection" — audit tools plus kernel state-dump tools for deep
//                     diagnostic cycles.
var harnessToolScopes = map[string][]string{
	"consolidation": {
		"cog_resolve_uri",
		"cog_search_memory",
		"cog_read_cogdoc",
		"cog_query_field",
		"cog_check_coherence",
		"cog_get_state",
		"cog_get_trust",
		"cog_get_nucleus",
		"cog_get_index",
		"cog_assemble_context",
		"cog_emit_event",
	},
	"audit": {
		"cog_resolve_uri",
		"cog_search_memory",
		"cog_read_cogdoc",
		"cog_query_field",
		"cog_check_coherence",
		"cog_get_state",
		"cog_get_trust",
		"cog_get_nucleus",
		"cog_get_index",
		"cog_assemble_context",
		"cog_emit_event",
		"cog_read_file",
		"cog_grep_files",
	},
}

const (
	localHarnessHistoryLimit   = 24
	localHarnessCycleTimeout   = 5 * time.Minute
	localHarnessAssessMaxToks  = 256
	localHarnessExecuteMaxToks = 1024
)

const localHarnessAssessPrompt = `You are the resident local CogOS maintenance agent.
Operate only through local inference and the kernel's local tools.
Decide whether a maintenance pass is warranted right now.
Return only one compact JSON object with these keys:
{"action":"sleep|observe|consolidate|repair|propose|escalate","reason":"short string","urgency":0..1,"target":"short string","task":"short concrete next step"}
Prefer "sleep" unless the observation names a concrete task worth doing now.`

const localHarnessExecutePrompt = `You are the resident local CogOS maintenance agent.
Stay local-only. Use the provided kernel tools when they materially improve the answer.
Prefer inspection and diagnosis over mutation. Finish with a concise plain-text result.

CogDoc URIs use the bare form cog:<type>/<path>. Valid types:
  mem, adr, role, skill, agent, spec, status, ledger, crystal,
  kernel, canonical, config, ontology, work, handoff, artifact, docs, hooks
Examples:
  cog:adr/077                    (ADRs resolve by numeric prefix)
  cog:mem/semantic/foo/bar       (memory under .cog/mem/<sector>/...)
  cog:spec/rfc-022-foo           (specs under .cog/specs/)
Cross-workspace refs use authority form: cog://other-workspace/mem/...
Invalid: cog://adrs/..., cog://docs/... with raw fs paths.
If cog_search_memory returns a bus event path (".cog/.state/buses/.../events.jsonl#N"),
that is a chat log entry, not a readable CogDoc — do not try to read it.`

const localHarnessDispatchPrompt = `You are the resident local CogOS harness.
Stay local-only. Use only the provided kernel tools. Be concise and finish with a direct answer.

CogDoc URIs use the bare form cog:<type>/<path>. Valid types:
  mem, adr, role, skill, agent, spec, status, ledger, crystal,
  kernel, canonical, config, ontology, work, handoff, artifact, docs, hooks
Example: cog:adr/077 (ADRs resolve by numeric prefix).
Cross-workspace refs use authority form: cog://other-workspace/mem/...
If cog_search_memory returns ".cog/.state/buses/.../events.jsonl#N", that's a chat log entry,
not a readable CogDoc — do not try to read it.`

type localHarnessAssessment struct {
	Action  string  `json:"action"`
	Reason  string  `json:"reason"`
	Urgency float64 `json:"urgency"`
	Target  string  `json:"target"`
	Task    string  `json:"task"`
}

type localHarnessCycleRecord struct {
	Cycle       int64
	Timestamp   time.Time
	Duration    time.Duration
	Action      string
	Urgency     float64
	Reason      string
	Target      string
	Observation string
	Result      string
	Model       string
}

type localHarnessCycleOutcome struct {
	record   localHarnessCycleRecord
	timedOut bool
}

type LocalHarnessController struct {
	cfg             *Config
	nucleus         *Nucleus
	process         *Process
	toolRegistry    *KernelToolRegistry
	dispatchTools   *KernelToolRegistry
	backgroundTools *KernelToolRegistry

	agentID  string
	started  time.Time
	interval time.Duration

	// localProviderTimeout is the HTTP timeout (seconds) applied to providers
	// constructed by buildLocalProvider for this controller's dispatches.
	// Resolved once at construction time from providers(.local).yaml; 0 means
	// "fall back to localProviderDefaultTimeoutSec".
	localProviderTimeout int

	// autonomicCfg holds escalation-predicate tunables. Safe to read from
	// multiple goroutines — written once before Start().
	autonomicCfg AutonomicConfig

	// busSessions is optional; when set, each tick emits a
	// KernelHealthSnapshot to bus_kernel_proprio. Nil is a safe no-op.
	busSessions *BusSessionManager

	runCtx context.Context

	running atomic.Bool
	stopped atomic.Bool

	cycleSeq   atomic.Int64
	triggerSeq atomic.Int64

	// triggerPending is set to true by TriggerAgent and cleared at the start
	// of each tick's escalation check. Used so an explicit trigger always
	// fires the LLM path even when providers are green.
	triggerPendingMu sync.Mutex
	triggerPending   bool

	startOnce sync.Once

	mu              sync.RWMutex
	lastObservation string
	lastModel       string
	lastCycle       *localHarnessCycleRecord
	lastLLMCycle    time.Time // wall-clock time of the last LLM assess call
	// lastEscalatedFingerprint is the snapshotFingerprint of the snapshot
	// that last triggered an escalateDegradedHealth cycle. Used to suppress
	// repeat escalations on stable degradation: the LLM has already seen
	// this exact picture; only the idle re-checkin window should fire next.
	// Cleared whenever the snapshot returns to AllGreen.
	lastEscalatedFingerprint string
	history                  []localHarnessCycleRecord

	// ollamaMu serializes all Ollama inference calls across both the metabolic
	// cycle (runCycle) and user-initiated dispatches (DispatchToHarness).
	// Ollama defaults to OLLAMA_NUM_PARALLEL=1; concurrent requests from the
	// same controller cause duplicate model copies in VRAM and queued waits
	// that look like hangs. One lock per controller, held for the duration of
	// the inference call, makes the serialization explicit and debuggable.
	ollamaMu sync.Mutex
}

func NewLocalHarnessController(cfg *Config, nucleus *Nucleus, process *Process, mcpSrv *MCPServer) (*LocalHarnessController, error) {
	return NewLocalHarnessControllerWithScope(cfg, nucleus, process, mcpSrv, "")
}

// NewLocalHarnessControllerWithScope creates a LocalHarnessController using
// the named harness scope. An empty scopeName selects defaultHarnessScopeName.
// Unknown scope names return an error immediately.
func NewLocalHarnessControllerWithScope(cfg *Config, nucleus *Nucleus, process *Process, mcpSrv *MCPServer, scopeName string) (*LocalHarnessController, error) {
	if mcpSrv == nil {
		return nil, fmt.Errorf("local harness requires MCP server wiring")
	}
	if scopeName == "" {
		scopeName = defaultHarnessScopeName
	}
	toolNames, ok := harnessToolScopes[scopeName]
	if !ok {
		return nil, fmt.Errorf("unknown harness scope %q (known: consolidation, audit)", scopeName)
	}
	registry := NewKernelToolRegistry(mcpSrv)
	dispatchTools, err := registry.Scoped(toolNames)
	if err != nil {
		return nil, err
	}

	interval := time.Minute
	if cfg != nil && cfg.HeartbeatInterval > 0 {
		interval = time.Duration(cfg.HeartbeatInterval) * time.Second
	}

	return &LocalHarnessController{
		cfg:                  cfg,
		nucleus:              nucleus,
		process:              process,
		toolRegistry:         registry,
		dispatchTools:        dispatchTools,
		backgroundTools:      dispatchTools,
		agentID:              DefaultAgentID,
		started:              time.Now().UTC(),
		interval:             interval,
		localProviderTimeout: resolveLocalProviderTimeout(cfg),
	}, nil
}

// SetBusSessionManager wires in the kernel's bus layer so that each autonomic
// tick emits a KernelHealthSnapshot to bus_kernel_proprio. Optional: nil is
// a safe no-op (snapshots are computed but not persisted).
func (c *LocalHarnessController) SetBusSessionManager(mgr *BusSessionManager) {
	c.busSessions = mgr
}

func (c *LocalHarnessController) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		c.runCtx = ctx
		c.stopped.Store(false)
		c.tryStartCycle("startup", 0, nil)
		go c.runTicker(ctx)
	})
}

// runTicker is the autonomic control loop. Each tick:
//
//  1. Probes all Reconcilables (deterministic, near-zero cost).
//  2. Emits a KernelHealthSnapshot to bus_kernel_proprio.
//  3. Evaluates the escalation predicate.
//  4. Only calls tryStartCycle (→ LLM assess+execute) when the predicate fires.
//
// When the registry is empty or all providers are green and no explicit trigger
// is pending and the idle re-checkin window hasn't elapsed, the tick completes
// with zero LLM calls.
func (c *LocalHarnessController) runTicker(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.stopped.Store(true)
			return
		case <-ticker.C:
			c.autonomicTick(ctx)
		}
	}
}

// autonomicTick is the per-tick unit of the autonomic control loop.
// It probes providers, emits a snapshot, and conditionally escalates.
func (c *LocalHarnessController) autonomicTick(ctx context.Context) {
	// 1. Probe all Reconcilables — cheap, deterministic.
	snap := buildKernelHealthSnapshot(ctx)

	// 2. Emit snapshot to bus regardless of health state.
	emitHealthSnapshot(ctx, c.busSessions, snap)

	// 2a. Run deterministic self-heal for any degraded provider that supports
	// the full plan/apply Reconcilable contract (e.g. MLXSupervisedProvider).
	// This runs BEFORE the escalation predicate so that transient crashes are
	// repaired autonomically without waking the LLM. If self-heal succeeds,
	// the next snapshot will be green and no escalation fires.
	if !snap.AllGreen() {
		healDegradedProviders(ctx)
	}

	// 3. Consume the triggerPending flag atomically.
	c.triggerPendingMu.Lock()
	triggerPending := c.triggerPending
	c.triggerPending = false
	c.triggerPendingMu.Unlock()

	// 4. Evaluate escalation predicate.
	c.mu.RLock()
	lastLLM := c.lastLLMCycle
	lastFP := c.lastEscalatedFingerprint
	c.mu.RUnlock()

	// When the snapshot returns to all-green, clear the dedupe fingerprint
	// so the next degradation (even if it's the same shape as before) is
	// treated as a new event the LLM should see.
	if snap.AllGreen() && lastFP != "" {
		c.mu.Lock()
		c.lastEscalatedFingerprint = ""
		c.mu.Unlock()
	}

	reason := shouldEscalate(snap, triggerPending, lastLLM, c.autonomicCfg)

	// Stable-degradation suppression. If the same provider population is in
	// the same non-green buckets as the snapshot that last triggered an LLM
	// cycle, don't keep waking the LLM every minute — the agent has already
	// seen this picture. The 1h idle re-checkin remains the safety valve, so
	// the agent does check back on the same problem periodically. Explicit
	// triggers (TriggerAgent) and out-of-sync (real declared-vs-live drift)
	// bypass dedupe — those are signals the operator wants surfaced.
	if reason == escalateDegradedHealth && lastFP != "" {
		fp := snapshotFingerprint(snap)
		if fp == lastFP {
			window := c.autonomicCfg.idleRecheckIn()
			if !triggerPending && !lastLLM.IsZero() && time.Since(lastLLM) < window {
				slog.Debug("autonomic: stable degradation, suppressing escalation",
					"providers", len(snap.Providers),
					"degraded", snap.Counts.Degraded,
					"missing", snap.Counts.Missing,
					"suspended", snap.Counts.Suspended,
					"fingerprint", fp[:12],
				)
				return
			}
			// Window has elapsed — fall through to escalation. Reframe the
			// reason so the log reflects what's actually happening: the LLM
			// is checking back on a stable problem, not seeing it for the
			// first time.
			reason = escalateIdleRecheckIn
		}
	}

	if reason == "" {
		// All green, no trigger, idle window not elapsed — pure deterministic tick.
		slog.Debug("autonomic: tick complete, no escalation",
			"providers", len(snap.Providers),
			"healthy", snap.Counts.Healthy,
		)
		return
	}

	slog.Info("autonomic: escalating to LLM cycle",
		"reason", string(reason),
		"providers", len(snap.Providers),
		"degraded", snap.Counts.Degraded,
		"missing", snap.Counts.Missing,
	)
	c.tryStartCycle(string(reason), 0, nil)

	// Record the fingerprint so the next tick can suppress a repeat of the
	// same degradation. Only update on degraded_health escalations: idle
	// recheckins, explicit triggers, and out-of-sync don't represent "the
	// LLM has seen this degradation."
	if reason == escalateDegradedHealth {
		fp := snapshotFingerprint(snap)
		if fp != "" {
			c.mu.Lock()
			c.lastEscalatedFingerprint = fp
			c.mu.Unlock()
		}
	}
}

func (c *LocalHarnessController) tryStartCycle(reason string, triggerSeq int64, waiter chan<- localHarnessCycleOutcome) bool {
	if c.stopped.Load() {
		return false
	}
	if !c.running.CompareAndSwap(false, true) {
		return false
	}
	parent := c.runCtx
	if parent == nil {
		parent = context.Background()
	}
	go c.runCycle(parent, reason, triggerSeq, waiter)
	return true
}

func (c *LocalHarnessController) runCycle(parent context.Context, reason string, triggerSeq int64, waiter chan<- localHarnessCycleOutcome) {
	defer c.running.Store(false)

	ctx, cancel := context.WithTimeout(parent, localHarnessCycleTimeout)
	defer cancel()

	start := time.Now().UTC()
	record := localHarnessCycleRecord{
		Cycle:     c.cycleSeq.Add(1),
		Timestamp: start,
		Action:    "sleep",
		Reason:    "idle",
	}
	record.Observation = c.buildObservation(reason)

	outcome := localHarnessCycleOutcome{record: record}

	target, err := detectLocalLLMTarget(ctx, "")
	if err != nil {
		outcome.record.Action = "error"
		outcome.record.Reason = err.Error()
		c.finishCycle(outcome.record)
		if waiter != nil {
			waiter <- outcome
		}
		return
	}

	model, _, note := resolveDispatchLocalModel(target.Models, c.localModelHint(), DispatchModelE4B)
	if model == "" {
		outcome.record.Action = "error"
		outcome.record.Reason = note
		outcome.record.Model = c.localModelHint()
		c.finishCycle(outcome.record)
		if waiter != nil {
			waiter <- outcome
		}
		return
	}
	outcome.record.Model = model

	// ollamaMu serializes this cycle's inference calls against concurrent
	// DispatchToHarness calls. Ollama is single-threaded by default; holding
	// the lock for the full assess+execute block prevents two concurrent
	// /api/chat requests from loading duplicate model copies into VRAM.
	c.ollamaMu.Lock()
	provider := buildLocalProvider(target, model, c.localProviderTimeout)
	assessment, err := c.assessCycle(ctx, provider, outcome.record.Observation)
	if err != nil {
		c.ollamaMu.Unlock()
		outcome.record.Action = "error"
		outcome.record.Reason = err.Error()
		c.finishCycle(outcome.record)
		if waiter != nil {
			waiter <- outcome
		}
		return
	}

	outcome.record.Action = assessment.Action
	outcome.record.Reason = assessment.Reason
	outcome.record.Urgency = clampUrgency(assessment.Urgency)
	outcome.record.Target = assessment.Target
	if note != "" {
		if outcome.record.Reason == "" {
			outcome.record.Reason = note
		} else {
			outcome.record.Reason = outcome.record.Reason + "; " + note
		}
	}

	if assessment.Action != "sleep" {
		result, err := c.executeCycleTask(ctx, provider, assessment, outcome.record.Observation, c.backgroundTools)
		if err != nil {
			outcome.record.Action = "error"
			outcome.record.Reason = err.Error()
		} else {
			outcome.record.Result = result
		}
	}
	c.ollamaMu.Unlock()

	if ctx.Err() == context.DeadlineExceeded {
		outcome.timedOut = true
		if outcome.record.Action == "" || outcome.record.Action == "sleep" {
			outcome.record.Action = "error"
		}
		if outcome.record.Reason == "" {
			outcome.record.Reason = "cycle timeout"
		}
	}

	c.finishCycle(outcome.record)
	if waiter != nil {
		waiter <- outcome
	}
}

func (c *LocalHarnessController) finishCycle(record localHarnessCycleRecord) {
	record.Duration = time.Since(record.Timestamp)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastObservation = record.Observation
	c.lastModel = record.Model
	c.lastCycle = &record
	// Record the wall-clock time of this LLM cycle so the idle re-checkin
	// predicate knows when we last ran. Error-state cycles still count —
	// the LLM was called even if it returned an error.
	if record.Action != "" {
		c.lastLLMCycle = record.Timestamp
	}
	c.history = append(c.history, record)
	if len(c.history) > localHarnessHistoryLimit {
		c.history = append([]localHarnessCycleRecord(nil), c.history[len(c.history)-localHarnessHistoryLimit:]...)
	}
}

func (c *LocalHarnessController) assessCycle(ctx context.Context, provider Provider, observation string) (*localHarnessAssessment, error) {
	temp := 0.0
	resp, err := provider.Complete(ctx, &CompletionRequest{
		SystemPrompt: localHarnessAssessPrompt,
		Messages: []ProviderMessage{
			{Role: "user", Content: observation},
		},
		MaxTokens:   localHarnessAssessMaxToks,
		Temperature: &temp,
		Metadata: RequestMetadata{
			RequestID:   fmt.Sprintf("local-harness-assess-%d", time.Now().UnixNano()),
			PreferLocal: true,
			Source:      "local-harness",
		},
	})
	if err != nil {
		return nil, err
	}

	var assessment localHarnessAssessment
	if err := decodeJSONPayload(resp.Content, &assessment); err != nil {
		return nil, fmt.Errorf("parse assessment: %w", err)
	}
	if strings.TrimSpace(assessment.Action) == "" {
		assessment.Action = "sleep"
	}
	assessment.Action = strings.ToLower(strings.TrimSpace(assessment.Action))
	assessment.Reason = strings.TrimSpace(assessment.Reason)
	assessment.Target = strings.TrimSpace(assessment.Target)
	assessment.Task = strings.TrimSpace(assessment.Task)
	assessment.Urgency = clampUrgency(assessment.Urgency)
	return &assessment, nil
}

func (c *LocalHarnessController) executeCycleTask(ctx context.Context, provider Provider, assessment *localHarnessAssessment, observation string, registry *KernelToolRegistry) (string, error) {
	temp := 0.1
	task := c.buildExecutionTask(assessment, observation)
	req := &CompletionRequest{
		SystemPrompt: localHarnessExecutePrompt,
		Messages: []ProviderMessage{
			{Role: "user", Content: task},
		},
		Tools:       registry.Definitions(),
		ToolChoice:  "auto",
		MaxTokens:   localHarnessExecuteMaxToks,
		Temperature: &temp,
		Metadata: RequestMetadata{
			RequestID:   fmt.Sprintf("local-harness-exec-%d", time.Now().UnixNano()),
			PreferLocal: true,
			Source:      "local-harness",
		},
	}
	resp, clientCalls, transcript, err := c.completeWithToolLoop(ctx, provider, req, registry)
	if err != nil {
		return "", err
	}
	if len(clientCalls) > 0 {
		slog.Warn("local harness produced unsupported client tool calls", "count", len(clientCalls))
	}
	content := strings.TrimSpace(resp.Content)
	if content == "" && len(transcript) > 0 {
		content = summarizeToolTranscript(transcript)
	}
	return content, nil
}

func (c *LocalHarnessController) buildExecutionTask(assessment *localHarnessAssessment, observation string) string {
	var b strings.Builder
	b.WriteString("Observation:\n")
	b.WriteString(observation)
	b.WriteString("\n\nRequested action: ")
	b.WriteString(assessment.Action)
	if assessment.Target != "" {
		b.WriteString("\nTarget: ")
		b.WriteString(assessment.Target)
	}
	if assessment.Reason != "" {
		b.WriteString("\nWhy: ")
		b.WriteString(assessment.Reason)
	}
	if assessment.Task != "" {
		b.WriteString("\nNext step: ")
		b.WriteString(assessment.Task)
	}
	return b.String()
}

func (c *LocalHarnessController) buildObservation(triggerReason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "time=%s\n", time.Now().UTC().Format(time.RFC3339))
	if triggerReason != "" {
		fmt.Fprintf(&b, "trigger=%s\n", triggerReason)
	}
	if c.cfg != nil {
		fmt.Fprintf(&b, "workspace=%s\n", filepath.Base(c.cfg.WorkspaceRoot))
	}
	if c.nucleus != nil && c.nucleus.Name != "" {
		fmt.Fprintf(&b, "identity=%s\n", c.nucleus.Name)
	}
	if c.process != nil {
		fmt.Fprintf(&b, "process_state=%s\n", c.process.State().String())
		fovea := c.process.Field().Fovea(5)
		if len(fovea) > 0 {
			b.WriteString("field_top:\n")
			for _, item := range fovea {
				fmt.Fprintf(&b, "- %s score=%.3f\n", item.Path, item.Score)
			}
		}
	}

	c.mu.RLock()
	last := c.lastCycle
	c.mu.RUnlock()
	if last != nil {
		fmt.Fprintf(&b, "last_cycle=%s action=%s urgency=%.2f reason=%s\n",
			last.Timestamp.Format(time.RFC3339), last.Action, last.Urgency, last.Reason)
	}
	return b.String()
}

func (c *LocalHarnessController) localModelHint() string {
	if c.cfg != nil && strings.TrimSpace(c.cfg.LocalModel) != "" {
		return strings.TrimSpace(c.cfg.LocalModel)
	}
	return defaultOllamaModel
}

// resolveProviderByProcessState consults process_state_routing in the merged
// providers config and returns the configured provider name for the current
// process state. Returns ("", false) when no process is attached, no state
// routing applies, or the config cannot be loaded.
//
// This implements Path 2 in DispatchToHarness: harness dispatches without an
// explicit req.Provider consult the same routing table as the SimpleRouter,
// so the autonomic loop honours the operator's per-state provider preferences
// (e.g., receptive -> mlx-lm, active -> claude-code).
func (c *LocalHarnessController) resolveProviderByProcessState() (string, bool) {
	if c.process == nil {
		return "", false
	}
	state := c.process.State().String()
	// "unknown" is the default-case sentinel from ProcessState.String(); treat
	// it the same as empty string so an unrecognised state falls back to the
	// legacy local-LLM path rather than routing to process_state_routing["unknown"].
	if state == "" || state == "unknown" {
		return "", false
	}
	pcfg, err := loadProvidersConfig(c.cfg)
	if err != nil {
		return "", false
	}
	name, ok := pcfg.Routing.ProcessStateRouting[state]
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// isOllamaProvider reports whether a named ProviderConfig resolves to the
// Ollama backend. It mirrors makeProvider's type inference: when pc.Type is
// empty the provider name itself is the effective type (see router.go
// makeProvider). This means a provider named "ollama" without an explicit
// type: field is correctly identified as Ollama — required so ollamaMu is
// acquired even when the config omits the redundant type: ollama annotation.
func isOllamaProvider(name string, pc ProviderConfig) bool {
	t := pc.Type
	if t == "" {
		t = name
	}
	return t == "ollama"
}

func (c *LocalHarnessController) summary() AgentSummary {
	c.mu.RLock()
	defer c.mu.RUnlock()

	s := AgentSummary{
		AgentID:   c.agentID,
		Alive:     !c.stopped.Load(),
		Running:   c.running.Load(),
		UptimeSec: int64(time.Since(c.started).Seconds()),
		Model:     c.lastModel,
		Interval:  c.interval.String(),
	}
	if c.nucleus != nil {
		s.Identity = c.nucleus.Name
	}
	if c.lastCycle != nil {
		s.CycleCount = c.lastCycle.Cycle
		s.LastAction = c.lastCycle.Action
		s.LastCycle = c.lastCycle.Timestamp.Format(time.RFC3339)
		s.LastUrgency = c.lastCycle.Urgency
		s.LastReason = c.lastCycle.Reason
		s.LastDurMs = c.lastCycle.Duration.Milliseconds()
	}
	if s.Model == "" {
		s.Model = c.localModelHint()
	}
	return s
}

func (c *LocalHarnessController) ListAgents(_ context.Context, _ bool) ([]AgentSummary, error) {
	if c.stopped.Load() {
		return nil, ErrAgentUnavailable
	}
	return []AgentSummary{c.summary()}, nil
}

func (c *LocalHarnessController) GetAgent(_ context.Context, id string, includeTrace bool, traceLimit int) (*AgentSnapshot, error) {
	if id != c.agentID {
		return nil, ErrAgentNotFound
	}
	if c.stopped.Load() {
		return nil, ErrAgentUnavailable
	}

	snap := &AgentSnapshot{
		Summary: c.summary(),
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	snap.LastObservation = c.lastObservation
	if c.nucleus != nil {
		snap.IdentityRef = c.nucleus.Name
	}
	snap.Memory = make([]AgentMemoryEntry, 0, len(c.history))
	for i := len(c.history) - 1; i >= 0; i-- {
		rec := c.history[i]
		snap.Memory = append(snap.Memory, AgentMemoryEntry{
			Cycle:    rec.Cycle,
			Action:   rec.Action,
			Urgency:  rec.Urgency,
			Sentence: summarizeMemoryEntry(rec),
			Ago:      sinceString(rec.Timestamp),
		})
	}
	if includeTrace {
		start := 0
		if traceLimit > 0 && len(c.history) > traceLimit {
			start = len(c.history) - traceLimit
		}
		for _, rec := range c.history[start:] {
			snap.Traces = append(snap.Traces, AgentCycleTrace{
				Cycle:       rec.Cycle,
				Timestamp:   rec.Timestamp.Format(time.RFC3339),
				DurationMs:  rec.Duration.Milliseconds(),
				Action:      rec.Action,
				Urgency:     rec.Urgency,
				Reason:      rec.Reason,
				Target:      rec.Target,
				Observation: rec.Observation,
				Result:      rec.Result,
			})
		}
	}
	return snap, nil
}

func (c *LocalHarnessController) TriggerAgent(ctx context.Context, id string, reason string, wait bool) (*AgentTriggerResult, error) {
	if id != c.agentID {
		return nil, ErrAgentNotFound
	}
	if c.stopped.Load() {
		return nil, ErrAgentUnavailable
	}

	seq := c.triggerSeq.Add(1)
	if !wait {
		if !c.tryStartCycle(reason, seq, nil) {
			// Cycle is already running; set triggerPending so the next
			// autonomic tick picks this up even if providers are green.
			c.triggerPendingMu.Lock()
			c.triggerPending = true
			c.triggerPendingMu.Unlock()
			return &AgentTriggerResult{
				Triggered:  false,
				AgentID:    id,
				TriggerSeq: seq,
				Message:    "already_running",
			}, nil
		}
		return &AgentTriggerResult{
			Triggered:  true,
			AgentID:    id,
			TriggerSeq: seq,
			Message:    "triggered",
		}, nil
	}

	waiter := make(chan localHarnessCycleOutcome, 1)
	if !c.tryStartCycle(reason, seq, waiter) {
		// Cycle is already running; set triggerPending so the next
		// autonomic tick picks this up.
		c.triggerPendingMu.Lock()
		c.triggerPending = true
		c.triggerPendingMu.Unlock()
		return &AgentTriggerResult{
			Triggered:  false,
			AgentID:    id,
			TriggerSeq: seq,
			Message:    "already_running",
		}, nil
	}

	select {
	case outcome := <-waiter:
		return &AgentTriggerResult{
			Triggered:  true,
			AgentID:    id,
			CycleID:    fmt.Sprintf("%s-%d", c.agentID, outcome.record.Cycle),
			TriggerSeq: seq,
			Message:    "completed",
			Action:     outcome.record.Action,
			Urgency:    outcome.record.Urgency,
			Reason:     outcome.record.Reason,
			DurationMs: outcome.record.Duration.Milliseconds(),
			TimedOut:   outcome.timedOut,
		}, nil
	case <-ctx.Done():
		return &AgentTriggerResult{
			Triggered:  true,
			AgentID:    id,
			TriggerSeq: seq,
			Message:    "triggered",
			TimedOut:   true,
		}, nil
	}
}

func (c *LocalHarnessController) DispatchToHarness(ctx context.Context, req DispatchRequest) (*DispatchBatchResult, error) {
	if c.stopped.Load() {
		return nil, ErrAgentUnavailable
	}
	if req.AgentID != "" && req.AgentID != c.agentID {
		return nil, ErrAgentNotFound
	}
	if err := req.Normalize(); err != nil {
		return nil, err
	}

	// Resolve the inference provider. Three paths, evaluated in order:
	//   1. Explicit named provider via req.Provider (RFC-0007 Layer 1):
	//      look the name up in providers.yaml + providers.local.yaml, build
	//      the matching Provider, and use its declared model.
	//   2. Process-state routing (new): when req.Provider is empty and the
	//      controller has an associated process, consult process_state_routing
	//      in providers config. If the current state maps to a configured
	//      provider, dispatch there. This wires the autonomic loop and harness
	//      dispatches through the same routing table as the main router.
	//   3. Legacy Model-enum routing ("e4b" | "26b") via local-LLM probe.
	//      Backward-compatible fallback when no process-state route applies.
	//
	// Unknown provider names error fast — never silently fall through to
	// the legacy path because that would mask a config typo.
	var provider Provider
	var model string
	var routeUsed DispatchModel
	var note string
	var err error
	// needsOllamaMu is true when the selected path goes through Ollama
	// (either legacy Path 3 or an explicit/state-routed provider of type
	// "ollama"). Non-Ollama state-routed dispatches (openai, mlx-lm, …)
	// must NOT acquire the lock — they have no VRAM-sharing risk with the
	// metabolic cycle and would needlessly block behind it.
	needsOllamaMu := false
	if req.Provider != "" {
		pcfg, perr := loadProvidersConfig(c.cfg)
		if perr != nil {
			return nil, &AgentControllerError{
				Code:    "internal_error",
				Message: fmt.Sprintf("failed to load providers config: %v", perr),
			}
		}
		pc, ok := pcfg.Providers[req.Provider]
		if !ok {
			known := make([]string, 0, len(pcfg.Providers))
			for k := range pcfg.Providers {
				known = append(known, k)
			}
			return nil, &AgentControllerError{
				Code:    "invalid_input",
				Message: fmt.Sprintf("provider %q is not configured (known: %v)", req.Provider, known),
			}
		}
		if !pc.IsEnabled() {
			return nil, &AgentControllerError{
				Code:    "invalid_input",
				Message: fmt.Sprintf("provider %q is disabled in config", req.Provider),
			}
		}
		p, merr := makeProvider(req.Provider, pc, nil)
		if merr != nil {
			return nil, &AgentControllerError{
				Code:    "internal_error",
				Message: fmt.Sprintf("failed to construct provider %q: %v", req.Provider, merr),
			}
		}
		provider = p
		model = pc.Model
		needsOllamaMu = isOllamaProvider(req.Provider, pc)
		// routeUsed stays empty; ProviderUsed on each slot is the canonical
		// signal that the named-provider path fired.
	} else {
		// Path 2: process-state routing — try before falling back to legacy.
		// When the controller has a process with a known state, consult
		// process_state_routing in providers config. If the state maps to a
		// valid, enabled provider, dispatch there. Otherwise fall through to
		// the legacy local-LLM probe (Path 3).
		usedStateRoute := false
		if stateProvider, stateOK := c.resolveProviderByProcessState(); stateOK {
			pcfg, perr := loadProvidersConfig(c.cfg)
			if perr == nil {
				if pc, pok := pcfg.Providers[stateProvider]; pok && pc.IsEnabled() {
					p, merr := makeProvider(stateProvider, pc, nil)
					if merr != nil {
						return nil, &AgentControllerError{
							Code:    "internal_error",
							Message: fmt.Sprintf("state-routing: failed to construct provider %q: %v", stateProvider, merr),
						}
					}
					provider = p
					model = pc.Model
					// Populate req.Provider so dispatchSlot records the
					// resolved provider name in ProviderUsed — otherwise it
					// stays empty and the caller can't distinguish this path
					// from the legacy Ollama path.
					req.Provider = stateProvider
					needsOllamaMu = isOllamaProvider(stateProvider, pc)
					note = fmt.Sprintf("state-routing: state=%s -> provider=%s", c.process.State().String(), stateProvider)
					usedStateRoute = true
				}
				// If provider not found or disabled: fall through to legacy path silently.
			}
		}
		if !usedStateRoute {
			// Path 3: legacy model-enum routing via local-LLM probe.
			// Always Ollama — always needs the serialisation lock.
			needsOllamaMu = true
			target, terr := detectLocalLLMTarget(ctx, "")
			if terr != nil {
				return nil, terr
			}
			m, ru, n := resolveDispatchLocalModel(target.Models, c.localModelHint(), req.Model)
			if m == "" {
				return nil, errors.New(n)
			}
			model, routeUsed, note = m, ru, n
			provider = buildLocalProvider(target, model, c.localProviderTimeout)
		}
	}

	// Resolve the named scope. Empty scope means the harness's own default
	// scope (c.dispatchTools, already scoped at construction time). A
	// non-empty scope is resolved from the catalog; unknown names are
	// rejected here so the error is immediate and clear.
	baseRegistry := c.dispatchTools
	if req.Scope != "" {
		scopeTools, ok := harnessToolScopes[req.Scope]
		if !ok {
			known := make([]string, 0, len(harnessToolScopes))
			for k := range harnessToolScopes {
				known = append(known, k)
			}
			return nil, &AgentControllerError{
				Code:    "invalid_input",
				Message: fmt.Sprintf("unknown harness scope %q (known: %v)", req.Scope, known),
			}
		}
		baseRegistry, err = c.toolRegistry.Scoped(scopeTools)
		if err != nil {
			return nil, err
		}
	}
	registry := baseRegistry
	if len(req.Tools) > 0 {
		registry, err = baseRegistry.Scoped(req.Tools)
		if err != nil {
			return nil, err
		}
	}

	// ollamaMu serializes dispatch inference calls against concurrent metabolic
	// cycles. Ollama is single-threaded by default; a dispatch arriving while
	// runCycle is mid-stream would issue a concurrent /api/chat that loads a
	// second model copy into VRAM. The lock is held for the full batch so that
	// req.N slots also serialize against the cycle (they already share the same
	// provider object, so they are already serialized at the Ollama layer, but
	// holding the lock makes the exclusion explicit and testable).
	//
	// Non-Ollama state-routed dispatches (openai, mlx-lm, …) skip the lock:
	// they have no shared VRAM risk with the metabolic cycle and must not
	// block behind Ollama-bound cycles.
	if needsOllamaMu {
		c.ollamaMu.Lock()
		defer c.ollamaMu.Unlock()
	}

	batch := &DispatchBatchResult{
		Results: make([]DispatchResult, req.N),
	}
	if note != "" {
		batch.Notes = append(batch.Notes, note)
	}

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < req.N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			batch.Results[idx] = c.dispatchSlot(ctx, provider, registry, model, routeUsed, req, idx, note)
		}(i)
	}
	wg.Wait()
	batch.TotalDurationSec = time.Since(start).Seconds()
	return batch, nil
}

func (c *LocalHarnessController) dispatchSlot(parent context.Context, provider Provider, registry *KernelToolRegistry, model string, routeUsed DispatchModel, req DispatchRequest, idx int, note string) DispatchResult {
	res := DispatchResult{
		Index:        idx,
		ModelUsed:    routeUsed,
		ProviderUsed: req.Provider,
	}
	slotCtx, cancel := context.WithTimeout(parent, time.Duration(req.TimeoutSeconds)*time.Second)
	defer cancel()

	systemPrompt := strings.TrimSpace(req.SystemPrompt)
	if systemPrompt == "" {
		systemPrompt = localHarnessDispatchPrompt
	}
	counting := &countingProvider{Provider: provider}

	temp := 0.1
	compReq := &CompletionRequest{
		SystemPrompt: systemPrompt,
		Messages: []ProviderMessage{
			{Role: "user", Content: strings.TrimSpace(req.Task)},
		},
		Tools:         registry.Definitions(),
		ToolChoice:    "auto",
		MaxTokens:     localHarnessExecuteMaxToks,
		Temperature:   &temp,
		ModelOverride: model,
		Metadata: RequestMetadata{
			RequestID:      fmt.Sprintf("local-harness-dispatch-%d-%d", time.Now().UnixNano(), idx),
			PreferLocal:    true,
			PreferProvider: req.Provider,
			Source:         "local-harness-dispatch",
		},
	}

	start := time.Now()
	resp, clientCalls, transcript, err := c.completeWithToolLoop(slotCtx, counting, compReq, registry)
	res.DurationSec = time.Since(start).Seconds()
	res.Turns = counting.CompleteCalls()
	for _, tc := range transcript {
		entry := DispatchToolCallSummary{
			Name:         tc.Name,
			ArgsDigest:   truncateDigest(tc.Arguments),
			ResultDigest: truncateDigest(tc.Result),
		}
		if tc.Rejected {
			entry.Error = tc.RejectReason
		}
		res.ToolCalls = append(res.ToolCalls, entry)
	}
	if len(clientCalls) > 0 {
		res.Error = fmt.Sprintf("unsupported client tool calls returned: %d", len(clientCalls))
	}
	if err != nil {
		if slotCtx.Err() == context.DeadlineExceeded {
			res.Error = "timeout"
		} else {
			res.Error = err.Error()
		}
		return res
	}
	res.Success = true
	res.Content = strings.TrimSpace(resp.Content)
	if res.Content == "" && len(transcript) > 0 {
		res.Content = summarizeToolTranscript(transcript)
	}
	// Routing notes (e.g. "state-routing: state=receptive -> provider=mlx-lm")
	// are batch-level diagnostics already captured in batch.Notes by the
	// caller; do NOT copy them into res.Error on successful slots — that
	// produces misleading success=true + non-empty error results.
	return res
}

func (c *LocalHarnessController) completeWithToolLoop(ctx context.Context, provider Provider, req *CompletionRequest, registry *KernelToolRegistry) (*CompletionResponse, []ToolCall, []ToolCallRecord, error) {
	resp, err := provider.Complete(ctx, req)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(resp.ToolCalls) == 0 {
		return resp, nil, nil, nil
	}
	return RunToolLoopWithTranscript(ctx, provider, req, resp, registry)
}

type countingProvider struct {
	Provider
	completeCalls atomic.Int64
}

func (p *countingProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	p.completeCalls.Add(1)
	return p.Provider.Complete(ctx, req)
}

func (p *countingProvider) CompleteCalls() int {
	return int(p.completeCalls.Load())
}

func clampUrgency(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func decodeJSONPayload(raw string, out any) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("empty response")
	}
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end >= start {
		raw = raw[start : end+1]
	}
	return json.Unmarshal([]byte(raw), out)
}

func summarizeMemoryEntry(rec localHarnessCycleRecord) string {
	switch {
	case rec.Result != "":
		return truncateDigest(rec.Result)
	case rec.Reason != "":
		return truncateDigest(rec.Reason)
	default:
		return rec.Action
	}
}

func summarizeToolTranscript(records []ToolCallRecord) string {
	if len(records) == 0 {
		return ""
	}
	last := records[len(records)-1]
	if last.Result != "" {
		return truncateDigest(last.Result)
	}
	return last.Name
}

func truncateDigest(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "..."
}

func sinceString(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return time.Since(ts).Round(time.Second).String()
}
