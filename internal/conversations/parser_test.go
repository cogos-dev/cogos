// parser_test.go — direct, white-box tests of ParseSession's internal
// bookkeeping (meta.rawParents, meta.compactBoundaryFallback) that the
// higher-level provider/threads tests don't exercise in isolation.
package conversations

import (
	"strings"
	"testing"
)

// TestParseSession_CompactBoundaryFallback_HeadOfFileGuard is the #557
// round-5 review coverage fixture for the head-of-file guard at
// parser.go's `if priorTurnCount+turnIndex > 0 && lastUUID != ""` (in the
// compact_boundary branch of the rawParents recording). Two mutants
// survived the full package suite green before this test existed:
//
//  1. dropping the priorTurnCount term (`turnIndex > 0 && lastUUID != ""`)
//  2. dropping the count condition altogether (`if lastUUID != ""`)
//
// Both subtests call ParseSession directly and inspect
// meta.compactBoundaryFallback (unexported, but this file is in-package)
// rather than going through the full indexSession/PartitionThreads
// pipeline: whether that map gets an entry for a given compact_boundary
// uuid is the guard's entire observable effect, and asserting it directly
// pins down exactly the condition under review instead of depending on
// how far downstream code happens to propagate it.
func TestParseSession_CompactBoundaryFallback_HeadOfFileGuard(t *testing.T) {
	const sid = "aaaa1111-bbbb-2222-cccc-333344445555"

	// Kills mutant 1 (drops the priorTurnCount term): priorTurnCount=2
	// (simulating a session with 2 already-indexed turns from a PRIOR
	// incremental cycle) but this call's own local turnIndex is still 0 at
	// the point the compact_boundary line is reached — only a uuid-carrying
	// non-turn record (a plain type:"system" record) precedes it in THIS
	// call. priorTurnCount+turnIndex (2+0=2) is > 0, so the real guard
	// fires and records the fallback; a mutant that drops priorTurnCount
	// and checks only turnIndex>0 sees 0>0=false and does not.
	t.Run("priorTurnCount alone must satisfy the guard when local turnIndex is 0", func(t *testing.T) {
		meta := &SessionMeta{TurnCount: 2}
		lines := []string{
			makeSystemRecord("pre1", "", sid, "2026-07-01T10:00:00Z"),
			makeCompactBoundaryRecord("cb1", "ghost-absent-from-file", sid, "2026-07-01T10:00:01Z"),
		}
		r := strings.NewReader(strings.Join(lines, "\n"))
		if _, err := ParseSession(r, sid, 8192, meta, func(Turn) bool { return true }); err != nil {
			t.Fatalf("ParseSession: %v", err)
		}
		got, ok := meta.compactBoundaryFallback["cb1"]
		if !ok {
			t.Fatalf("compactBoundaryFallback[%q]: want an entry (priorTurnCount=2 makes this mid-file "+
				"even though local turnIndex=0), got none — meta.compactBoundaryFallback=%+v",
				"cb1", meta.compactBoundaryFallback)
		}
		if got != "pre1" {
			t.Errorf("compactBoundaryFallback[%q]: want %q (nearest preceding uuid-carrying record), got %q",
				"cb1", "pre1", got)
		}
	})

	// Kills mutant 2 (drops the count condition entirely): priorTurnCount=0
	// AND local turnIndex=0 — a genuine head-of-file compact_boundary (a
	// session resumed into a fresh file), preceded only by a uuid-carrying
	// non-turn record. The doc comment for compactBoundaryFallback is
	// explicit that this shape must be left alone: the true parent
	// legitimately lives in a different file this parse never sees, so
	// rooting here is correct. The real guard requires BOTH
	// priorTurnCount+turnIndex>0 AND lastUUID!="" — with the count term
	// dropped, `lastUUID != ""` alone would wrongly fire just because a
	// non-turn record happened to precede the boundary in this call.
	t.Run("lastUUID alone must not satisfy the guard at genuine file start", func(t *testing.T) {
		meta := &SessionMeta{} // TurnCount=0: priorTurnCount is 0
		lines := []string{
			makeSystemRecord("pre2", "", sid, "2026-07-01T10:00:00Z"),
			makeCompactBoundaryRecord("cb2", "ghost-absent-from-file-2", sid, "2026-07-01T10:00:01Z"),
		}
		r := strings.NewReader(strings.Join(lines, "\n"))
		if _, err := ParseSession(r, sid, 8192, meta, func(Turn) bool { return true }); err != nil {
			t.Fatalf("ParseSession: %v", err)
		}
		if got, ok := meta.compactBoundaryFallback["cb2"]; ok {
			t.Errorf("compactBoundaryFallback[%q]: want no entry (priorTurnCount=0 and local turnIndex=0 "+
				"is genuine file start — the boundary must be left rooted, not bridged to the preceding "+
				"non-turn record), got %q", "cb2", got)
		}
	})
}
