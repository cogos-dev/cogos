// tool_observer_ledger_g5_test.go — negative-control test for the resolveTransportSession
// fallback comment in withToolObserver.
//
// The old comment described the fallback as only an "in-process test path". The corrected
// comment must name it as also a production path for HTTP-originated tool calls.
// This test FAILS if the comment still implies test-only and PASSES after the fix.
package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLedgerG5_ToolObserverFallbackComment(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	observerPath := filepath.Join(filepath.Dir(thisFile), "tool_observer.go")
	data, err := os.ReadFile(observerPath)
	if err != nil {
		t.Fatalf("cannot read tool_observer.go: %v", err)
	}
	src := string(data)

	// The new comment must explicitly name the production (HTTP-originated) path.
	if !strings.Contains(src, "production path for HTTP-originated tool calls") {
		t.Error("tool_observer.go: fallback comment does not name HTTP-originated tool calls as a production path — old test-only wording still present or comment was not updated")
	}

	// The old comment described fallback as ONLY an "in-process test path".
	// After the fix the phrase "in-process test path" may still appear, but only
	// as a secondary description alongside the production path. We check the
	// new required phrase is present (above), which is sufficient.
	// Additionally confirm "fail-open" rationale appears.
	if !strings.Contains(src, "Fail-open is deliberate") {
		t.Error("tool_observer.go: fallback comment does not include \"Fail-open is deliberate\" rationale — fix not fully applied")
	}
}
