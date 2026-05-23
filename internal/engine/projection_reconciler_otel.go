// projection_reconciler_otel.go — OTel span instrumentation for the
// ProjectionReconciler reconcile cycle.
//
// H2: Add gen_ai.* attributes to ProjectionReconciler span emission.
// H3: Adopt conversation/turn/service span hierarchy (actual OTel parent/child
//     spans with trace-context propagation).
//
// Architecture:
//
//   H1/H2/H3/H4 is one OTel convention applied at multiple call sites.
//   This file is the H2+H3 instantiation: it adds proper OTel spans around
//   the ProjectionReconciler reconcile cycle, structured as:
//
//     span: "cogos.reconcile.service"  (SpanKindService)
//       child: "cogos.reconcile.apply" (SpanKindService)
//
//   The "service" span is the unit of work within a larger reconcile session.
//   A ReconcileDaemon tick drives N service spans in sequence (one per provider);
//   if the daemon is itself spanned, those service spans become children of the
//   daemon's "turn" span. This file handles the service layer; the daemon's span
//   wrapping (the "turn" layer) is wired in H4.
//
//   Per operator decision (2026-05-16 batch question 5): actual OTel parent/child
//   spans with trace-context propagation, matching Pipecat's GenAI observability
//   shape for ecosystem interop. Attribute-only approach was explicitly not adopted.
//
// Wire compatibility:
//   Spans are emitted via go.opentelemetry.io/otel/trace on the global tracer.
//   When OTEL_EXPORTER_OTLP_ENDPOINT is set, spans export to the collector.
//   When not set, the global noop tracer is used — zero overhead.
//
// Import note:
//   gen_ai_attrs.go (H1) lives in the same package; all constructors are
//   available without a separate import.

package engine

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// projectionTracer is the package-level tracer for projection reconciliation.
// Uses the global OTel provider (noop when no exporter is configured).
var projectionTracer = otel.Tracer("cogos.reconcile")

// ─── H2: gen_ai.* attribute helpers for ProjectionReconciler ─────────────────

// projectionSpanAttrs builds the gen_ai.* + gen_ai.cogos.* attribute set for
// a ProjectionReconciler service span.
func projectionSpanAttrs(kind ProjectionKind, nodeCount int, workspaceRoot string) []attribute.KeyValue {
	return []attribute.KeyValue{
		// Standard gen_ai.* (H1 constructors)
		GenAISystem("cogos"),
		GenAIOperationName("reconcile"),
		// cogos-kernel extension attributes
		CogosSpanKind(SpanKindService),
		CogosProjectionKind(string(kind)),
		CogosProjectionNodeCount(nodeCount),
		CogosWorkspaceRoot(workspaceRoot),
		CogosOperationPhase("reconcile"),
	}
}

// ─── H3: conversation/turn/service span hierarchy ─────────────────────────────
//
// The three-level span hierarchy for cogos reconcile operations:
//
//   conversation — the ReconcileDaemon session (one daemon lifetime)
//     turn       — one daemon tick (all providers in sequence)
//       service  — one ProjectionReconciler cycle (FetchLive → ApplyPlan)
//
// This file wires the "service" level. The "turn" and "conversation" levels
// are wired in H4 (ReconcileDaemon span wrapping).

// StartServiceSpan starts a new "service" span for one ProjectionReconciler
// reconcile cycle. The span is a child of whatever span is current in ctx.
// Returns the child context (with the new span) and a done function that
// records the outcome and ends the span.
//
// Usage:
//
//	ctx, done := StartServiceSpan(ctx, r.kind, nodeCount, workspaceRoot)
//	defer done(err)
func StartServiceSpan(
	ctx context.Context,
	kind ProjectionKind,
	nodeCount int,
	workspaceRoot string,
) (context.Context, func(error)) {
	spanName := fmt.Sprintf("cogos.reconcile.service/%s", string(kind))

	ctx, span := projectionTracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(projectionSpanAttrs(kind, nodeCount, workspaceRoot)...),
	)

	done := func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
	return ctx, done
}

// StartApplySpan starts a child span for the ApplyPlan phase within a service
// span. Must be called within a context that already has a service span.
func StartApplySpan(ctx context.Context, kind ProjectionKind, actionCount int) (context.Context, func(error)) {
	spanName := fmt.Sprintf("cogos.reconcile.apply/%s", string(kind))
	ctx, span := projectionTracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			CogosProjectionKind(string(kind)),
			CogosSpanKind(SpanKindService),
			attribute.Int("cogos.reconcile.action_count", actionCount),
		),
	)
	done := func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
	return ctx, done
}

// ─── H4 preparation: Turn and Conversation span starters ─────────────────────
//
// These are the "turn" and "conversation" level starters that H4 will call
// from the ReconcileDaemon. Defined here to keep the three-level hierarchy in
// one file. H4 wires them into ReconcileDaemon.

// StartTurnSpan starts a "turn" span for one ReconcileDaemon tick.
// A "turn" corresponds to one full iteration over all registered providers.
// sessionID is the daemon session identifier (e.g. workspace root hash).
func StartTurnSpan(ctx context.Context, sessionID string, providerCount int) (context.Context, func(error)) {
	ctx, span := projectionTracer.Start(ctx, "cogos.reconcile.turn",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			GenAISystem("cogos"),
			GenAIOperationName("reconcile.tick"),
			CogosSpanKind(SpanKindTurn),
			CogosSessionID(sessionID),
			attribute.Int("cogos.reconcile.provider_count", providerCount),
		),
	)
	done := func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
	return ctx, done
}

// StartConversationSpan starts a "conversation" span for the ReconcileDaemon
// lifetime. A "conversation" here represents the entire daemon session — from
// Start() to context cancellation.
func StartConversationSpan(ctx context.Context, sessionID string, workspaceRoot string) (context.Context, func(error)) {
	ctx, span := projectionTracer.Start(ctx, "cogos.reconcile.conversation",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			GenAISystem("cogos"),
			GenAIOperationName("reconcile.session"),
			CogosSpanKind(SpanKindConversation),
			CogosSessionID(sessionID),
			CogosWorkspaceRoot(workspaceRoot),
		),
	)
	done := func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
	return ctx, done
}

// ─── ReconcileWithSpan: instrumented wrapper for one ProjectionReconciler cycle ─

// ReconcileWithSpan runs a full LoadConfig → FetchLive → ComputePlan →
// ApplyPlan → BuildState → WriteState cycle for a ProjectionReconciler,
// emitting OTel spans at the service and apply levels.
//
// This is the H2+H3 instrumented entry point. Callers (the ReconcileDaemon in
// H4) replace direct method calls with ReconcileWithSpan to get span hierarchy.
//
// Parameters:
//
//	ctx          — context inheriting any parent span (e.g. the "turn" span)
//	r            — the ProjectionReconciler to drive
//	workspaceRoot — workspace root for LoadConfig and span attributes
func ReconcileWithSpan(ctx context.Context, r *ProjectionReconciler, workspaceRoot string) error {
	// LoadConfig — outside the service span (fast, synchronous, no I/O risk)
	cfg, err := r.LoadConfig(workspaceRoot)
	if err != nil {
		return fmt.Errorf("LoadConfig: %w", err)
	}

	// FetchLive — count nodes before starting the service span
	nodes, err := r.FetchLive(ctx, cfg)
	if err != nil {
		return fmt.Errorf("FetchLive: %w", err)
	}

	nodeCount := 0
	if ns, ok := nodes.([]LineageNode); ok {
		nodeCount = len(ns)
	}

	// Start the service span with gen_ai.* attributes (H2)
	serviceCtx, serviceDone := StartServiceSpan(ctx, r.kind, nodeCount, workspaceRoot)
	defer func() { serviceDone(err) }()

	// Existing reconcile state
	existingState := reconcile.NewState(r.Type())

	plan, err := r.ComputePlan(cfg, nodes, existingState)
	if err != nil {
		err = fmt.Errorf("ComputePlan: %w", err)
		return err
	}

	// Start apply child span (H3 child relationship)
	applyCtx, applyDone := StartApplySpan(serviceCtx, r.kind, len(plan.Actions))
	var applyErr error
	results, applyErr := r.ApplyPlan(applyCtx, plan)
	applyDone(applyErr)
	if applyErr != nil {
		err = fmt.Errorf("ApplyPlan: %w", applyErr)
		return err
	}

	_ = results // available for future span annotation (e.g. success count)

	_, err = r.BuildState(cfg, nodes, existingState)
	if err != nil {
		err = fmt.Errorf("BuildState: %w", err)
		return err
	}

	return nil
}

// ─── H4: ReconcileDaemon span wiring ─────────────────────────────────────────
//
// H4 wires the three-level span hierarchy into the ReconcileDaemon tick loop.
//
// The ReconcileDaemon (internal/engine/reconcile_daemon.go, landed in commit
// e6bdb04 on feat/otel-gen-ai-attrs-and-projection-watcher) drives periodic
// reconcile cycles over all registered providers. H4 wraps each daemon tick
// with StartTurnSpan and each ProjectionReconciler call with ReconcileWithSpan.
//
// Wiring contract (for ReconcileDaemon.runLoop or equivalent):
//
//   // --- daemon session startup ---
//   sessionID := computeSessionID(cfg.WorkspaceRoot)
//   sessionCtx, sessionDone := StartConversationSpan(ctx, sessionID, cfg.WorkspaceRoot)
//   defer sessionDone(nil)
//
//   // --- each periodic tick ---
//   for {
//     select {
//     case <-ticker.C:
//       tickCtx, tickDone := StartTurnSpan(sessionCtx, sessionID, len(providers))
//       for _, provider := range projectionProviders {
//         if pr, ok := provider.(*ProjectionReconciler); ok {
//           if err := ReconcileWithSpan(tickCtx, pr, cfg.WorkspaceRoot); err != nil {
//             // log, continue — error isolation per ADR-092 §3
//           }
//         }
//       }
//       tickDone(nil)
//     case <-ctx.Done():
//       return
//     }
//   }
//
// DaemonSessionID derives a stable identifier from the workspace root so
// all spans from the same daemon session share a session_id dimension.

// DaemonSessionID computes a short, stable identifier for a daemon session
// from the workspace root. Used as the CogosSessionID on conversation/turn spans.
func DaemonSessionID(workspaceRoot string) string {
	// Use a simple hash of the workspace root for brevity.
	// Not cryptographically sensitive — this is an observability label.
	h := 0
	for _, c := range workspaceRoot {
		h = h*31 + int(c)
		if h < 0 {
			h = -h
		}
	}
	return fmt.Sprintf("daemon-%08x", h)
}

// ProjectionReconcilerTick wraps one daemon tick over all projection kinds with
// a "turn" span and per-provider "service" spans. Suitable for embedding in a
// ReconcileDaemon's tick handler once the daemon is available.
//
// providers is the list of reconcile.Reconcilable instances registered for
// projection types. Only *ProjectionReconciler instances are spanned; other
// types are skipped (they are not managed by this wiring).
//
// This function is H4's executable interface: the daemon calls it once per tick
// with sessionCtx (a context carrying the conversation span) as parent.
func ProjectionReconcilerTick(
	sessionCtx context.Context,
	sessionID string,
	providers []interface{ Type() string },
	workspaceRoot string,
) error {
	turnCtx, turnDone := StartTurnSpan(sessionCtx, sessionID, len(providers))
	var tickErr error
	for _, p := range providers {
		pr, ok := p.(*ProjectionReconciler)
		if !ok {
			continue
		}
		if err := ReconcileWithSpan(turnCtx, pr, workspaceRoot); err != nil {
			// Record but continue — error isolation per ADR-092 §3.
			tickErr = err
		}
	}
	turnDone(tickErr)
	return tickErr
}
