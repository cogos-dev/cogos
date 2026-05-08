// agent_dispatch.go — Concrete dispatcher behind the cog_dispatch_to_harness
// MCP tool. Lives in the root package so it can call AgentHarness.ExecuteScoped
// directly; engine-side types (DispatchRequest, AgentDispatcher) are defined
// in internal/engine/agent_dispatch.go to keep the import direction one-way.
//
// Pool mechanism: option (c) hybrid from the Phase 2 plan.
// A single AgentHarness *struct* is shared across dispatches; each dispatch
// is a goroutine that calls ExecuteScoped with its own per-call options
// (system prompt, tool subset, backend URL/kind/model, think flag, deadline).
// Concurrency is bounded by N (1..4); KV-cache isolation is provided by
// Ollama's per-request cache keying — each call rebuilds the messages slice
// fresh and posts to /api/chat with a distinct context. No shared mutable
// state between slots beyond the harness's tool-func map (read-only after
// RegisterCoreTools).
//
// Identity propagation: claims travel via DispatchRequest.Identity. The
// dispatcher logs them on the cycle-trace metadata and stores a tagged
// context value so the respond tool's session_id behavior is preserved
// (a dispatch from Claude carries Sub as a synthetic session for fan-out).
// Full CRD-based identity binding waits for Wave 6b.

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/myrgic/cogos/internal/engine"
)

// HarnessDispatcher is the AgentDispatcher implementation backed by the
// kernel's resident *AgentHarness. The MCP layer reaches it via the
// AgentController interface (it is composed into the same controller
// adapter that surfaces ListAgents / GetAgent / TriggerAgent).
type HarnessDispatcher struct {
	// AgentID is the handle this dispatcher serves. Today always
	// engine.DefaultAgentID. The plural-ready API surface preserves room
	// for future multi-agent deployments without a wire break.
	AgentID string

	// Harness is the resident harness instance. The dispatcher does not
	// take ownership; the kernel owns the lifecycle. The ExecuteScoped
	// method on *AgentHarness is goroutine-safe by construction (read-only
	// access to h.tools and h.toolFuncs after RegisterCoreTools).
	Harness *AgentHarness

	// LMStudioBaseURL is the OpenAI-compatible endpoint the "26b" model
	// route targets. Empty disables the route — all 26b requests then
	// degrade to e4b with a per-slot warning.
	LMStudioBaseURL string

	// LMStudioModel is the model name to send in the OpenAI request when
	// routing to LM Studio. Defaults to "gemma-3-27b-it" if empty (the
	// canonical 27B-class identifier; LM Studio is tolerant of either
	// identifier prefix).
	LMStudioModel string

	// reachable cache for LM Studio. mu protects the two fields together.
	// reachableUntil is the wall-clock instant after which we re-probe;
	// reachableOK records the last probe result.
	mu             sync.Mutex
	reachableUntil time.Time
	reachableOK    bool
}

// reachabilityCacheTTL is how long a successful or failed LM Studio probe
// is trusted before re-checking. 60s lines up with the Phase 2 plan and
// keeps probe overhead negligible.
const reachabilityCacheTTL = 60 * time.Second

// reachabilityProbeTimeout is the per-probe HTTP timeout. 2s is enough for a
// localhost-LAN /v1/models response and keeps a misconfigured remote from
// pinning the dispatcher.
const reachabilityProbeTimeout = 2 * time.Second

// defaultLMStudioModel is the model identifier sent to LM Studio when the
// dispatcher's LMStudioModel field is empty.
const defaultLMStudioModel = "gemma-3-27b-it"

// DispatchToHarness implements engine.AgentDispatcher. See the engine-side
// contract for semantics. This method blocks until every slot has finished
// (success, error, or timeout); the returned batch is always non-nil when
// err is nil.
func (d *HarnessDispatcher) DispatchToHarness(ctx context.Context, req engine.DispatchRequest) (*engine.DispatchBatchResult, error) {
	if d == nil || d.Harness == nil {
		return nil, engine.ErrAgentUnavailable
	}
	if req.AgentID != "" && d.AgentID != "" && req.AgentID != d.AgentID {
		return nil, engine.ErrAgentNotFound
	}

	// Validate the requested tool allowlist against the live registry
	// once, batch-wide. The same allowlist applies to every slot, so a
	// per-slot recheck would be wasted work.
	if len(req.Tools) > 0 {
		registered := stringSet(d.Harness.ToolNames())
		for _, name := range req.Tools {
			if _, ok := registered[name]; !ok {
				return nil, &engine.AgentControllerError{
					Code:    "invalid_input",
					Message: fmt.Sprintf("tool %q not registered (available: %s)", name, strings.Join(d.Harness.ToolNames(), ", ")),
				}
			}
		}
	}

	// Decide model routing once per batch. If the caller asked for 26B and
	// LM Studio is unreachable, every slot degrades to e4b with the same
	// warning attached.
	resolvedModel := req.Model
	var degradeNote string
	if resolvedModel == engine.DispatchModel26B {
		if d.lmStudioReachable(ctx) {
			// stay on 26b
		} else {
			resolvedModel = engine.DispatchModelE4B
			degradeNote = "26b backend unreachable, all slots degraded to e4b"
		}
	}

	batch := &engine.DispatchBatchResult{
		Results: make([]engine.DispatchResult, req.N),
	}
	if degradeNote != "" {
		batch.Notes = append(batch.Notes, degradeNote)
	}

	// Stamp identity onto the parent context for trace correlation.
	parentCtx := withDispatchIdentity(ctx, req.Identity)

	// Fan out N goroutines; each writes its slot in batch.Results by
	// index. No mutex needed because slot indices are disjoint.
	batchStart := time.Now()
	var wg sync.WaitGroup
	wg.Add(req.N)
	for i := 0; i < req.N; i++ {
		idx := i
		go func() {
			defer wg.Done()
			batch.Results[idx] = d.runSlot(parentCtx, idx, req, resolvedModel)
		}()
	}
	wg.Wait()
	batch.TotalDurationSec = time.Since(batchStart).Seconds()

	if degradeNote != "" {
		// Replicate the batch-level note in each slot's Error so callers
		// inspecting individual slots see the degradation context.
		for i := range batch.Results {
			if batch.Results[i].Error == "" {
				batch.Results[i].Error = degradeNote
			}
		}
	}
	return batch, nil
}

// runSlot runs one dispatch index to completion. Returns a populated
// DispatchResult with Success, Content, Error, etc filled in.
func (d *HarnessDispatcher) runSlot(parentCtx context.Context, idx int, req engine.DispatchRequest, resolvedModel engine.DispatchModel) engine.DispatchResult {
	res := engine.DispatchResult{
		Index:     idx,
		ModelUsed: resolvedModel,
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	slotCtx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	opts := ExecuteScopedOptions{
		SystemPrompt: req.SystemPrompt,
		AllowedTools: req.Tools,
		Thinking:     req.Thinking,
	}
	if resolvedModel == engine.DispatchModel26B {
		opts.BackendURL = d.LMStudioBaseURL
		opts.BackendKind = backendKindOpenAI
		opts.Model = d.LMStudioModel
		if opts.Model == "" {
			opts.Model = defaultLMStudioModel
		}
	}

	slotStart := time.Now()
	r, err := d.Harness.ExecuteScoped(slotCtx, req.Task, opts)
	res.DurationSec = time.Since(slotStart).Seconds()

	if r != nil {
		res.Turns = r.Turns
		res.Content = r.Content
		for _, tc := range r.ToolCalls {
			res.ToolCalls = append(res.ToolCalls, engine.DispatchToolCallSummary{
				Name:         tc.Name,
				ArgsDigest:   tc.ArgsDigest,
				ResultDigest: tc.ResultDigest,
				Error:        tc.Error,
			})
		}
	}

	if err != nil {
		// Distinguish timeout from generic failure. context.DeadlineExceeded
		// is canonical; the harness wraps it via fmt.Errorf so check the
		// slot context directly.
		if slotCtx.Err() == context.DeadlineExceeded {
			res.Error = "timeout"
		} else {
			res.Error = err.Error()
		}
		res.Success = false
		return res
	}
	res.Success = true
	return res
}

// lmStudioReachable returns true when the LM Studio /v1/models endpoint
// responded OK within reachabilityProbeTimeout in the last
// reachabilityCacheTTL. Empty LMStudioBaseURL always returns false (the
// route is disabled).
func (d *HarnessDispatcher) lmStudioReachable(ctx context.Context) bool {
	if d.LMStudioBaseURL == "" {
		return false
	}
	d.mu.Lock()
	if time.Now().Before(d.reachableUntil) {
		ok := d.reachableOK
		d.mu.Unlock()
		return ok
	}
	d.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, reachabilityProbeTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(probeCtx, http.MethodGet, d.LMStudioBaseURL+"/v1/models", nil)
	if err != nil {
		d.cacheReachability(false)
		return false
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		log.Printf("[dispatch] lm-studio probe failed: %v", err)
		d.cacheReachability(false)
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	ok := resp.StatusCode == http.StatusOK
	d.cacheReachability(ok)
	return ok
}

// cacheReachability stores the probe result with a TTL.
func (d *HarnessDispatcher) cacheReachability(ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reachableOK = ok
	d.reachableUntil = time.Now().Add(reachabilityCacheTTL)
}

// dispatchIdentityKey is the context key used to thread DispatchIdentity
// into the harness's tool dispatchers. Tools that care about caller
// provenance (respond's session_id fan-out, propose's authorship metadata)
// can read it via DispatchIdentityFromContext.
type dispatchIdentityKey struct{}

// withDispatchIdentity stamps the identity onto ctx. Empty identity is a
// no-op so existing tests don't see a synthetic identity stamp.
func withDispatchIdentity(ctx context.Context, id engine.DispatchIdentity) context.Context {
	if id.Iss == "" && id.Sub == "" && id.Aud == "" && len(id.Claims) == 0 {
		return ctx
	}
	return context.WithValue(ctx, dispatchIdentityKey{}, id)
}

// DispatchIdentityFromContext returns the identity claims attached by the
// dispatcher, or the zero value when the call did not originate from a
// dispatch.
func DispatchIdentityFromContext(ctx context.Context) engine.DispatchIdentity {
	if ctx == nil {
		return engine.DispatchIdentity{}
	}
	v, _ := ctx.Value(dispatchIdentityKey{}).(engine.DispatchIdentity)
	return v
}

// stringSet builds a presence map from a slice. Used to validate the
// requested tool allowlist against the harness registry without a nested
// loop scan per name.
func stringSet(ss []string) map[string]struct{} {
	out := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		out[s] = struct{}{}
	}
	return out
}
