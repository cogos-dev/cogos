// provider_vllm.go — vLLM inference Provider + cgo failure-isolation wrapper.
//
// Implements engine.Provider for the vLLM backend. The Provider wraps a
// VLLMClient (cgo or Python-sidecar) and routes CompletionRequests through
// vLLM's OpenAI-compat API layer.
//
// All cgo callouts into the vLLM Python runtime are wrapped via vllmCall,
// which converts panics (and Go-recoverable SIGSEGV via SetPanicOnFault) into
// typed Go errors so that cgo failure never propagates to crash the kernel.
//
// v0.5.0: scaffolding only. Complete() and Stream() return ErrVLLMNotAvailable
// until Linux/CUDA hardware integration lands. Hardware paths are marked
// TODO(rfc-0006/requires-cuda).
package vllm

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/myrgic/cogos/internal/engine"
)

// ErrVLLMNotAvailable is returned when the vLLM runtime is not reachable or
// hardware integration is not yet complete (scaffolding phase).
var ErrVLLMNotAvailable = errors.New("vllm: provider not available (requires-cuda; see RFC-0006)")

// Provider implements engine.Provider for the vLLM backend.
// Construct via NewProvider; inject a VLLMClient for tests.
//
// Thread safety: Provider is safe to call concurrently. The embedded client
// must also be safe for concurrent use.
type Provider struct {
	client    VLLMClient
	channelID string
	modelName string
}

// NewProvider constructs a Provider with the given client and configuration.
// Pass a MockVLLMClient for unit tests; the cgo binding for production
// (requires Linux/CUDA — see README-vllm.md).
func NewProvider(client VLLMClient, channelID, modelName string) *Provider {
	return &Provider{
		client:    client,
		channelID: channelID,
		modelName: modelName,
	}
}

// Name implements engine.Provider.
func (p *Provider) Name() string { return "vllm" }

// Model implements engine.Provider.
func (p *Provider) Model() string { return p.modelName }

// Complete implements engine.Provider.
// TODO(rfc-0006/requires-cuda): real implementation routes through vLLM cgo binding.
func (p *Provider) Complete(ctx context.Context, req *engine.CompletionRequest) (*engine.CompletionResponse, error) {
	return nil, ErrVLLMNotAvailable
}

// Stream implements engine.Provider.
// TODO(rfc-0006/requires-cuda): real implementation routes through vLLM cgo binding.
func (p *Provider) Stream(ctx context.Context, req *engine.CompletionRequest) (<-chan engine.StreamChunk, error) {
	return nil, ErrVLLMNotAvailable
}

// Available implements engine.Provider.
// Returns false in v0.5.0 scaffolding; real check requires cgo binding.
func (p *Provider) Available(ctx context.Context) bool {
	// TODO(rfc-0006/requires-cuda): probe vLLM health endpoint.
	return false
}

// Capabilities implements engine.Provider.
func (p *Provider) Capabilities() engine.ProviderCapabilities {
	return engine.ProviderCapabilities{
		Capabilities: []engine.Capability{
			engine.CapStreaming,
			engine.CapToolUse,
			engine.CapJSON,
		},
		MaxContextTokens:   128_000,
		MaxOutputTokens:    4_096,
		IsLocal:            true,
		CostPerInputToken:  0,
		CostPerOutputToken: 0,
	}
}

// Ping implements engine.Provider.
// TODO(rfc-0006/requires-cuda): probe real vLLM health endpoint.
func (p *Provider) Ping(ctx context.Context) (time.Duration, error) {
	return 0, ErrVLLMNotAvailable
}

// ParentKVBlockHash implements KVBlockHashProvider so the fork handler
// (RFC-0005) can obtain the parent session's KV block hash.
func (p *Provider) ParentKVBlockHash(ctx context.Context, sessionID string, atMessageID string) (BlockHash, error) {
	var hash BlockHash
	err := vllmCall(func() error {
		// TODO(rfc-0006/requires-cuda): query vllm.core.block_manager for block
		// covering the message's token range. In scaffolding, derive hash from
		// the messageID as prompt key using the standard equivalence tuple.
		h := p.client.BlockHashForPrompt(atMessageID, p.modelName, KVSamplingConfig{})
		hash = BlockHash(h)
		return nil
	})
	return hash, err
}

// ── cgo failure-isolation wrapper ──────────────────────────────────────────
//
// All cgo callouts into the vLLM Python runtime must be wrapped in vllmCall.
// It installs SetPanicOnFault(true) so that hardware-fault SIGSEGVs from the
// C/Python layer are converted to Go panics, then recovers those panics and
// converts them to typed errors.
//
// If SIGSEGV cannot be recovered at Go's runtime level (some C library
// failures bypass the panic-handler), the Python-sidecar IPC fallback path
// should be used instead; the cgo path is then opt-in for trusted vLLM versions.

// vllmCall wraps a cgo callout in deferred recover + runtime.SetPanicOnFault.
// Any panic (including recovered SIGSEGV) is converted to a typed error.
// The returned error is never nil when a panic occurs; fn's own errors pass through.
//
// Usage:
//
//	err := vllmCall(func() error {
//	    return cgoBindingFunction(...)
//	})
func vllmCall(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("vllm cgo failure (recovered): %v", r)
		}
	}()
	debug.SetPanicOnFault(true)
	defer debug.SetPanicOnFault(false)
	return fn()
}
