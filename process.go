// process.go — CogOS v3 continuous process state machine
//
// Implements the always-running cognitive process described in the v3 spec.
// The process has four states and an internal event loop that runs independently
// of external HTTP requests.
//
// States:
//
//	Active       — processing an external perturbation
//	Receptive    — idle, listening for input
//	Consolidating — running internal maintenance (memory, coherence)
//	Dormant      — minimal activity, heartbeat only
//
// The select loop is the core architectural difference from v2:
// v2 is request-triggered; v3 has internal tickers that fire regardless.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ProcessState represents the four operational states of the v3 process.
type ProcessState int

const (
	StateActive        ProcessState = iota // Processing external input
	StateReceptive                         // Idle, waiting
	StateConsolidating                     // Internal maintenance
	StateDormant                           // Minimal activity
)

func (s ProcessState) String() string {
	switch s {
	case StateActive:
		return "active"
	case StateReceptive:
		return "receptive"
	case StateConsolidating:
		return "consolidating"
	case StateDormant:
		return "dormant"
	default:
		return "unknown"
	}
}

// TrustState tracks kernel-local identity and coherence trust metadata.
type TrustState struct {
	LocalScore           float64   `json:"local_score"`
	LastHeartbeatHash    string    `json:"last_heartbeat_hash,omitempty"`
	LastHeartbeatAt      time.Time `json:"last_heartbeat_at,omitempty"`
	CoherenceFingerprint string    `json:"coherence_fingerprint,omitempty"`
}

// Process is the always-running cognitive process.
type Process struct {
	mu      sync.RWMutex
	state   ProcessState
	nucleus *Nucleus
	field   *AttentionalField
	gate    *Gate
	cfg     *Config

	// sessionID is the persistent process session identifier.
	sessionID string

	// startedAt records when this process instance was created.
	startedAt time.Time

	// NodeID is the stable kernel node identity persisted across restarts.
	NodeID string

	// TrustState carries local trust, coherence, and heartbeat metadata.
	TrustState TrustState

	// externalCh receives events from the HTTP serve layer.
	externalCh chan *GateEvent

	// index is the CogDoc index, rebuilt on each consolidation.
	indexMu sync.RWMutex
	index   *CogDocIndex

	// observer is the trajectory model that closes the trefoil loop:
	// field state → prediction → salience actions → field state.
	observer *TrajectoryModel

	// trm is the MambaTRM temporal retrieval model (nil if weights not loaded).
	trm *MambaTRM

	// embeddingIndex is the CogDoc embedding index for TRM pre-filtering (nil if not loaded).
	embeddingIndex *EmbeddingIndex

	// lightCones manages per-conversation SSM hidden states.
	lightCones *LightConeManager

	// lastConsolidation records when the previous consolidation ran, so the
	// observer can filter the attention log to the current tick window.
	lastConsolidation time.Time

	// lastCoherenceReport caches the most recent coherence result so the
	// heartbeat can reuse it instead of recomputing.
	lastCoherenceReport *CoherenceReport

	// lastIndexHEAD tracks the HEAD hash at last index rebuild, so we skip
	// rebuilding when nothing has changed.
	lastIndexHEAD string
}

// NewProcess constructs and initialises the process.
func NewProcess(cfg *Config, nucleus *Nucleus) *Process {
	field := NewAttentionalField(cfg)
	gate := NewGate(field, cfg)
	now := time.Now().UTC()
	return &Process{
		state:     StateReceptive,
		nucleus:   nucleus,
		field:     field,
		gate:      gate,
		cfg:       cfg,
		sessionID: uuid.New().String(),
		startedAt: now,
		NodeID:    loadOrCreateNodeID(cfg),
		TrustState: TrustState{
			LocalScore:           1.0,
			CoherenceFingerprint: "sha256:" + sha256Hex("coherence:unknown"),
		},
		externalCh:        make(chan *GateEvent, 64),
		observer:          NewTrajectoryModel(),
		lastConsolidation: now,
	}
}

// State returns the current process state (safe for concurrent reads).
func (p *Process) State() ProcessState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

// Field returns the attentional field (for use by the serve layer).
func (p *Process) Field() *AttentionalField {
	return p.field
}

// Gate returns the attentional gate.
func (p *Process) Gate() *Gate {
	return p.gate
}

// Index returns the current CogDoc index (may be nil before first consolidation).
func (p *Process) Index() *CogDocIndex {
	p.indexMu.RLock()
	defer p.indexMu.RUnlock()
	return p.index
}

// SessionID returns the process session identifier.
func (p *Process) SessionID() string {
	return p.sessionID
}

// StartedAt returns when this process instance was created.
func (p *Process) StartedAt() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.startedAt
}

// TrustSnapshot returns a copy of the current trust metadata.
func (p *Process) TrustSnapshot() TrustState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.TrustState
}

// Fingerprint returns a stable trust fingerprint for the current process state.
func (p *Process) Fingerprint() string {
	p.mu.RLock()
	nodeID := p.NodeID
	coherenceState := p.TrustState.CoherenceFingerprint
	p.mu.RUnlock()

	workspaceRoot := ""
	if p.cfg != nil {
		workspaceRoot = p.cfg.WorkspaceRoot
	}

	nucleusHash := p.nucleusDigest()
	material := strings.Join([]string{nodeID, workspaceRoot, nucleusHash, coherenceState}, "|")
	return "sha256:" + sha256Hex(material)
}

// Observer returns the trajectory model (for use by the HTTP layer).
func (p *Process) Observer() *TrajectoryModel {
	return p.observer
}

// TRM returns the MambaTRM model (nil if not loaded).
func (p *Process) TRM() *MambaTRM {
	return p.trm
}

// EmbeddingIndex returns the embedding index (nil if not loaded).
func (p *Process) EmbeddingIndex() *EmbeddingIndex {
	return p.embeddingIndex
}

// LightCones returns the per-conversation light cone manager.
func (p *Process) LightCones() *LightConeManager {
	return p.lightCones
}

// SetTRM installs the TRM model and embedding index (called at startup).
func (p *Process) SetTRM(trm *MambaTRM, idx *EmbeddingIndex) {
	p.trm = trm
	p.embeddingIndex = idx
	p.lightCones = NewLightConeManager(trm)
}

// Send delivers an external event to the process loop (non-blocking).
// Returns false if the channel is full.
func (p *Process) Send(evt *GateEvent) bool {
	select {
	case p.externalCh <- evt:
		return true
	default:
		return false
	}
}

// Run starts the continuous process loop. It blocks until ctx is cancelled.
func (p *Process) Run(ctx context.Context) error {
	// Build CogDoc index first (fast — just frontmatter parsing).
	// This must happen before field.Update() which can be slow (git log per file).
	slog.Info("process: building initial CogDoc index")
	if idx, err := BuildIndex(p.cfg.WorkspaceRoot); err != nil {
		slog.Warn("process: initial index build failed", "err", err)
	} else {
		p.indexMu.Lock()
		p.index = idx
		p.indexMu.Unlock()
		slog.Info("process: index built", "docs", len(idx.ByURI))
	}

	// Update attentional field (slow — git log per file, runs in background).
	go func() {
		slog.Info("process: updating attentional field (background)")
		if err := p.field.Update(); err != nil {
			slog.Warn("process: field update failed", "err", err)
		} else {
			slog.Info("process: field updated", "files", p.field.Len())
		}
	}()

	// Emit genesis event.
	p.emitEvent("process.start", map[string]interface{}{
		"state":    p.State().String(),
		"session":  p.sessionID,
		"identity": p.nucleus.Name,
	})

	consolidationTicker := time.NewTicker(time.Duration(p.cfg.ConsolidationInterval) * time.Second)
	heartbeatTicker := time.NewTicker(time.Duration(p.cfg.HeartbeatInterval) * time.Second)
	defer consolidationTicker.Stop()
	defer heartbeatTicker.Stop()

	slog.Info("process: running", "state", p.State(), "session", p.sessionID)

	for {
		select {
		case <-ctx.Done():
			p.emitEvent("process.stop", map[string]interface{}{
				"reason": ctx.Err().Error(),
			})
			slog.Info("process: stopped", "reason", ctx.Err())
			return nil

		case evt := <-p.externalCh:
			p.handleExternal(evt)

		case <-consolidationTicker.C:
			p.runConsolidation()

		case <-heartbeatTicker.C:
			p.emitHeartbeat()
		}
	}
}

// handleExternal processes an external perturbation.
func (p *Process) handleExternal(evt *GateEvent) {
	result := p.gate.Process(evt)
	p.transition(result.StateTransition)
	slog.Debug("process: external event",
		"type", evt.Type,
		"state", p.State(),
		"elevated", len(result.Elevated),
	)
}

// runConsolidation runs the internal maintenance loop.
// This is the observer's heartbeat — the tick where:
//
//	Loop 1: the field is read (field state + attention log → perception)
//	Loop 2: the TrajectoryModel is updated (prediction + error computation)
//	Loop 3: the model acts on the field (pre-warm, attenuate, coherence signal)
func (p *Process) runConsolidation() {
	p.transition(StateConsolidating)

	// Record the current tick window before updating lastConsolidation.
	now := time.Now()
	tickStart := p.lastConsolidation
	p.lastConsolidation = now

	slog.Debug("process: consolidating", "window_since", tickStart.Format(time.RFC3339))

	// ── Loop 1: Update the attentional field (Field → Observer) ────────────
	if err := p.field.Update(); err != nil {
		slog.Warn("process: field update failed", "err", err)
	}

	// Rebuild the CogDoc index only if HEAD has changed.
	currentHEAD := resolveHEAD(p.cfg.WorkspaceRoot)
	if currentHEAD == "" || currentHEAD != p.lastIndexHEAD {
		if idx, err := BuildIndex(p.cfg.WorkspaceRoot); err != nil {
			slog.Warn("process: index rebuild failed", "err", err)
		} else {
			p.indexMu.Lock()
			p.index = idx
			p.indexMu.Unlock()
			p.lastIndexHEAD = currentHEAD
		}
	}

	// Run coherence check and cache the result for the heartbeat.
	p.indexMu.RLock()
	currentIdx := p.index
	p.indexMu.RUnlock()
	report := RunCoherence(p.cfg, p.nucleus, currentIdx)
	p.lastCoherenceReport = report
	if !report.Pass {
		slog.Warn("process: coherence check failed", "results", len(report.Results))
		p.emitEvent("coherence.fail", map[string]interface{}{"pass": false})
	}

	// Read attention signals since the last tick (Loop 1 percept).
	attended := readRecentAttentionSignals(p.cfg.WorkspaceRoot, tickStart)
	fieldScores := p.field.AllScores()

	// ── Loop 2: Update the trajectory model (Observer → Model) ─────────────
	u := p.observer.Update(attended, fieldScores)

	slog.Debug("process: observer",
		"cycle", u.Cycle,
		"attended", len(attended),
		"prediction_error", fmt.Sprintf("%.4f", u.PredictionError),
		"mean_error", fmt.Sprintf("%.4f", u.MeanError),
		"predicted", len(u.Prediction),
		"receding", len(u.Receding),
	)

	// Detect surprise: error above threshold on cycle > 1 (first cycle has
	// no prior prediction, so error is always 0 or 1 depending on attendance).
	surprise := u.PredictionError > surpriseThreshold && u.Cycle > 1
	if surprise {
		slog.Info("process: observer surprise",
			"error", fmt.Sprintf("%.4f", u.PredictionError),
			"threshold", surpriseThreshold,
			"cycle", u.Cycle,
		)
		p.emitEvent("observer.surprise", map[string]interface{}{
			"cycle":            u.Cycle,
			"prediction_error": u.PredictionError,
			"threshold":        surpriseThreshold,
			"attended":         attended,
		})
	}

	// Record prediction in the ledger — hash-chained, irreversible.
	// This is the arrow of time: the model's anticipation, written before
	// the outcome is known.
	p.emitEvent("observer.prediction", map[string]interface{}{
		"cycle":            u.Cycle,
		"prediction_error": u.PredictionError,
		"mean_error":       u.MeanError,
		"prediction":       u.Prediction,
		"attended":         attended,
		"surprise":         surprise,
	})

	// ── Loop 3: Model acts on the field (Model → Field) ────────────────────
	warmed, attenuated := applyObserverActions(p.field, p.observer, u.Prediction, u.Receding)

	slog.Debug("process: observer field actions", "warmed", warmed, "attenuated", attenuated)

	// Write consolidation CogDoc — the model's trace in the field.
	if err := writeConsolidationDoc(p.cfg, u, surprise); err != nil {
		slog.Warn("process: consolidation doc write failed", "err", err)
	}

	p.emitEvent("consolidation.complete", map[string]interface{}{
		"field_size":            p.field.Len(),
		"coherence_pass":        report.Pass,
		"observer_cycle":        u.Cycle,
		"prediction_error":      u.PredictionError,
		"mean_prediction_error": u.MeanError,
		"warmed":                warmed,
		"attenuated":            attenuated,
		"surprise":              surprise,
	})

	p.transition(StateReceptive)
}

// emitHeartbeat fires during the dormant state.
func (p *Process) emitHeartbeat() {
	// Only emit heartbeat if not already in an active state.
	if p.State() == StateActive {
		return
	}

	// Reuse the cached coherence report from the last consolidation tick
	// instead of recomputing it. Fall back to a fresh check if no cache exists.
	report := p.lastCoherenceReport
	if report == nil {
		p.indexMu.RLock()
		currentIdx := p.index
		p.indexMu.RUnlock()
		report = RunCoherence(p.cfg, p.nucleus, currentIdx)
	}
	coherenceHash := coherenceFingerprint(report)
	now := time.Now().UTC()

	p.mu.Lock()
	p.TrustState.CoherenceFingerprint = coherenceHash
	p.mu.Unlock()

	p.transition(StateDormant)
	heartbeat := map[string]interface{}{
		"state":          p.State().String(),
		"field_size":     p.field.Len(),
		"node_id":        p.NodeID,
		"fingerprint":    p.Fingerprint(),
		"timestamp":      now.Format(time.RFC3339),
		"coherence_hash": coherenceHash,
	}
	p.emitEvent("heartbeat", heartbeat)

	raw, _ := json.Marshal(heartbeat)
	trust := p.TrustSnapshot()
	block := &CogBlock{
		ID:              uuid.NewString(),
		Timestamp:       now,
		SessionID:       p.sessionID,
		SourceChannel:   "internal",
		SourceTransport: "direct",
		SourceIdentity:  p.NodeID,
		WorkspaceID:     filepath.Base(p.cfg.WorkspaceRoot),
		Kind:            BlockSystemEvent,
		RawPayload:      raw,
		Messages:        []ProviderMessage{{Role: "system", Content: "heartbeat"}},
		Provenance: BlockProvenance{
			OriginSession: p.sessionID,
			OriginChannel: "internal",
			IngestedAt:    now,
			NormalizedBy:  "direct",
		},
		TrustContext: TrustContext{
			Authenticated: true,
			TrustScore:    trust.LocalScore,
			Scope:         "local",
		},
	}
	if p.nucleus != nil {
		block.TargetIdentity = p.nucleus.Name
	}
	ref := p.RecordBlock(block)

	p.mu.Lock()
	p.TrustState.LastHeartbeatHash = ref
	p.TrustState.LastHeartbeatAt = now
	p.mu.Unlock()
}

// transition moves the process to a new state (with logging).
func (p *Process) transition(next ProcessState) {
	p.mu.Lock()
	prev := p.state
	p.state = next
	p.mu.Unlock()
	if prev != next {
		slog.Debug("process: state transition", "from", prev, "to", next)
	}
}

// emitEvent records a ledger event for the process session.
func (p *Process) emitEvent(eventType string, data map[string]interface{}) {
	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      eventType,
			Timestamp: nowISO(),
			SessionID: p.sessionID,
			Data:      data,
		},
		Metadata: EventMetadata{
			Source: "kernel-v3",
		},
	}
	if err := AppendEvent(p.cfg.WorkspaceRoot, p.sessionID, env); err != nil {
		slog.Debug("process: ledger append failed", "err", fmt.Sprintf("%v", err))
	}
}

func loadOrCreateNodeID(cfg *Config) string {
	if cfg == nil {
		return uuid.NewString()
	}
	runDir := filepath.Join(cfg.CogDir, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return uuid.NewString()
	}
	path := filepath.Join(runDir, "node_id")
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}
	id := uuid.NewString()
	if err := os.WriteFile(path, []byte(id+"\n"), 0o644); err != nil {
		return id
	}
	return id
}

func (p *Process) nucleusDigest() string {
	if p == nil || p.nucleus == nil {
		return "sha256:" + sha256Hex("nucleus:nil")
	}
	material := strings.Join([]string{p.nucleus.Name, p.nucleus.Role, p.nucleus.Card}, "|")
	return "sha256:" + sha256Hex(material)
}

func coherenceFingerprint(report *CoherenceReport) string {
	if report == nil {
		return "sha256:" + sha256Hex("coherence:nil")
	}
	parts := []string{fmt.Sprintf("pass:%t", report.Pass)}
	for _, result := range report.Results {
		rule := ""
		expected := ""
		actual := ""
		if result.Diagnostic != nil {
			rule = result.Diagnostic.Rule
			expected = result.Diagnostic.Expected
			actual = result.Diagnostic.Actual
		}
		parts = append(parts, strings.Join([]string{
			result.Layer,
			fmt.Sprintf("%t", result.Pass),
			rule,
			expected,
			actual,
		}, "|"))
	}
	return "sha256:" + sha256Hex(strings.Join(parts, "\n"))
}

func sha256Hex(input string) string {
	digest := sha256.Sum256([]byte(input))
	return hex.EncodeToString(digest[:])
}
