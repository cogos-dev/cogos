// kvblock_hash_provider.go — KVBlockHashProvider interface for fork integration.
//
// KVBlockHashProvider is the cross-RFC integration point between the vLLM
// provider (RFC-0006, this package) and the cog_fork_session fork handler
// (RFC-0005). The interface text is identical in both RFCs; the vLLM provider
// is the implementing side; the fork handler is the consuming side.
//
// When RFC-0005's fork handler lands, it will query KVBlockHashProvider to
// obtain the parent session's KV block hash for inheritance. In v0.5.0, this
// stub carries the hash through but emits a log line rather than routing to
// the same vLLM channel (see ForkOverKVCacheStub in provider_vllm_fork.go).
package vllm

import "context"

// KVBlockHashProvider is implemented by inference providers that expose
// a content-addressed KV-block layer (e.g., vLLM PagedAttention).
// The fork-session handler queries this when forking over a kvcache layer
// to obtain the parent's KV block hash.
type KVBlockHashProvider interface {
	// ParentKVBlockHash returns the hash of the KV block representing
	// the session's KV state at the given message ID, or an error if
	// the block is no longer addressable (evicted, runtime restarted).
	ParentKVBlockHash(ctx context.Context, sessionID string, atMessageID string) (BlockHash, error)
}

// ErrKVBlockEvicted is returned by ParentKVBlockHash when the block has been
// evicted from the vLLM block manager. The fork handler degrades gracefully
// to a cold start when this error is received.
type ErrKVBlockEvicted struct {
	SessionID   string
	AtMessageID string
}

func (e *ErrKVBlockEvicted) Error() string {
	return "vllm: KV block evicted for session " + e.SessionID + " at message " + e.AtMessageID
}

// ErrKVBlockRuntimeRestarted is returned by ParentKVBlockHash when the vLLM
// runtime has restarted and block state is no longer addressable.
type ErrKVBlockRuntimeRestarted struct {
	ChannelID string
}

func (e *ErrKVBlockRuntimeRestarted) Error() string {
	return "vllm: runtime restarted on channel " + e.ChannelID + "; KV block state lost"
}
