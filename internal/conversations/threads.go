// threads.go — parentUuid DAG partitioning into threads.
//
// A session's turns form a DAG keyed by (uuid, parentUuid): most sessions are
// a single linear chain, but two mechanisms produce genuine additional
// connected components within one session_id:
//
//  1. Subagent sidechain transcripts (verified on disk, see #557 plan): a
//     separate <session-uuid>/subagents/agent-*.jsonl file whose records
//     share the parent session's sessionId, carry isSidechain:true, and
//     start with parentUuid:null — a fresh DAG root. Phase 1 (this file)
//     only partitions turns already in memory; Phase 2 (a separate PR)
//     extends source discovery so those turns actually get merged into one
//     session's turn list before reaching PartitionThreads.
//  2. A genuine branch point: two turns sharing one parentUuid (e.g. a
//     resumed/forked continuation). Not verified against real data at plan
//     time — see the #557 plan's risks section — but handled by the same
//     algorithm: the first child (by turn order) continues the parent's
//     thread, every subsequent child starts its own.
//
// PartitionThreads is pure: no I/O, no global state. It operates on an
// already-fully-assembled, in-order []Turn slice (the caller's responsibility
// — see indexSession in provider.go, which calls this once on the final
// turn slice before returning).
package conversations

import "fmt"

// PartitionThreads partitions turns into connected components of the
// parentUuid DAG, setting Turn.ThreadID on each element of turns IN PLACE
// (turns is mutated through its backing array — callers must not rely on the
// input slice being left untouched), and returns one ThreadMeta per thread
// found, ordered by each thread's root turn's position in turns.
//
// A turn is a root of a new thread when:
//   - ParentUUID is empty, or
//   - ParentUUID does not match any UUID present in turns (parent outside
//     this turn set — e.g. this slice is a partial view), or
//   - ParentUUID matches a turn in this set that already has an earlier
//     child (a branch point): the first child by turn order continues the
//     parent's thread, every subsequent child starts its own.
//
// Degenerate input (0 turns, or turns missing a UUID) does not panic: a turn
// without a UUID becomes the root of a synthetic thread keyed by its index
// rather than colliding with other UUID-less turns on an empty ThreadID.
func PartitionThreads(turns []Turn) []ThreadMeta {
	n := len(turns)
	if n == 0 {
		return nil
	}

	// uuidToIdx maps a turn's own uuid to its index in turns. Turns without a
	// uuid are simply absent from this map — they can never be referenced as
	// someone else's parent, which is the only thing this map is used for.
	uuidToIdx := make(map[string]int, n)
	for i, t := range turns {
		if t.UUID != "" {
			uuidToIdx[t.UUID] = i
		}
	}

	// childrenOf maps a parent uuid to the indices of its direct children, in
	// turns order.
	childrenOf := make(map[string][]int)
	for i, t := range turns {
		if t.ParentUUID != "" {
			childrenOf[t.ParentUUID] = append(childrenOf[t.ParentUUID], i)
		}
	}

	// Branch points: when a parent uuid is itself present in this turn set
	// and has more than one child, every child beyond the first (by turns
	// order) starts its own thread instead of continuing the parent's.
	isBranchRoot := make([]bool, n)
	for parentUUID, kids := range childrenOf {
		if _, present := uuidToIdx[parentUUID]; !present {
			continue // parent outside this set — handled by the ParentUUID-absent rule below
		}
		for j, kidIdx := range kids {
			if j > 0 {
				isBranchRoot[kidIdx] = true
			}
		}
	}

	isRoot := make([]bool, n)
	for i, t := range turns {
		switch {
		case t.ParentUUID == "":
			isRoot[i] = true
		default:
			if _, present := uuidToIdx[t.ParentUUID]; !present {
				isRoot[i] = true
			} else if isBranchRoot[i] {
				isRoot[i] = true
			}
		}
	}

	// Assign ThreadID via BFS from every root, propagating only along
	// continuation edges (a root's own children that are not themselves
	// roots). This does not assume turns is topologically ordered — it works
	// regardless of whether a parent appears before or after its child in
	// the slice.
	threadID := make([]string, n)
	var queue []int
	for i := range turns {
		if isRoot[i] {
			id := turns[i].UUID
			if id == "" {
				id = fmt.Sprintf("no-uuid-%d", i)
			}
			threadID[i] = id
			queue = append(queue, i)
		}
	}
	visited := make([]bool, n)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		curUUID := turns[cur].UUID
		if curUUID == "" {
			continue // no children can reference a uuid-less turn as parent
		}
		for _, childIdx := range childrenOf[curUUID] {
			if isRoot[childIdx] {
				continue // already assigned its own thread
			}
			if threadID[childIdx] == "" {
				threadID[childIdx] = threadID[cur]
				queue = append(queue, childIdx)
			}
		}
	}
	// Defensive fallback: any turn left unassigned (should not happen given
	// every non-root has a present, in-set parent) becomes its own thread
	// root rather than silently carrying an empty ThreadID.
	for i := range turns {
		if threadID[i] == "" {
			id := turns[i].UUID
			if id == "" {
				id = fmt.Sprintf("no-uuid-%d", i)
			}
			threadID[i] = id
		}
	}

	for i := range turns {
		turns[i].ThreadID = threadID[i]
	}

	return buildThreadMeta(turns, isRoot)
}

// buildThreadMeta aggregates per-thread metadata from turns (already carrying
// ThreadID) and the isRoot marker computed by PartitionThreads. Threads are
// returned ordered by their root turn's position in turns.
func buildThreadMeta(turns []Turn, isRoot []bool) []ThreadMeta {
	order := make([]string, 0, 4)
	byID := make(map[string]*ThreadMeta)

	// mainThreadID is the ThreadID of the turn with TurnIndex 0, when present.
	mainThreadID := ""
	haveMain := false
	for _, t := range turns {
		if t.TurnIndex == 0 {
			mainThreadID = t.ThreadID
			haveMain = true
			break
		}
	}

	for i, t := range turns {
		tm, ok := byID[t.ThreadID]
		if !ok {
			tm = &ThreadMeta{ThreadID: t.ThreadID}
			byID[t.ThreadID] = tm
			order = append(order, t.ThreadID)
			if isRoot[i] {
				tm.FirstUUID = t.UUID
				if tm.FirstUUID == "" {
					tm.FirstUUID = t.ThreadID // synthetic no-uuid-N id
				}
			}
		}
		tm.MessageCount++
		tm.LastUUID = t.UUID
		if t.IsSidechain {
			tm.Role = ThreadRoleSubagentSidechain
		}
		if !t.Timestamp.IsZero() {
			if tm.StartedAt.IsZero() || t.Timestamp.Before(tm.StartedAt) {
				tm.StartedAt = t.Timestamp
			}
			if tm.EndedAt.IsZero() || t.Timestamp.After(tm.EndedAt) {
				tm.EndedAt = t.Timestamp
			}
		}
		// FirstUUID fallback for the (rare) case the root turn's own index
		// wasn't the first one visited above (shouldn't happen given the
		// single-pass construction, but guards against future reordering).
		if tm.FirstUUID == "" && isRoot[i] {
			tm.FirstUUID = t.UUID
			if tm.FirstUUID == "" {
				tm.FirstUUID = t.ThreadID
			}
		}
	}

	out := make([]ThreadMeta, 0, len(order))
	for _, id := range order {
		tm := byID[id]
		if haveMain && id == mainThreadID && tm.Role != ThreadRoleSubagentSidechain {
			tm.Role = ThreadRoleMain
		} else if tm.Role == "" {
			tm.Role = ThreadRoleUnknownFork
		}
		out = append(out, *tm)
	}
	return out
}
