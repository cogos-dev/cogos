// threads_test.go — unit tests for PartitionThreads (threads.go).
//
// Case (2) below (isSidechain fresh-root chain) is grounded in the verified
// on-disk mechanism from the #557 plan: a subagent transcript's first record
// carries parentUuid:null and isSidechain:true. Case (3) (branch point, two
// children sharing one parentUuid) is SYNTHETIC ONLY — no real fixture for a
// genuine branch was found in the operator's corpus during planning; see the
// #557 plan's risks section.
package conversations

import (
	"testing"
	"time"
)

func mkTurn(uuid, parentUUID string, turnIndex int, sidechain bool, ts time.Time) Turn {
	return Turn{
		UUID:        uuid,
		SessionID:   "sess",
		TurnIndex:   turnIndex,
		Role:        RoleUser,
		Timestamp:   ts,
		Text:        "text",
		ParentUUID:  parentUUID,
		IsSidechain: sidechain,
	}
}

// TestPartitionThreads_SingleLinearRoot: a plain linear session has exactly
// one thread, role=main, MessageCount equal to the turn count.
func TestPartitionThreads_SingleLinearRoot(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	turns := []Turn{
		mkTurn("u1", "", 0, false, now),
		mkTurn("a1", "u1", 1, false, now.Add(time.Minute)),
		mkTurn("u2", "a1", 2, false, now.Add(2*time.Minute)),
	}

	threads := PartitionThreads(turns, nil)

	if len(threads) != 1 {
		t.Fatalf("want 1 thread, got %d: %+v", len(threads), threads)
	}
	tm := threads[0]
	if tm.Role != ThreadRoleMain {
		t.Errorf("Role: want main, got %q", tm.Role)
	}
	if tm.MessageCount != len(turns) {
		t.Errorf("MessageCount: want %d, got %d", len(turns), tm.MessageCount)
	}
	if tm.ThreadID != "u1" {
		t.Errorf("ThreadID: want u1 (root uuid), got %q", tm.ThreadID)
	}
	if tm.FirstUUID != "u1" {
		t.Errorf("FirstUUID: want u1, got %q", tm.FirstUUID)
	}
	if tm.LastUUID != "u2" {
		t.Errorf("LastUUID: want u2, got %q", tm.LastUUID)
	}
	for _, tu := range turns {
		if tu.ThreadID != "u1" {
			t.Errorf("turn %s: ThreadID want u1, got %q", tu.UUID, tu.ThreadID)
		}
	}
}

// TestPartitionThreads_SubagentSidechain: a linear main thread plus a
// subagent sidechain chain (fresh root, parentUuid:null, isSidechain:true,
// appended after the main thread's turns — the observed on-disk ordering,
// since the subagent file is a separate later-discovered source) produces
// two threads: the first thread's role stays main and is unaffected by the
// second; the second is subagent-sidechain.
func TestPartitionThreads_SubagentSidechain(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	turns := []Turn{
		mkTurn("u1", "", 0, false, now),
		mkTurn("a1", "u1", 1, false, now.Add(time.Minute)),
		// Subagent sidechain: fresh root (ParentUUID empty), isSidechain true.
		mkTurn("sub-u1", "", 2, true, now.Add(2*time.Minute)),
		mkTurn("sub-a1", "sub-u1", 3, true, now.Add(3*time.Minute)),
	}

	threads := PartitionThreads(turns, nil)

	if len(threads) != 2 {
		t.Fatalf("want 2 threads, got %d: %+v", len(threads), threads)
	}

	var main, sidechain *ThreadMeta
	for i := range threads {
		switch threads[i].Role {
		case ThreadRoleMain:
			main = &threads[i]
		case ThreadRoleSubagentSidechain:
			sidechain = &threads[i]
		}
	}
	if main == nil {
		t.Fatalf("no main thread found: %+v", threads)
	}
	if sidechain == nil {
		t.Fatalf("no subagent-sidechain thread found: %+v", threads)
	}
	if main.ThreadID != "u1" {
		t.Errorf("main ThreadID: want u1, got %q", main.ThreadID)
	}
	if main.MessageCount != 2 {
		t.Errorf("main MessageCount: want 2, got %d", main.MessageCount)
	}
	if sidechain.ThreadID != "sub-u1" {
		t.Errorf("sidechain ThreadID: want sub-u1, got %q", sidechain.ThreadID)
	}
	if sidechain.MessageCount != 2 {
		t.Errorf("sidechain MessageCount: want 2, got %d", sidechain.MessageCount)
	}

	// Per-turn ThreadID assignment.
	byUUID := map[string]string{}
	for _, tu := range turns {
		byUUID[tu.UUID] = tu.ThreadID
	}
	if byUUID["u1"] != "u1" || byUUID["a1"] != "u1" {
		t.Errorf("main turns should carry ThreadID u1, got u1=%q a1=%q", byUUID["u1"], byUUID["a1"])
	}
	if byUUID["sub-u1"] != "sub-u1" || byUUID["sub-a1"] != "sub-u1" {
		t.Errorf("sidechain turns should carry ThreadID sub-u1, got sub-u1=%q sub-a1=%q", byUUID["sub-u1"], byUUID["sub-a1"])
	}
}

// TestPartitionThreads_BranchPoint is SYNTHETIC ONLY: two turns sharing one
// parentUuid (a genuine branch/fork) was never observed in real data during
// the #557 plan's investigation. This asserts the algorithm's documented
// behavior for that case (first child by turn order continues the parent's
// thread, the second starts its own) without claiming it matches verified
// on-disk CC semantics.
func TestPartitionThreads_BranchPoint(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	turns := []Turn{
		mkTurn("root", "", 0, false, now),
		// Two children of "root" — a synthetic branch point.
		mkTurn("child-a", "root", 1, false, now.Add(time.Minute)),
		mkTurn("child-b", "root", 2, false, now.Add(2*time.Minute)),
		mkTurn("grandchild-b", "child-b", 3, false, now.Add(3*time.Minute)),
	}

	threads := PartitionThreads(turns, nil)

	if len(threads) != 2 {
		t.Fatalf("want 2 threads (branch point), got %d: %+v", len(threads), threads)
	}

	byUUID := map[string]string{}
	for _, tu := range turns {
		byUUID[tu.UUID] = tu.ThreadID
	}
	// root + first child continue the same thread.
	if byUUID["root"] != "root" || byUUID["child-a"] != "root" {
		t.Errorf("root/child-a should share ThreadID root, got root=%q child-a=%q", byUUID["root"], byUUID["child-a"])
	}
	// second child starts its own thread, and its own descendant follows it.
	if byUUID["child-b"] != "child-b" {
		t.Errorf("child-b should be its own thread root, got ThreadID %q", byUUID["child-b"])
	}
	if byUUID["grandchild-b"] != "child-b" {
		t.Errorf("grandchild-b should continue child-b's thread, got ThreadID %q", byUUID["grandchild-b"])
	}

	// The thread containing turn_index 0 (root) is main; the branch thread is
	// unknown-fork (no isSidechain marker anywhere in it).
	for _, tm := range threads {
		switch tm.ThreadID {
		case "root":
			if tm.Role != ThreadRoleMain {
				t.Errorf("root thread Role: want main, got %q", tm.Role)
			}
			if tm.MessageCount != 2 {
				t.Errorf("root thread MessageCount: want 2, got %d", tm.MessageCount)
			}
		case "child-b":
			if tm.Role != ThreadRoleUnknownFork {
				t.Errorf("child-b thread Role: want unknown-fork, got %q", tm.Role)
			}
			if tm.MessageCount != 2 {
				t.Errorf("child-b thread MessageCount: want 2, got %d", tm.MessageCount)
			}
		default:
			t.Errorf("unexpected thread id %q", tm.ThreadID)
		}
	}
}

// TestPartitionThreads_SidechainRoleDerivesFromRootOnly: a and c are plain
// (non-sidechain) turns continuing the same DAG chain through b, which is a
// non-root member carrying IsSidechain:true. Role must come from the
// thread's ROOT (a), not from any member (b) — otherwise a single
// isSidechain turn anywhere downstream reclassifies the whole thread,
// including turn_index 0, leaving no thread with role=main at all.
func TestPartitionThreads_SidechainRoleDerivesFromRootOnly(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	turns := []Turn{
		mkTurn("a", "", 0, false, now),
		mkTurn("b", "a", 1, true, now.Add(time.Minute)), // non-root, IsSidechain
		mkTurn("c", "b", 2, false, now.Add(2*time.Minute)),
	}

	threads := PartitionThreads(turns, nil)

	if len(threads) != 1 {
		t.Fatalf("want 1 thread, got %d: %+v", len(threads), threads)
	}
	tm := threads[0]
	if tm.Role != ThreadRoleMain {
		t.Errorf("Role: want main (root a is not IsSidechain), got %q", tm.Role)
	}
	if tm.MessageCount != 3 {
		t.Errorf("MessageCount: want 3, got %d", tm.MessageCount)
	}
}

// TestBuildThreadMeta_CycleGetsNonEmptyFirstUUID: a cycle in the parentUuid
// graph (a's parent is b, b's parent is a) means no member is ever marked
// isRoot, so the normal FirstUUID assignment never fires. FirstUUID must
// still come back non-empty (falling back to ThreadID) rather than shipping
// an empty string that looks like a computed-but-blank value.
func TestBuildThreadMeta_CycleGetsNonEmptyFirstUUID(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	turns := []Turn{
		mkTurn("a", "b", 0, false, now),
		mkTurn("b", "a", 1, false, now.Add(time.Minute)),
	}

	threads := PartitionThreads(turns, nil)

	if len(threads) != 2 {
		t.Fatalf("want 2 threads (cycle fallback: each member its own thread), got %d: %+v", len(threads), threads)
	}
	for _, tm := range threads {
		if tm.FirstUUID == "" {
			t.Errorf("thread %q: FirstUUID must not be empty, even for a cycle", tm.ThreadID)
		}
		if tm.FirstUUID != tm.ThreadID {
			t.Errorf("thread %q: FirstUUID fallback should equal ThreadID, got %q", tm.ThreadID, tm.FirstUUID)
		}
	}
}

// TestPartitionThreads_Degenerate exercises degenerate input that must not
// panic: zero turns, and a set of all-orphan turns (each has a ParentUUID
// pointing outside the set, or no UUID at all).
func TestPartitionThreads_Degenerate(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		threads := PartitionThreads(nil, nil)
		if threads != nil {
			t.Errorf("want nil threads for empty input, got %+v", threads)
		}
	})

	t.Run("all orphans, parent outside set", func(t *testing.T) {
		now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
		turns := []Turn{
			mkTurn("o1", "outside-parent-1", 0, false, now),
			mkTurn("o2", "outside-parent-2", 1, false, now.Add(time.Minute)),
		}
		threads := PartitionThreads(turns, nil)
		if len(threads) != 2 {
			t.Fatalf("want 2 independent threads, got %d: %+v", len(threads), threads)
		}
		if turns[0].ThreadID != "o1" || turns[1].ThreadID != "o2" {
			t.Errorf("each orphan should be its own thread root, got %q %q", turns[0].ThreadID, turns[1].ThreadID)
		}
	})

	t.Run("turns missing uuid entirely", func(t *testing.T) {
		now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
		turns := []Turn{
			mkTurn("", "", 0, false, now),
			mkTurn("", "", 1, false, now.Add(time.Minute)),
		}
		threads := PartitionThreads(turns, nil)
		if len(threads) != 2 {
			t.Fatalf("want 2 synthetic threads for uuid-less turns, got %d: %+v", len(threads), threads)
		}
		if turns[0].ThreadID == turns[1].ThreadID {
			t.Errorf("uuid-less turns must not collide on the same synthetic ThreadID, both got %q", turns[0].ThreadID)
		}
		if turns[0].ThreadID == "" || turns[1].ThreadID == "" {
			t.Errorf("synthetic ThreadID must not be empty: %q, %q", turns[0].ThreadID, turns[1].ThreadID)
		}
	})
}
