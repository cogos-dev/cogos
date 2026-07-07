// eval_sidecar_multiwriter_test.go — cross-process write-race regression
// tests for the eval package's two JSON state sidecars, both siblings of
// issue #449's _meta.json race flagged by cog-review on PR #458:
//
//   - eval-dispatch-triggers.json (fifth review pass, head 9d0aa2e):
//     writeDispatchTrigger (cog_run_experiment MCP handler) and
//     readAndClearDispatchTriggers (daemon eval ComputePlan cycle) both did a
//     plain os.WriteFile with no atomic tmp+rename and no cross-process lock.
//   - eval-baselines.json (sixth review pass, head c5726e2): writePinBaseline
//     (cog_pin_baseline MCP handler) had the identical unlocked, non-atomic
//     os.WriteFile read-modify-write; an earlier round of this PR incorrectly
//     judged it out of scope on the claim that no concurrent reader/writer
//     exists, which cog-review corrected — provider_impl.go's LoadConfig
//     reads this file every reconcile cycle, and concurrent cog_pin_baseline
//     calls race each other with no serialization at all.
package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// TestDispatchTriggers_ConcurrentWritesAllSurvive drives N goroutines each
// calling writeDispatchTrigger for a distinct experimentID against the same
// root concurrently, and asserts every trigger survives a single
// readAndClearDispatchTriggers call afterward — i.e. no interleaved writer
// silently dropped a peer's just-added entry.
func TestDispatchTriggers_ConcurrentWritesAllSurvive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	const numWriters = 50
	var wg sync.WaitGroup
	errs := make(chan error, numWriters)
	wg.Add(numWriters)
	for i := 0; i < numWriters; i++ {
		i := i
		go func() {
			defer wg.Done()
			expID := "exp-" + strconv.Itoa(i)
			if err := writeDispatchTrigger(dir, expID, i%2 == 0); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("writeDispatchTrigger error: %v", err)
	}

	triggers := readAndClearDispatchTriggers(dir)
	if len(triggers) != numWriters {
		missing := 0
		for i := 0; i < numWriters; i++ {
			if _, ok := triggers["exp-"+strconv.Itoa(i)]; !ok {
				missing++
			}
		}
		t.Fatalf("triggers count = %d, want %d (missing %d) — concurrent writers clobbered each other", len(triggers), numWriters, missing)
	}
	for i := 0; i < numWriters; i++ {
		expID := "exp-" + strconv.Itoa(i)
		want := i%2 == 0
		if got, ok := triggers[expID]; !ok {
			t.Errorf("%s missing from triggers", expID)
		} else if got != want {
			t.Errorf("%s force = %v, want %v", expID, got, want)
		}
	}

	// A second read must see no pending triggers — the clear must have won.
	if t2 := readAndClearDispatchTriggers(dir); len(t2) != 0 {
		t.Errorf("expected triggers cleared after read, got %v", t2)
	}
}

// TestDispatchTriggers_WriteDuringClearDoesNotLoseEitherSide races a single
// writeDispatchTrigger call against a concurrent readAndClearDispatchTriggers
// call for a *different* experimentID already on disk, and asserts that
// whichever interleaving occurs, the final on-disk state is exactly one of
// two well-defined outcomes — never a torn/corrupt file, and never a silent
// merge that loses the write while also failing to have cleared the
// pre-existing entry.
func TestDispatchTriggers_WriteDuringClearDoesNotLoseEitherSide(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Seed one pre-existing trigger that the "clear" side will consume.
	if err := writeDispatchTrigger(dir, "pre-existing", false); err != nil {
		t.Fatalf("seed writeDispatchTrigger: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var clearResult map[string]bool
	go func() {
		defer wg.Done()
		clearResult = readAndClearDispatchTriggers(dir)
	}()
	go func() {
		defer wg.Done()
		_ = writeDispatchTrigger(dir, "new-trigger", true)
	}()
	wg.Wait()

	// The clear call must have seen the pre-existing trigger (it ran either
	// before or after the concurrent write, but the write only ever adds
	// "new-trigger", never removes "pre-existing").
	if _, ok := clearResult["pre-existing"]; !ok {
		t.Errorf("readAndClearDispatchTriggers did not observe pre-existing trigger: %v", clearResult)
	}

	// Whatever remains on disk after both calls settle must be exactly
	// {"new-trigger": true} if the write landed after the clear, or empty if
	// the write landed before the clear (and was itself cleared). It must
	// never silently contain "pre-existing" again (the lock ensures the
	// write always operates on a post-clear read, not a stale pre-clear
	// snapshot) and it must be well-formed.
	final := readAndClearDispatchTriggers(dir)
	if _, ok := final["pre-existing"]; ok {
		t.Errorf("pre-existing trigger resurrected after clear: %v", final)
	}
	if len(final) > 1 {
		t.Errorf("unexpected extra triggers remained: %v", final)
	}
}

// TestWritePinBaseline_ConcurrentWritesAllSurvive drives N goroutines each
// calling writePinBaseline for a distinct experimentID against the same root
// concurrently, and asserts every pin survives on disk — i.e. no interleaved
// writer silently dropped a peer's just-written pin (the race cog-review's
// sixth review pass confirmed was live: two concurrent cog_pin_baseline MCP
// calls with no serialization).
func TestWritePinBaseline_ConcurrentWritesAllSurvive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	const numWriters = 50
	var wg sync.WaitGroup
	errs := make(chan error, numWriters)
	wg.Add(numWriters)
	for i := 0; i < numWriters; i++ {
		i := i
		go func() {
			defer wg.Done()
			expID := "exp-" + strconv.Itoa(i)
			runID := "run-" + strconv.Itoa(i)
			if err := writePinBaseline(dir, expID, runID); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("writePinBaseline error: %v", err)
	}

	pinsPath := filepath.Join(dir, ".cog", "state", "eval-baselines.json")
	data, err := os.ReadFile(pinsPath)
	if err != nil {
		t.Fatalf("read pins file: %v", err)
	}
	var pins map[string]string
	if err := json.Unmarshal(data, &pins); err != nil {
		t.Fatalf("pins file is not valid JSON after concurrent writers: %v\ncontent: %s", err, data)
	}

	if len(pins) != numWriters {
		missing := 0
		for i := 0; i < numWriters; i++ {
			if _, ok := pins["exp-"+strconv.Itoa(i)]; !ok {
				missing++
			}
		}
		t.Fatalf("pins count = %d, want %d (missing %d) — concurrent writers clobbered each other", len(pins), numWriters, missing)
	}
	for i := 0; i < numWriters; i++ {
		expID := "exp-" + strconv.Itoa(i)
		want := "run-" + strconv.Itoa(i)
		if got := pins[expID]; got != want {
			t.Errorf("%s = %q, want %q", expID, got, want)
		}
	}
}
