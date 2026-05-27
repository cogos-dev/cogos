// spine.go — the decision manifold: gravity / inertia / accretion / orbits.
//
// The "spine" is the evolutionary lineage of the architecture's own
// decisions (ADRs, RFCs, ratified decision-insights), read as a directed
// graph over their `rel:` edges. This file holds the pure, deterministic
// computation that turns a decision corpus into a measured field:
//
//   - Gravity  — weighted in-degree over decision rel: edges. High-mass
//     decisions are the stars / centres of mass.
//   - Inertia  — settledness: age-since-ratification × low-revisit ×
//     cost-to-move. Founding, rarely-touched, depended-on
//     decisions are the settled core; drafts are molten.
//   - Accretion — temporal accumulation of mass: decisions ordered by date,
//     the core forming first, later bodies accreting around it.
//   - Orbits / basins — clustering of decisions by edge-proximity to the
//     high-mass cores; which bodies orbit which well.
//
// This is the empirical complement to the evolutionary-spine RFC and the
// "true shape of the decision manifold" cartography: where those *name* the
// spine, this *measures* it. It is the decision-lineage sibling to the
// theoretical-lineage convergence-map projection — different corpus, same
// projection shape.
//
// IMPORTANT: this is metric computation over edges and dates, not a physics
// simulation. The solar-system framing (mass, inertia, accretion, orbit) is
// the operator's mental model; the numbers below are graph statistics chosen
// to express it.
package engine

import (
	"math"
	"sort"
	"strings"
	"time"
)

// ─── Edge weights ─────────────────────────────────────────────────────────────

// SpineEdgeWeight assigns gravity weight to each decision rel: edge kind.
//
// Lineage-structural edges (supersedes / reshapes / extends / builds-on /
// depends-on) pull harder than annotation edges (grounds / related / etc.):
// they define position in the evolutionary history, per the evolutionary-spine
// RFC's spine-structural vs annotation partition. An edge that supersedes a
// prior decision exerts maximal pull — the superseded vertebra is a fossil the
// living structure grew off.
//
// Edge names are matched case-insensitively. Unknown edges contribute the
// default annotation weight so the field never silently drops a real edge.
//
// GRAVITY IS WEIGHTED IN-DEGREE — this intentionally diverges from the
// decision-manifold cartography's raw-count baseline. The cartography ranked
// vertebrae by unweighted incoming-edge count; here a structural edge can
// outweigh several annotation edges, so the top of the ranking shifts (e.g.
// self-similar-bootstrap, which carries its mass via many `cited-by`
// annotation edges, drops relative to the raw-count view). To reproduce the
// raw-count ranking exactly, set every weight (and defaultEdgeWeight) to 1.0:
// uniform weight 1.0 == raw in-degree.
var spineEdgeWeights = map[string]float64{
	// Spine-structural (lineage) edges — high pull.
	//
	// supersedes/superseded-by carry the SAME weight in both directions, and
	// BOTH directions add to their respective node's gravity. This is
	// intentional: a `superseded-by` edge on a fossil adds gravity to the
	// fossil (not just to its successor) because the fossil is the genetic
	// anchor the living structure grew off — a high-gravity fossil is a
	// load-bearing ancestor, not dead weight. So the supersede relation
	// deposits structural mass on both the old vertebra and the new one.
	"supersedes":     3.0,
	"superseded-by":  3.0,
	"reshapes":       2.5,
	"reshaped-by":    2.5,
	"extends":        2.0,
	"extended-by":    2.0,
	"builds-on":      2.0,
	"built-on-by":    2.0,
	"depends-on":     2.5,
	"depended-on-by": 2.5,
	"requires":       2.5,
	// evolved-from / evolves / absorbed-by are DELIBERATE EXTENSIONS to the
	// evolutionary-spine RFC's edge vocabulary — semantic equivalents of
	// supersedes (a decision that evolved from / absorbed an earlier one
	// replaces it) and treated as structural. Accepted by the operator
	// 2026-05-27.
	"evolved-from": 3.0,
	"evolves":      3.0,
	"absorbed-by":  3.0,
	// unifies is structural (it is supersede-by-*merging*: it folds two or more
	// prior vertebrae into one) but weighted SOFTER than supersedes — the prior
	// decisions are merged, not discarded. Weight 2.0, not 3.0.
	// This resolves RFC `evolutionary-spine-decision-lineage` Open-Q2
	// (operator decision 2026-05-27).
	"unifies": 2.0,

	// Annotation edges — light pull.
	"grounds":         1.5,
	"grounded-by":     1.5,
	"implements":      1.5,
	"implemented-by":  1.5,
	"uses":            1.0,
	"used-by":         1.0,
	"composes-with":   1.0,
	"complements":     1.0,
	"companion":       1.0,
	"enables":         1.0,
	"refined-by":      1.5,
	"refines":         1.5,
	"clarifies":       1.0,
	"clarified-by":    1.0,
	"shares-envelope": 1.0,
	"cited-by":        1.0,
	"applies":         1.0,
	"related":         0.5,
	"context":         0.5,
	"provenance":      0.5,
	"compares-with":   0.5,
}

// defaultEdgeWeight is the gravity contribution of an edge whose rel kind is
// not in the table above. It matches a generic annotation edge.
const defaultEdgeWeight = 0.5

// spineStructuralEdges is the set of rel kinds that define lineage position
// (vs annotation). Used for orbit/basin attachment and the lineage view.
var spineStructuralEdges = map[string]bool{
	"supersedes": true, "superseded-by": true,
	"reshapes": true, "reshaped-by": true,
	"extends": true, "extended-by": true,
	"builds-on": true, "built-on-by": true,
	"depends-on": true, "depended-on-by": true,
	"requires":     true,
	"evolved-from": true, "evolves": true,
	"absorbed-by": true, "unifies": true,
}

// edgeWeight returns the gravity weight for a rel kind (case-insensitive).
func edgeWeight(rel string) float64 {
	if w, ok := spineEdgeWeights[strings.ToLower(strings.TrimSpace(rel))]; ok {
		return w
	}
	return defaultEdgeWeight
}

// isStructuralEdge reports whether a rel kind is a spine-structural (lineage)
// edge as opposed to an annotation edge.
func isStructuralEdge(rel string) bool {
	return spineStructuralEdges[strings.ToLower(strings.TrimSpace(rel))]
}

// ─── Decision (vertebra) model ──────────────────────────────────────────────

// DecisionEdge is one outgoing rel: edge from a decision to a target.
// Target is the resolved slug/id of the referenced decision (the last path
// segment of the cog:// URI), or the raw URI when it cannot be resolved.
type DecisionEdge struct {
	Rel    string `json:"rel"`
	Target string `json:"target"` // resolved decision id/slug
	URI    string `json:"uri"`    // original uri
}

// Decision is one vertebra in the spine: an ADR, RFC, or ratified
// decision-insight, with its lineage edges and the date it accreted.
type Decision struct {
	ID      string         `json:"id"` // canonical key (slug or id)
	Title   string         `json:"title"`
	Kind    string         `json:"kind"`    // adr | rfc | insight
	Status  string         `json:"status"`  // accepted | proposed | draft | superseded | ...
	Created string         `json:"created"` // YYYY-MM-DD or RFC3339
	Path    string         `json:"path"`
	Edges   []DecisionEdge `json:"edges"`
}

// createdTime parses the Created field into a time, returning zero time on
// failure. Accepts both date-only (YYYY-MM-DD) and RFC3339.
func (d Decision) createdTime() time.Time {
	s := strings.TrimSpace(d.Created)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	// Some frontmatter quotes the date or includes time without zone.
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}

// ─── The manifold ─────────────────────────────────────────────────────────────

// VertebraMetrics is the computed field value for a single decision.
type VertebraMetrics struct {
	Decision Decision `json:"decision"`

	// Gravity is the weighted in-degree: sum of edge weights of every other
	// decision that points at this one. The centre-of-mass measure.
	Gravity float64 `json:"gravity"`

	// RawInDegree is the unweighted count of incoming edges.
	RawInDegree int `json:"raw_in_degree"`

	// OutDegree is the count of outgoing edges this decision declares.
	OutDegree int `json:"out_degree"`

	// Inertia is settledness in [0,1]: how hard this decision is to move.
	// Old + accepted + heavily-depended-on → high inertia (settled core).
	// Draft / proposed / young → low inertia (molten / proto-planetary).
	Inertia float64 `json:"inertia"`

	// CostToMove is the number of decisions that would have to move if this
	// one changed (its incoming structural in-degree). The blast radius.
	CostToMove int `json:"cost_to_move"`

	// Basin is the id of the high-mass core this decision orbits, or its own
	// id if it is itself a core (or unattached).
	Basin string `json:"basin"`

	// AgeDays is days since ratification at compute time (0 if undated).
	AgeDays int `json:"age_days"`
}

// Manifold is the whole computed decision field.
type Manifold struct {
	// Vertebrae indexed by decision id, with computed metrics.
	Vertebrae map[string]*VertebraMetrics `json:"vertebrae"`

	// Ranked is Vertebrae sorted by descending gravity (ties broken by id).
	Ranked []*VertebraMetrics `json:"ranked"`

	// Cores are the highest-gravity vertebrae (the stars), in rank order.
	Cores []*VertebraMetrics `json:"cores"`

	// Basins maps a core id → the decisions that orbit it (including the core).
	Basins map[string][]string `json:"basins"`

	// Eras are accretion windows: decisions grouped by the period they
	// formed in, oldest first.
	Eras []AccretionEra `json:"eras"`

	// ComputedAt is the timestamp the field was measured.
	ComputedAt time.Time `json:"computed_at"`
}

// AccretionEra is a temporal window in which a set of decisions accreted.
type AccretionEra struct {
	Label     string   `json:"label"`      // e.g. "2025-12 founding burst"
	Period    string   `json:"period"`     // YYYY-MM
	Decisions []string `json:"decisions"`  // ids created in this window, gravity-desc
	TotalMass float64  `json:"total_mass"` // sum of gravity accreted in this era
}

// ─── ComputeManifold ───────────────────────────────────────────────────────────

// numCores is how many top-gravity vertebrae are treated as basin centres.
const numCores = 6

// ComputeManifold turns a decision corpus into the measured field. Pure and
// deterministic given (decisions, now): same input → same output.
//
// now is the reference time for age/inertia (pass time.Now().UTC() in
// production; a fixed time in tests for determinism).
func ComputeManifold(decisions []Decision, now time.Time) *Manifold {
	m := &Manifold{
		Vertebrae:  make(map[string]*VertebraMetrics, len(decisions)),
		Basins:     make(map[string][]string),
		ComputedAt: now,
	}

	// Seed vertebra metrics.
	for _, d := range decisions {
		m.Vertebrae[d.ID] = &VertebraMetrics{
			Decision:  d,
			OutDegree: len(d.Edges),
		}
	}

	// ── Gravity: weighted in-degree ──
	// Every outgoing edge contributes mass to its TARGET. We only count edges
	// whose target is a known decision in the corpus (the spine is closed over
	// itself; references out to research notes etc. don't add decision-gravity).
	for _, d := range decisions {
		for _, e := range d.Edges {
			tgt, ok := m.Vertebrae[e.Target]
			if !ok {
				continue
			}
			tgt.Gravity += edgeWeight(e.Rel)
			tgt.RawInDegree++
			if isStructuralEdge(e.Rel) {
				tgt.CostToMove++
			}
		}
	}

	// ── Inertia: settledness ──
	for id := range m.Vertebrae {
		v := m.Vertebrae[id]
		v.AgeDays = ageDays(v.Decision, now)
		v.Inertia = computeInertia(v, now)
	}

	// ── Ranking by gravity ──
	m.Ranked = make([]*VertebraMetrics, 0, len(m.Vertebrae))
	for _, v := range m.Vertebrae {
		m.Ranked = append(m.Ranked, v)
	}
	sort.Slice(m.Ranked, func(i, j int) bool {
		if m.Ranked[i].Gravity != m.Ranked[j].Gravity {
			return m.Ranked[i].Gravity > m.Ranked[j].Gravity
		}
		// Tie-break: higher raw in-degree, then id ascending for determinism.
		if m.Ranked[i].RawInDegree != m.Ranked[j].RawInDegree {
			return m.Ranked[i].RawInDegree > m.Ranked[j].RawInDegree
		}
		return m.Ranked[i].Decision.ID < m.Ranked[j].Decision.ID
	})

	// ── Cores: the top-N by gravity that actually have mass ──
	for _, v := range m.Ranked {
		if v.Gravity <= 0 {
			break
		}
		m.Cores = append(m.Cores, v)
		if len(m.Cores) >= numCores {
			break
		}
	}

	// ── Orbits / basins: attach each decision to the nearest core ──
	computeBasins(m)

	// ── Accretion: order by date into eras ──
	m.Eras = computeAccretion(m)

	return m
}

// ageDays returns whole days between the decision's creation and now.
func ageDays(d Decision, now time.Time) int {
	t := d.createdTime()
	if t.IsZero() {
		return 0
	}
	days := int(now.Sub(t).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// computeInertia derives a settledness score in [0,1].
//
// Three coupled signals, combined multiplicatively-ish so a decision must be
// settled on every axis to score high:
//
//   - status: accepted/superseded seal the decision (high); proposed/draft are
//     molten (low). A superseded decision has *maximal* status-inertia in the
//     model's sense — it cannot move, it can only be read (a fossil).
//   - age: older-and-still-here → more settled. Saturating curve so the
//     founding burst (oldest) tops out rather than growing unbounded.
//   - cost-to-move: more decisions depending on it → harder to move (more
//     would have to move with it). Saturating in dependents.
func computeInertia(v *VertebraMetrics, now time.Time) float64 {
	// status axis ∈ [0,1]
	var statusScore float64
	switch strings.ToLower(strings.TrimSpace(v.Decision.Status)) {
	case "superseded":
		statusScore = 1.0 // sealed fossil — immovable by definition
	case "accepted", "active", "ratified":
		statusScore = 0.9
	case "pending":
		statusScore = 0.5
	case "proposed":
		statusScore = 0.35
	case "in-review":
		statusScore = 0.25
	case "draft":
		statusScore = 0.15
	default:
		statusScore = 0.4
	}

	// age axis ∈ [0,1] — saturating. ~1 year (365d) → ~0.86, asymptote 1.
	ageScore := 0.0
	if v.AgeDays > 0 {
		ageScore = 1.0 - math.Exp(-float64(v.AgeDays)/180.0)
	}

	// cost-to-move axis ∈ [0,1] — saturating in structural dependents.
	costScore := 1.0 - math.Exp(-float64(v.CostToMove)/4.0)

	// Weighted blend. Status dominates (it is the seal); age and cost refine.
	inertia := 0.55*statusScore + 0.25*ageScore + 0.20*costScore
	if inertia > 1.0 {
		inertia = 1.0
	}
	return inertia
}

// computeBasins attaches every decision to the core it most closely orbits.
//
// Attachment rule (edge-proximity to a high-mass core):
//  1. A core anchors its own basin ONLY if it has no structural edge to a
//     strictly-heavier core. A lighter core that references a heavier one is a
//     sub-well that orbits the heavier core (a moon-with-moons still orbits its
//     planet) — matching the cartography, where heavy-but-derivative nodes
//     (e.g. bus-payloads-as-cogblocks) orbit the deepest well (the block).
//  2. A non-core decision orbits the heaviest core it points at directly.
//  3. Otherwise it follows one more hop: if an outgoing structural edge points
//     at a decision that orbits a core, it inherits that basin (two-hop).
//  4. Otherwise it is unattached (basin = its own id), a free body.
func computeBasins(m *Manifold) {
	coreSet := make(map[string]float64, len(m.Cores))
	for _, c := range m.Cores {
		coreSet[c.Decision.ID] = c.Gravity
	}

	// Pass 1: cores. A core orbits a strictly-heavier core it references;
	// otherwise it anchors its own basin. (Iterating m.Cores in gravity-desc
	// order means the heaviest cores resolve to self first.)
	for _, c := range m.Cores {
		best := ""
		bestMass := c.Gravity // only a STRICTLY heavier core wins
		for _, e := range c.Decision.Edges {
			if !isStructuralEdge(e.Rel) {
				continue
			}
			if g, ok := coreSet[e.Target]; ok && g > bestMass {
				best = e.Target
				bestMass = g
			}
		}
		if best != "" {
			c.Basin = best
		} else {
			c.Basin = c.Decision.ID
		}
	}

	// Pass 2: non-core one-hop attachment to the heaviest directly-referenced
	// core. (Cores already have a basin from Pass 1.)
	for _, v := range m.Ranked {
		if _, isCore := coreSet[v.Decision.ID]; isCore {
			continue
		}
		best := ""
		bestMass := -1.0
		for _, e := range v.Decision.Edges {
			if !isStructuralEdge(e.Rel) {
				continue
			}
			if g, ok := coreSet[e.Target]; ok && g > bestMass {
				best = e.Target
				bestMass = g
			}
		}
		if best != "" {
			v.Basin = best
		}
	}

	// Pass 3: two-hop attachment — inherit a neighbour's basin.
	for _, v := range m.Ranked {
		if v.Basin != "" {
			continue
		}
		best := ""
		bestMass := -1.0
		for _, e := range v.Decision.Edges {
			if !isStructuralEdge(e.Rel) {
				continue
			}
			nbr, ok := m.Vertebrae[e.Target]
			if !ok || nbr.Basin == "" {
				continue
			}
			if g, ok := coreSet[nbr.Basin]; ok && g > bestMass {
				best = nbr.Basin
				bestMass = g
			}
		}
		if best != "" {
			v.Basin = best
		}
	}

	// Pass 4: remaining are free bodies (own basin).
	for _, v := range m.Ranked {
		if v.Basin == "" {
			v.Basin = v.Decision.ID
		}
	}

	// Pass 5: roll sub-well chains up to the deepest (root) well, so basin
	// membership reports the well a body ultimately falls toward — not the
	// intermediate sub-well it happens to reference. A body attached to a
	// sub-well core whose own basin is a heavier core inherits that heavier
	// core. Bounded iteration guards against any pathological cycle (the
	// decision graph is a DAG, so this terminates in ≤ depth hops).
	for _, v := range m.Ranked {
		root := v.Basin
		for hops := 0; hops < len(m.Vertebrae); hops++ {
			anchor, ok := m.Vertebrae[root]
			if !ok || anchor.Basin == "" || anchor.Basin == root {
				break // reached a self-anchored root well or a free body
			}
			root = anchor.Basin
		}
		v.Basin = root
	}

	// Collect basin membership, gravity-desc within each basin.
	for _, v := range m.Ranked {
		m.Basins[v.Basin] = append(m.Basins[v.Basin], v.Decision.ID)
	}
}

// computeAccretion groups decisions by the month they were created, oldest
// first, summing the gravity that accreted in each window.
func computeAccretion(m *Manifold) []AccretionEra {
	byPeriod := make(map[string][]*VertebraMetrics)
	for _, v := range m.Ranked {
		t := v.Decision.createdTime()
		if t.IsZero() {
			byPeriod["undated"] = append(byPeriod["undated"], v)
			continue
		}
		period := t.Format("2006-01")
		byPeriod[period] = append(byPeriod[period], v)
	}

	periods := make([]string, 0, len(byPeriod))
	for p := range byPeriod {
		periods = append(periods, p)
	}
	// Chronological; "undated" sorts last.
	sort.Slice(periods, func(i, j int) bool {
		if periods[i] == "undated" {
			return false
		}
		if periods[j] == "undated" {
			return true
		}
		return periods[i] < periods[j]
	})

	var eras []AccretionEra
	for _, p := range periods {
		members := byPeriod[p]
		// Within an era, order by gravity-desc (heaviest accreted first).
		sort.Slice(members, func(i, j int) bool {
			if members[i].Gravity != members[j].Gravity {
				return members[i].Gravity > members[j].Gravity
			}
			return members[i].Decision.ID < members[j].Decision.ID
		})
		ids := make([]string, 0, len(members))
		var mass float64
		for _, v := range members {
			ids = append(ids, v.Decision.ID)
			mass += v.Gravity
		}
		label := p
		if p != "undated" && len(members) >= 10 {
			label = p + " (accretion burst)"
		}
		eras = append(eras, AccretionEra{
			Label:     label,
			Period:    p,
			Decisions: ids,
			TotalMass: mass,
		})
	}
	return eras
}

// ─── Lookup helpers ─────────────────────────────────────────────────────────

// Lookup finds a vertebra by id, slug, or display-number-style suffix match.
// Returns nil if not found. Match order:
//  1. exact id
//  2. case-insensitive id
//  3. id ends with the query (e.g. query "84" matches "084", "adr-084")
//  4. title contains the query (case-insensitive)
func (m *Manifold) Lookup(query string) *VertebraMetrics {
	if v, ok := m.Vertebrae[query]; ok {
		return v
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	for id, v := range m.Vertebrae {
		if strings.ToLower(id) == q {
			return v
		}
	}
	// Suffix match (e.g. "84" → "adr-084"/"084"). Collect ALL candidates and
	// sort by id before returning the first, so a multi-match query is
	// deterministic regardless of Go's randomized map iteration order.
	var suffixIDs []string
	for id := range m.Vertebrae {
		lid := strings.ToLower(id)
		if strings.HasSuffix(lid, "-"+q) || strings.HasSuffix(lid, q) {
			suffixIDs = append(suffixIDs, id)
		}
	}
	if len(suffixIDs) > 0 {
		sort.Strings(suffixIDs)
		return m.Vertebrae[suffixIDs[0]]
	}

	// Title substring match — same determinism discipline.
	var titleIDs []string
	for id, v := range m.Vertebrae {
		if strings.Contains(strings.ToLower(v.Decision.Title), q) {
			titleIDs = append(titleIDs, id)
		}
	}
	if len(titleIDs) > 0 {
		sort.Strings(titleIDs)
		return m.Vertebrae[titleIDs[0]]
	}
	return nil
}

// Orbiters returns the ids orbiting a given core (excluding the core itself),
// gravity-desc.
func (m *Manifold) Orbiters(coreID string) []string {
	members := m.Basins[coreID]
	out := make([]string, 0, len(members))
	for _, id := range members {
		if id != coreID {
			out = append(out, id)
		}
	}
	return out
}

// IncomingEdges returns the decisions (id + rel) that point AT the given id.
func (m *Manifold) IncomingEdges(id string) []DecisionEdge {
	var in []DecisionEdge
	for srcID, v := range m.Vertebrae {
		for _, e := range v.Decision.Edges {
			if e.Target == id {
				in = append(in, DecisionEdge{Rel: e.Rel, Target: srcID, URI: e.URI})
			}
		}
	}
	sort.Slice(in, func(i, j int) bool { return in[i].Target < in[j].Target })
	return in
}
