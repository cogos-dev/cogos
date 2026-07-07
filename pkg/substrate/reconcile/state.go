// state.go
// Generic state management for the reconciliation framework.
// Provides load/write/lineage operations for any resource provider.
//
// Each provider stores state at .cog/config/{resource_type}/.state.json
// using the State format from types.go.
//
// LoadState and WriteState are individually safe (WriteState is atomic:
// tmp + rename), but the reconcile cycle that uses them —
// LoadState → ComputePlan → ApplyPlan → BuildState → WriteState, run by both
// `cogos reconcile <type>` (internal/engine/cli_reconcile.go) and the
// daemon's own periodic reconcile loop (internal/engine/reconcile_daemon.go)
// against the identical un-namespaced state file — is a read-modify-write
// cycle with no cross-process coordination. BuildState derives the entire
// new Resources slice fresh from live state plus the existing state's
// Serial/Lineage (it does not apply a delta on top of a re-read of disk), so
// unlike the conversations index's _meta.json fix (issue #449, per-session
// delta merge), there is no sound delta-merge here: two concurrent cycles'
// WriteState calls race on a plain last-write-wins tmp+rename, and whichever
// lands last silently discards the other cycle's Serial/Resources.
//
// Callers that run a full LoadState→WriteState cycle for a given
// resourceType must wrap it in AcquireStateLock/Release (see doc comment)
// to serialize concurrent cycles for the same resourceType across processes.
package reconcile

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/myrgic/cogos/pkg/filelock"
)

// StateLockTimeout bounds how long AcquireStateLock waits to acquire the
// cross-process lock before giving up. Mirrors metaLockTimeout in
// internal/conversations/index.go (same bug class, same timeout budget).
const StateLockTimeout = 5 * time.Second

// StatePath returns the path to a provider's state file.
func StatePath(root, resourceType string) string {
	return filepath.Join(root, ".cog", "config", resourceType, ".state.json")
}

// StateLockPath returns the path to the advisory cross-process lock file
// guarding a resource type's LoadState→WriteState reconcile cycle. A
// sibling file, not the state file itself, so the lock's own lifecycle never
// touches the JSON content file LoadState reads.
func StateLockPath(root, resourceType string) string {
	return StatePath(root, resourceType) + ".lock"
}

// AcquireStateLock takes the cross-process advisory lock for resourceType's
// reconcile cycle. Callers must run their entire
// LoadState → ComputePlan → ApplyPlan → BuildState → WriteState cycle while
// holding the returned lock, then Release it — this is what serializes a
// CLI-invoked `cogos reconcile <type>` run against the daemon's own
// reconcile-loop cycle for the same resourceType, closing the same bug class
// issue #449 fixed for the conversations index's _meta.json.
//
//	lock, err := reconcile.AcquireStateLock(root, resourceType)
//	if err != nil {
//	    return err
//	}
//	defer lock.Release()
//	state, _ := reconcile.LoadState(root, resourceType)
//	... ComputePlan / ApplyPlan / BuildState ...
//	reconcile.WriteState(root, resourceType, newState)
func AcquireStateLock(root, resourceType string) (*filelock.FileLock, error) {
	lockPath := StateLockPath(root, resourceType)
	// The lock must be acquirable before the state file (and its directory)
	// necessarily exist — the very first reconcile cycle for a resourceType
	// has no .state.json yet. WriteState creates the directory too, but that
	// happens after LoadState/lock-acquire in the caller's cycle, so ensure
	// it here rather than requiring every caller to mkdir before locking.
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return nil, fmt.Errorf("creating state dir for %s lock: %w", resourceType, err)
	}
	return filelock.Acquire(lockPath, StateLockTimeout)
}

// LoadState loads the state file for a given resource type.
// Returns nil, nil if no state file exists yet.
func LoadState(root, resourceType string) (*State, error) {
	data, err := os.ReadFile(StatePath(root, resourceType))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s state: %w", resourceType, err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing %s state: %w", resourceType, err)
	}
	return &state, nil
}

// WriteState atomically writes the state file for a resource type.
// Increments serial and sets generated_at timestamp automatically.
func WriteState(root, resourceType string, state *State) error {
	state.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	state.Serial++
	state.ResourceType = resourceType

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s state: %w", resourceType, err)
	}

	sp := StatePath(root, resourceType)

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(sp), 0755); err != nil {
		return fmt.Errorf("creating state dir for %s: %w", resourceType, err)
	}

	// Atomic write: tmp file + rename
	tmp := sp + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("writing tmp %s state: %w", resourceType, err)
	}
	if err := os.Rename(tmp, sp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming %s state: %w", resourceType, err)
	}
	return nil
}

// NewState creates a fresh state with a new lineage.
func NewState(resourceType string) *State {
	return &State{
		Version:      1,
		Lineage:      GenerateLineage(),
		Serial:       0,
		ResourceType: resourceType,
		Resources:    []Resource{},
	}
}

// GenerateLineage creates a random hex string for state lineage tracking.
func GenerateLineage() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
