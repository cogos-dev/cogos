// Package vllm implements the CogOS vLLM inference provider with direct
// PagedAttention block-API access (RFC-0006).
//
// # Architecture
//
// This package exposes vLLM's content-addressed KV cache as first-class kernel
// objects: KV blocks become cache.kv_block CogBlocks; the cache layer is managed
// as a Reconcilable resource; block-level events flow on the kernel bus.
//
// # v0.5.0 scope
//
// v0.5.0 ships scaffolding and mock-based tests. Hardware integration (cgo binding
// to vllm.core.block_manager, real CUDA device) is deferred and tagged requires-cuda.
//
// # Access shape
//
// Primary: cgo binding to the vLLM Python runtime. All cgo callouts are wrapped in
// the vllmCall helper (see provider_vllm.go) which recovers panics and converts them
// to typed Go errors.
//
// Fallback: Python sidecar over Unix domain socket IPC. If cgo proves brittle
// (GIL contention, version skew), the sidecar provides equivalent semantics over
// a JSON-lines protocol. The VLLMClient interface is satisfied by both.
//
// See README-vllm.md in this directory for hardware requirements.
package vllm
