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
//  2. A genuine branch point: two turns DIRECTLY sharing one parentUuid in
//     the raw JSONL graph (e.g. a resumed/forked continuation) — not
//     verified against real data at plan time, see the #557 plan's risks
//     section — handled by the same algorithm: the first direct child (by
//     turn order) continues the parent's thread, every subsequent direct
//     child starts its own. A child reached only via bridgeDroppedParents
//     splicing across a dropped record (ordinary, including parallel,
//     tool-call structure routinely produces this) is never a branch point
//     regardless of how many such bridged children a parent has — see
//     PartitionThreads' doc comment for the corpus-verified failure this
//     guards against.
//
// PartitionThreads is pure: no I/O, no global state. It operates on an
// already-fully-assembled, in-order []Turn slice (the caller's responsibility
// — see indexSession in provider.go, which calls this once on the final
// turn slice before returning).
//
// Before calling PartitionThreads, indexSession first calls
// bridgeDroppedParents (this file) to rewrite each turn's ParentUUID past
// any chain of records ParseSession saw but never turned into a Turn
// (tool_result-only user records, type:"system" records, hook outputs,
// text-less assistant records, duplicate uuids). Without that step, a
// surviving turn whose immediate JSONL parent was one of those dropped
// records looks like a fresh DAG root here — which fragments every
// tool-using session (i.e. nearly all of them) into hundreds of spurious
// one- or two-message threads. PartitionThreads itself stays pure and
// unaware of the raw JSONL; the bridging happens one layer up, over the raw
// uuid graph parser.go now records alongside the turn set.
package conversations

import "fmt"

// bridgeDroppedParents rewrites turns[i].ParentUUID, in place, to splice
// across any run of records that ParseSession saw but that never became a
// Turn. rawParents is the full uuid -> parentUuid graph parser.go records
// for every line with a uuid, kept or dropped alike.
//
// For each turn whose direct ParentUUID does not name a turn present in the
// given slice, this walks rawParents upward — parent, grandparent, and so
// on — until it finds a uuid that DOES survive in turns, and rewrites
// ParentUUID to that ancestor. If the chain terminates (empty parentUuid, a
// link absent from rawParents, or a cycle) before reaching a surviving
// ancestor, ParentUUID is left untouched: it already fails to match any
// surviving uuid, so PartitionThreads's existing "parent outside this set"
// root rule already treats it correctly.
//
// A visited-set per turn guards against a cycle in the raw graph (not
// expected in real Claude Code data, but rawParents is otherwise untrusted
// input and PartitionThreads' own cycle handling assumes finite walks).
//
// Returns the set of turn UUIDs whose ParentUUID was actually rewritten
// (splice performed). PartitionThreads uses this to tell a bridged edge —
// this turn's true JSONL parent was one or more dropped records, most
// commonly a tool_result-only user record from ordinary (including
// parallel) tool-call structure — from a genuine second child recorded
// directly against the surviving parent. Only the latter is a real branch
// point; see PartitionThreads' branch-point doc comment.
func bridgeDroppedParents(turns []Turn, rawParents map[string]string) map[string]bool {
	bridged := make(map[string]bool)
	if len(rawParents) == 0 {
		return bridged
	}
	surviving := make(map[string]bool, len(turns))
	for _, t := range turns {
		if t.UUID != "" {
			surviving[t.UUID] = true
		}
	}
	for i := range turns {
		p := turns[i].ParentUUID
		if p == "" || surviving[p] {
			continue // true root, or parent already present — nothing to bridge
		}
		visited := map[string]bool{p: true}
		cur := p
		for {
			next, ok := rawParents[cur]
			if !ok || next == "" {
				break // chain leaves the raw graph or reaches a true root
			}
			if surviving[next] {
				turns[i].ParentUUID = next
				if turns[i].UUID != "" {
					bridged[turns[i].UUID] = true
				}
				break
			}
			if visited[next] {
				break // cycle in the raw graph — give up, leave ParentUUID as-is
			}
			visited[next] = true
			cur = next
		}
	}
	return bridged
}

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
//     UNBRIDGED child (a genuine branch point): the first such child by
//     turn order continues the parent's thread, every subsequent one
//     starts its own.
//
// bridged names the turn UUIDs whose ParentUUID was rewritten by
// bridgeDroppedParents (see its doc comment) — i.e. this turn's real JSONL
// parent was a dropped record (most commonly a tool_result-only user
// record), not the surviving parent it now points at directly. A bridged
// child is never treated as a branch point and never competes with a
// sibling for "first child" status: ordinary (including parallel) tool-call
// structure routinely leaves one assistant turn with both a direct
// surviving child AND a second child reached only by bridging through a
// dropped tool_result — that is not a conversational fork, and counting the
// bridged child toward the branch tally fragmented real multi-thousand-turn
// sessions into a handful-of-messages "main" thread plus large
// "unknown-fork" threads. Pass nil when the caller performed no bridging
// (e.g. synthetic test fixtures) — every child is then treated as direct,
// preserving the original genuine-fork detection.
//
// Degenerate input (0 turns, or turns missing a UUID) does not panic: a turn
// without a UUID becomes the root of a synthetic thread keyed by its index
// rather than colliding with other UUID-less turns on an empty ThreadID.
func PartitionThreads(turns []Turn, bridged map[string]bool) []ThreadMeta {
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
	// and has more than one UNBRIDGED (direct) child, every such child
	// beyond the first (by turns order) starts its own thread instead of
	// continuing the parent's. Bridged children are excluded from this
	// tally entirely — see the doc comment above — so they never become a
	// branch root and never bump a direct sibling out of the "first child"
	// slot.
	isBranchRoot := make([]bool, n)
	for parentUUID, kids := range childrenOf {
		if _, present := uuidToIdx[parentUUID]; !present {
			continue // parent outside this set — handled by the ParentUUID-absent rule below
		}
		var direct []int
		for _, kidIdx := range kids {
			if bridged[turns[kidIdx].UUID] {
				continue // reached only by splicing across a dropped record — not a fork
			}
			direct = append(direct, kidIdx)
		}
		for j, kidIdx := range direct {
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
	// Defensive fallback: a cycle in the (post-bridge) parentUuid graph —
	// e.g. two turns each bridged to the other as parent, which a real
	// compact_boundary logicalParentUuid can produce when the "preserved
	// segment" it names was replayed AFTER the boundary in the raw file
	// rather than sitting before it — leaves every member of that cycle
	// with isRoot false (its parent is present and unbridged-single-child,
	// so none of the three isRoot rules ever fire) and unreachable from any
	// other root, so the main BFS above never visits it. Minting one
	// synthetic thread PER leftover turn here (the original, simpler form
	// of this fallback) turns that single stranded component into as many
	// threads as it has members — measured on a real corpus session: 2241
	// turns, one continuous conversation, exploded into 2241 one-turn
	// threads. Instead, sweep each remaining connected component with its
	// own BFS (seeded from its first-by-turns-order member, so turn_index 0
	// — if it's the stranded one — still anchors role=main via
	// buildThreadMeta's mainThreadID lookup) so the whole component
	// collapses into a single thread instead.
	for i := range turns {
		if threadID[i] != "" {
			continue
		}
		id := turns[i].UUID
		if id == "" {
			id = fmt.Sprintf("no-uuid-%d", i)
		}
		threadID[i] = id
		queue := []int{i}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			curUUID := turns[cur].UUID
			if curUUID == "" {
				continue
			}
			for _, childIdx := range childrenOf[curUUID] {
				if threadID[childIdx] == "" {
					threadID[childIdx] = id
					queue = append(queue, childIdx)
				}
			}
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
		// Role derives from the thread's ROOT turn's IsSidechain, not from
		// any member: a thread is a subagent-sidechain thread because it
		// forked from one (parentUuid:null, isSidechain:true), not because
		// some later member happens to carry the flag. Checking every member
		// meant one isSidechain turn anywhere downstream of a plain root
		// (e.g. once Phase 2 merges subagent turns into a session ahead of
		// PartitionThreads) reclassified the WHOLE thread — including the
		// session's actual main thread — leaving no thread with role=main at
		// all.
		if isRoot[i] && t.IsSidechain {
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
		// A cycle in the parentUuid graph (e.g. a(parent=b), b(parent=a))
		// means no member of the thread is ever marked isRoot, so neither
		// FirstUUID assignment above fires and it would otherwise ship as
		// the empty string — a positive false claim (FirstUUID has no
		// omitempty), not just a missing value. ThreadID is guaranteed
		// non-empty here (PartitionThreads' own fallback sets it to the
		// member's own uuid, or a synthetic id, before this ever runs), so
		// it is the honest fallback: "this thread's identity, root
		// unresolved" rather than a lie that FirstUUID was computed.
		if tm.FirstUUID == "" {
			tm.FirstUUID = tm.ThreadID
		}
		out = append(out, *tm)
	}
	return out
}
