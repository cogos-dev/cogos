// outcome.go — the provenance result channel from the detached updater back to
// the daemon.
//
// WHY THIS FILE EXISTS.
//
// ApplyPlan spawns a detached `cogos self-update` and then never learns what
// happened to it. On a successful update that is fine: the daemon is restarted
// by the child and the new process observes its own new version. On a REFUSAL
// it is not fine at all. The provider sets Health=Progressing "updating to
// <tag>", the watchdog clears inProgress a few minutes later, the next cycle
// spawns another updater that refuses for the same reason, and the operator's
// dashboard shows a node that is perpetually updating and never arrives.
//
// That is the worst possible presentation of the one signal this whole change
// exists to produce. A provenance refusal is either a real supply-chain attack
// or a broken pipeline, and in both cases it must be loud on the surfaces the
// operator actually watches — not buried in a log file in the run directory.
//
// So the child writes its terminal provenance result here, and FetchLive reads
// it on the next cycle and projects it into Health and into the reconcile
// state's attributes. The file is small, atomically replaced, and always
// describes exactly one tag: the last one the updater made a decision about.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Provenance result codes. These are stable strings: they appear in
// kernel state attributes and may be matched by external tooling.
const (
	// ProvenanceOK — signature verified against the pinned identity.
	ProvenanceOK = "ok"
	// ProvenanceUnsigned — the release carries no signature material at all.
	ProvenanceUnsigned = "unsigned"
	// ProvenanceInvalid — signature material was present and failed a check.
	// This is the code that means "attack or broken pipeline".
	ProvenanceInvalid = "invalid"
	// ProvenanceTransport — the signature material could not be fetched.
	// Says nothing about authenticity; retryable.
	ProvenanceTransport = "transport_error"
	// ProvenanceSkipped — verification disabled by require_signature: off.
	ProvenanceSkipped = "skipped"
)

// ProvenanceOutcome is the detached updater's terminal verdict on one tag.
type ProvenanceOutcome struct {
	Tag     string `json:"tag"`
	Result  string `json:"result"`
	Mode    string `json:"mode"`    // the require_signature posture in force
	Blocked bool   `json:"blocked"` // true when the update was refused because of this
	Message string `json:"message"`
	At      string `json:"at"` // RFC3339 UTC
}

// provenanceOutcomePath returns the outcome file for a workspace root.
func provenanceOutcomePath(root string) string {
	return filepath.Join(root, ".cog", "run", "selfupdate-provenance.json")
}

// WriteProvenanceOutcome records the updater's verdict, replacing any previous
// one atomically.
//
// Errors are returned but callers should not treat them as fatal: failing to
// record an outcome must never turn an otherwise-successful update into a
// failure, nor turn a refusal into an apply. The write is observability, not
// control flow.
func WriteProvenanceOutcome(root string, o ProvenanceOutcome) error {
	if root == "" {
		return nil
	}
	if o.At == "" {
		o.At = time.Now().UTC().Format(time.RFC3339)
	}
	path := provenanceOutcomePath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("recording provenance outcome: %w", err)
	}
	return nil
}

// ReadProvenanceOutcome returns the last recorded verdict, or nil when none has
// been written. A corrupt file is reported as absent rather than as an error:
// the caller's only use for it is to enrich status, and a parse failure must not
// break a reconcile cycle.
func ReadProvenanceOutcome(root string) *ProvenanceOutcome {
	if root == "" {
		return nil
	}
	data, err := os.ReadFile(provenanceOutcomePath(root))
	if err != nil {
		return nil
	}
	var o ProvenanceOutcome
	if err := json.Unmarshal(data, &o); err != nil {
		return nil
	}
	if o.Tag == "" || o.Result == "" {
		return nil
	}
	return &o
}
