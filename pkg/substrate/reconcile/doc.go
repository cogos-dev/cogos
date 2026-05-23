// Package reconcile provides the plan/apply reconciliation framework for CogOS.
//
// Reconciliation is the core control-plane pattern: providers declare desired
// state, the reconciler diffs against actual state, produces a plan of changes,
// and applies them idempotently. This is the same Terraform-style loop used
// throughout the CogOS kernel, extracted as an importable library.
//
// The Reconcilable interface is the central contract: any provider that
// implements its seven methods (Type, LoadConfig, FetchLive, ComputePlan,
// ApplyPlan, BuildState, Health) gets plan/apply/status/refresh for free
// through the generic orchestration layer.
//
// # Architecture
//
// Types and interfaces define the contract (types.go). State management
// handles persistence with lineage tracking (state.go). The registry maps
// provider names to implementations (registry.go). Events provide lifecycle
// observability (events.go). The meta-reconciler orchestrates multi-provider
// reconciliation with dependency resolution via Kahn's topological sort (meta.go).
package reconcile
