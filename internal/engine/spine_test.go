// spine_test.go — tests for the decision-manifold computation.
//
// A small synthetic decision set with known rel-edges exercises:
//   - gravity ranking (weighted in-degree)
//   - inertia field (settledness ordering)
//   - accretion ordering (eras oldest-first, gravity-desc within)
//   - basin clustering (orbit attachment to high-mass cores)
package engine

import (
	"strings"
	"testing"
	"time"
)

// fixtureNow is a fixed reference time so age/inertia is deterministic.
var fixtureNow = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// fixtureCorpus builds a synthetic spine with a clear centre of mass.
//
// Shape:
//   - "block" is the deepest well: superseded-by from three fossils + builds-on
//     from two live decisions → highest gravity.
//   - "workspace" is a secondary core: extends from two decisions.
//   - "fossil-a/b/c" supersede-toward "block" (high-weight structural edges).
//   - "leaf-1" builds-on "block" (orbits block).
//   - "leaf-2" extends "workspace" (orbits workspace).
//   - "leaf-3" builds-on "leaf-1" (two-hop → inherits block basin).
//   - "orphan" references nothing in-corpus (free body, zero gravity).
func fixtureCorpus() []Decision {
	return []Decision{
		{
			ID: "block", Title: "Content-Addressed Block", Kind: "adr",
			Status: "accepted", Created: "2025-12-01",
		},
		{
			ID: "workspace", Title: "Workspace Membrane", Kind: "adr",
			Status: "accepted", Created: "2025-12-15",
		},
		{
			ID: "fossil-a", Title: "Old KV Mesh", Kind: "adr", Status: "superseded",
			Created: "2025-12-05",
			Edges:   []DecisionEdge{{Rel: "superseded-by", Target: "block"}},
		},
		{
			ID: "fossil-b", Title: "Old Consensus", Kind: "adr", Status: "superseded",
			Created: "2025-12-06",
			Edges:   []DecisionEdge{{Rel: "superseded-by", Target: "block"}},
		},
		{
			ID: "fossil-c", Title: "Merkle Proofs", Kind: "adr", Status: "superseded",
			Created: "2025-12-07",
			Edges:   []DecisionEdge{{Rel: "evolved-from", Target: "block"}},
		},
		{
			ID: "leaf-1", Title: "Bus Payloads", Kind: "adr", Status: "accepted",
			Created: "2026-04-01",
			Edges:   []DecisionEdge{{Rel: "builds-on", Target: "block"}},
		},
		{
			ID: "leaf-2", Title: "Federated Workspace", Kind: "rfc", Status: "draft",
			Created: "2026-05-01",
			Edges:   []DecisionEdge{{Rel: "extends", Target: "workspace"}},
		},
		{
			ID: "leaf-3", Title: "Block GC", Kind: "rfc", Status: "proposed",
			Created: "2026-05-10",
			Edges:   []DecisionEdge{{Rel: "builds-on", Target: "leaf-1"}},
		},
		{
			ID: "wkext", Title: "Holographic Workspace", Kind: "adr", Status: "accepted",
			Created: "2026-01-10",
			Edges:   []DecisionEdge{{Rel: "extends", Target: "workspace"}},
		},
		{
			ID: "orphan", Title: "Standalone Note", Kind: "adr", Status: "draft",
			Created: "2026-05-20",
			// References something OUTSIDE the corpus → contributes no
			// in-corpus gravity to anyone, and nothing points back at it.
			Edges: []DecisionEdge{{Rel: "related", Target: "external-thing", URI: "cog://external/thing"}},
		},
	}
}

func TestComputeManifold_GravityRanking(t *testing.T) {
	m := ComputeManifold(fixtureCorpus(), fixtureNow)

	// block: superseded-by(3)+superseded-by(3)+evolved-from(3)+builds-on(2) = 11
	wantBlock := 3.0 + 3.0 + 3.0 + 2.0
	if got := m.Vertebrae["block"].Gravity; got != wantBlock {
		t.Errorf("block gravity = %.1f, want %.1f", got, wantBlock)
	}
	// workspace: extends(2) + extends(2) = 4
	if got := m.Vertebrae["workspace"].Gravity; got != 4.0 {
		t.Errorf("workspace gravity = %.1f, want 4.0", got)
	}
	// leaf-1: builds-on(2) from leaf-3 = 2
	if got := m.Vertebrae["leaf-1"].Gravity; got != 2.0 {
		t.Errorf("leaf-1 gravity = %.1f, want 2.0", got)
	}
	// orphan: nothing in-corpus points at it → 0
	if got := m.Vertebrae["orphan"].Gravity; got != 0.0 {
		t.Errorf("orphan gravity = %.1f, want 0.0", got)
	}

	// block must rank first, workspace second.
	if m.Ranked[0].Decision.ID != "block" {
		t.Errorf("rank[0] = %s, want block", m.Ranked[0].Decision.ID)
	}
	if m.Ranked[1].Decision.ID != "workspace" {
		t.Errorf("rank[1] = %s, want workspace", m.Ranked[1].Decision.ID)
	}

	// Cores must include the two with mass; orphan must not be a core.
	coreIDs := map[string]bool{}
	for _, c := range m.Cores {
		coreIDs[c.Decision.ID] = true
	}
	if !coreIDs["block"] || !coreIDs["workspace"] {
		t.Errorf("cores missing block/workspace: %v", coreIDs)
	}
	if coreIDs["orphan"] {
		t.Error("orphan should not be a core (zero gravity)")
	}
}

func TestComputeManifold_CostToMove(t *testing.T) {
	m := ComputeManifold(fixtureCorpus(), fixtureNow)
	// block has 4 structural incoming edges (3 fossils + leaf-1 builds-on).
	if got := m.Vertebrae["block"].CostToMove; got != 4 {
		t.Errorf("block cost-to-move = %d, want 4", got)
	}
	// orphan: incoming "related" is annotation, and it's outgoing anyway → 0.
	if got := m.Vertebrae["orphan"].CostToMove; got != 0 {
		t.Errorf("orphan cost-to-move = %d, want 0", got)
	}
}

func TestComputeManifold_InertiaField(t *testing.T) {
	m := ComputeManifold(fixtureCorpus(), fixtureNow)

	// A superseded, old, depended-on fossil must be more settled than a young draft.
	fossil := m.Vertebrae["fossil-a"].Inertia
	draft := m.Vertebrae["leaf-2"].Inertia // draft, young
	if fossil <= draft {
		t.Errorf("fossil inertia (%.2f) should exceed young-draft inertia (%.2f)", fossil, draft)
	}

	// block: old + accepted + high cost-to-move → high inertia.
	if m.Vertebrae["block"].Inertia < 0.7 {
		t.Errorf("block inertia = %.2f, want >= 0.7 (settled core)", m.Vertebrae["block"].Inertia)
	}

	// A fresh draft must be molten (< 0.35).
	if m.Vertebrae["leaf-2"].Inertia >= 0.35 {
		t.Errorf("leaf-2 (draft) inertia = %.2f, want < 0.35 (molten)", m.Vertebrae["leaf-2"].Inertia)
	}
}

func TestComputeManifold_Basins(t *testing.T) {
	m := ComputeManifold(fixtureCorpus(), fixtureNow)

	// fossils + leaf-1 orbit block (direct structural edge to a core).
	for _, id := range []string{"fossil-a", "fossil-b", "fossil-c", "leaf-1"} {
		if m.Vertebrae[id].Basin != "block" {
			t.Errorf("%s basin = %q, want block", id, m.Vertebrae[id].Basin)
		}
	}
	// leaf-2 and wkext orbit workspace.
	for _, id := range []string{"leaf-2", "wkext"} {
		if m.Vertebrae[id].Basin != "workspace" {
			t.Errorf("%s basin = %q, want workspace", id, m.Vertebrae[id].Basin)
		}
	}
	// leaf-3 builds-on leaf-1 (which orbits block) → two-hop inherits block.
	if m.Vertebrae["leaf-3"].Basin != "block" {
		t.Errorf("leaf-3 basin = %q, want block (two-hop)", m.Vertebrae["leaf-3"].Basin)
	}
	// orphan is a free body (own basin).
	if m.Vertebrae["orphan"].Basin != "orphan" {
		t.Errorf("orphan basin = %q, want orphan (free body)", m.Vertebrae["orphan"].Basin)
	}

	// Orbiters of block must include the four direct orbiters + leaf-3.
	orbiters := map[string]bool{}
	for _, id := range m.Orbiters("block") {
		orbiters[id] = true
	}
	for _, id := range []string{"fossil-a", "fossil-b", "fossil-c", "leaf-1", "leaf-3"} {
		if !orbiters[id] {
			t.Errorf("block orbiters missing %s: %v", id, orbiters)
		}
	}
}

func TestComputeManifold_Accretion(t *testing.T) {
	m := ComputeManifold(fixtureCorpus(), fixtureNow)

	if len(m.Eras) == 0 {
		t.Fatal("no accretion eras computed")
	}
	// Eras must be chronological. The first era is 2025-12 (founding burst).
	if !strings.HasPrefix(m.Eras[0].Period, "2025-12") {
		t.Errorf("first era period = %q, want 2025-12*", m.Eras[0].Period)
	}
	// Periods strictly increasing (undated would sort last; none here).
	for i := 1; i < len(m.Eras); i++ {
		if m.Eras[i-1].Period >= m.Eras[i].Period {
			t.Errorf("eras not chronological: %q then %q", m.Eras[i-1].Period, m.Eras[i].Period)
		}
	}
	// Within the founding era, block (highest gravity) must come first.
	founding := m.Eras[0]
	if founding.Decisions[0] != "block" {
		t.Errorf("founding era[0] = %q, want block (gravity-desc within era)", founding.Decisions[0])
	}
	// The core forms in the founding era, before the leaves' eras.
	foundingIdx, leafIdx := -1, -1
	for i, e := range m.Eras {
		for _, id := range e.Decisions {
			if id == "block" {
				foundingIdx = i
			}
			if id == "leaf-2" {
				leafIdx = i
			}
		}
	}
	if foundingIdx < 0 || leafIdx < 0 || foundingIdx >= leafIdx {
		t.Errorf("core (era %d) should accrete before leaf-2 (era %d)", foundingIdx, leafIdx)
	}
}

func TestComputeManifold_Lookup(t *testing.T) {
	m := ComputeManifold(fixtureCorpus(), fixtureNow)

	if v := m.Lookup("block"); v == nil || v.Decision.ID != "block" {
		t.Error("Lookup exact id failed")
	}
	if v := m.Lookup("BLOCK"); v == nil || v.Decision.ID != "block" {
		t.Error("Lookup case-insensitive failed")
	}
	if v := m.Lookup("Holographic"); v == nil || v.Decision.ID != "wkext" {
		t.Error("Lookup by title substring failed")
	}
	if v := m.Lookup("nonexistent-xyz"); v != nil {
		t.Error("Lookup of missing id should return nil")
	}
}

func TestComputeManifold_Deterministic(t *testing.T) {
	a := renderDecisionLineageProjection(ComputeManifold(fixtureCorpus(), fixtureNow))
	b := renderDecisionLineageProjection(ComputeManifold(fixtureCorpus(), fixtureNow))
	if a != b {
		t.Error("projection render is not deterministic for identical input")
	}
}

func TestEdgeWeight(t *testing.T) {
	if edgeWeight("supersedes") != 3.0 {
		t.Errorf("supersedes weight = %.1f, want 3.0", edgeWeight("supersedes"))
	}
	if edgeWeight("SUPERSEDES") != 3.0 {
		t.Errorf("case-insensitive supersedes weight = %.1f, want 3.0", edgeWeight("SUPERSEDES"))
	}
	if edgeWeight("related") != 0.5 {
		t.Errorf("related weight = %.1f, want 0.5", edgeWeight("related"))
	}
	if edgeWeight("totally-unknown-edge") != defaultEdgeWeight {
		t.Errorf("unknown edge weight = %.1f, want %.1f", edgeWeight("totally-unknown-edge"), defaultEdgeWeight)
	}
	if !isStructuralEdge("supersedes") {
		t.Error("supersedes should be structural")
	}
	if isStructuralEdge("related") {
		t.Error("related should be annotation, not structural")
	}
}
