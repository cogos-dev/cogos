package vitalsretention

import (
	"os"
	"regexp"
	"strings"
	"sync"
)

// NodeKeySource resolves the identity used to key vitals files on disk.
//
// RFC-036 §11.1 settles the LAYERING question: host vitals are physical
// facts about the machine, so S2 keys files on L1 (constellation NodeID)
// under the shared L2 workspace — that's what lets Eclipse's pulse
// BEP-sync to Darkstar and a fleet view fall out of the file layout. What
// is NOT settled is the concrete *value*: the kernel's node_id storage
// location and field name disagree today (Seam B, cogos PR #474), pending
// the operator's ruling. Per the RFC's 2026-07-29 acceptance note ("implement
// file naming behind an interface until Seam B settles the concrete node_id
// value"), this interface is the seam — swapping the key source once #474
// lands is a call to SetNodeKeySource, not a rewrite of recorder.go,
// store.go, compact.go, or window.go, and NOT a migration of already-written
// files (whatever key was in effect when a file was created stays its key;
// only new writes pick up a changed source).
type NodeKeySource interface {
	// NodeKey returns the current node identity string. Implementations
	// should be cheap (called on every recorded tick) and must never panic;
	// return "" on failure and the caller falls back to a safe default.
	NodeKey() string
}

// NodeKeyFunc adapts a plain function to NodeKeySource.
type NodeKeyFunc func() string

// NodeKey implements NodeKeySource.
func (f NodeKeyFunc) NodeKey() string { return f() }

// defaultNodeKeySource falls back to os.Hostname() when nothing better has
// been wired. This is explicitly NOT the RFC-036 L1 NodeID — constellation
// identity resolution lives elsewhere and is exactly the thing PR #474 is
// still reconciling. It is a stable, good-enough-for-one-machine key so S2
// can ship without blocking on #474 (RFC-040 N4/acceptance note).
var defaultNodeKeySource NodeKeySource = NodeKeyFunc(hostnameNodeKey)

func hostnameNodeKey() string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return "unknown-node"
	}
	return h
}

var (
	nodeKeySourceMu sync.RWMutex
	nodeKeySource   NodeKeySource = defaultNodeKeySource
)

// SetNodeKeySource overrides the node-identity source used to key newly
// recorded files. Call this once cogos #474 settles the concrete node_id
// value/location — the intended swap-in point this seam exists for. Passing
// nil restores the hostname-based default (mainly for tests).
func SetNodeKeySource(src NodeKeySource) {
	nodeKeySourceMu.Lock()
	defer nodeKeySourceMu.Unlock()
	if src == nil {
		src = defaultNodeKeySource
	}
	nodeKeySource = src
}

// currentNodeKey resolves and filesystem-sanitizes the active node key.
func currentNodeKey() string {
	nodeKeySourceMu.RLock()
	src := nodeKeySource
	nodeKeySourceMu.RUnlock()

	key := ""
	if src != nil {
		key = src.NodeKey()
	}
	if strings.TrimSpace(key) == "" {
		key = hostnameNodeKey()
	}
	return sanitizeNodeKey(key)
}

// nodeKeyUnsafeChars matches anything that isn't safe as a single path
// segment across macOS/Linux/Windows filesystems.
var nodeKeyUnsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeNodeKey collapses an arbitrary identity string into one
// filesystem-safe path segment so a node key can never traverse directories
// or collide with reserved names.
func sanitizeNodeKey(s string) string {
	s = strings.TrimSpace(s)
	s = nodeKeyUnsafeChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if s == "" {
		return "unknown-node"
	}
	return s
}
