// lattice.go — First Instruments Module D: the FROZEN 9-cell (C,H,P) lattice.
//
// Mirrors decision_surface_sim.py §1 (LATTICE) and IMPL-SPEC A6/D6 exactly.
// This is measurement tooling, not a production code path (IMPL-SPEC D0):
// the runner in this package boots real testkernel instances and observes
// Module E's cadence taps; it never becomes part of the shipped daemon.
package experiment

import "math"

// Family tags a cell's role in the frozen sweep (IMPL-SPEC A6/D6).
type Family string

const (
	FamilyAnchor          Family = "anchor"           // K0 — production scale, divisible, mixture-check only
	FamilyScaledAnchor    Family = "scaled_anchor"    // Ks0 — reference cell for per-cell eligibility
	FamilyCoScaled        Family = "co_scaled"        // Ks2, Ks4 — pure co-scale of Ks0
	FamilyOneFactor       Family = "one_factor"       // KsH2, KsHhalf, KsC2 — one clock moved
	FamilyNDDiscriminator Family = "nd_discriminator" // KsND1, KsND2 — non-divisible law discriminators
)

// Cell is one frozen (C,H,P) tuple in the lattice (PREREG §4.3).
type Cell struct {
	ID     string
	C      int // ConsolidationInterval, seconds
	H      int // HeartbeatInterval, seconds
	P      int // PollInterval, seconds
	Family Family
	Role   string
}

// CeilCH returns ceil(C/H) — the clean-form law ratio at this cell.
func (c Cell) CeilCH() int {
	return int(math.Ceil(float64(c.C) / float64(c.H)))
}

// CadenceLaw returns the law-predicted observed cadence ceil(C/H)*H seconds.
func (c Cell) CadenceLaw() int {
	return c.CeilCH() * c.H
}

// Divisible reports whether H | C (the ceiling sits on the discontinuity —
// the knife-edge PREREG §3.2/§M11 Finding A describes).
func (c Cell) Divisible() bool {
	return c.C%c.H == 0
}

// NonDivisible is the negation of Divisible, for readability at call sites.
func (c Cell) NonDivisible() bool {
	return !c.Divisible()
}

// QuantizationDominated reports C < H (PREREG §3.5-H1): ceil(C/H)=1,
// C-dependence quantized away. No cell in the frozen lattice satisfies
// this (every cell has C >= H); kept as an explicit, checkable predicate
// mirroring the sim.
func (c Cell) QuantizationDominated() bool {
	return c.C < c.H
}

// LawCellDiscriminating reports ceil(C/H)*H != C — the law is observably
// separated from the raw-C alternative (PREREG §4.3 F2).
func (c Cell) LawCellDiscriminating() bool {
	return c.CadenceLaw() != c.C
}

// LawVsRawCSepSeconds is the separation, in seconds, between the law
// prediction and the raw-C alternative at this cell.
func (c Cell) LawVsRawCSepSeconds() int {
	sep := c.CadenceLaw() - c.C
	if sep < 0 {
		sep = -sep
	}
	return sep
}

// Lattice is the FROZEN 9-cell lattice (PREREG §4.3, IMPL-SPEC A6/D6),
// exact tuples in seconds. Order matches decision_surface_sim.py LATTICE.
var Lattice = []Cell{
	{"K0", 3600, 60, 30, FamilyAnchor, "production anchor; DIVISIBLE -> law-CHARACTERIZATION/mixture-check only; small n_anchor; NOT in stability stat; NOT law-confirm/kill"},
	{"Ks0", 10, 4, 2, FamilyScaledAnchor, "scaled anchor s=1; NON-DIV; stability statistic + law DISCRIMINATING (reference cell)"},
	{"Ks2", 20, 8, 4, FamilyCoScaled, "co-scaled s=2; NON-DIV; stability statistic + law DISCRIMINATING"},
	{"Ks4", 40, 16, 8, FamilyCoScaled, "co-scaled s=4; NON-DIV; stability statistic + law DISCRIMINATING"},
	{"KsH2", 10, 8, 2, FamilyOneFactor, "one-factor H*2; NON-DIV; (A)/(B) asymmetry (M11r 3->2) + law DISCRIMINATING"},
	{"KsHhalf", 10, 2, 2, FamilyOneFactor, "one-factor H*1/2; DIVISIBLE; (A)/(B) asymmetry (M11r 3->5); law-CHARACTERIZATION/mixture only"},
	{"KsC2", 20, 4, 2, FamilyOneFactor, "one-factor C*2; DIVISIBLE; (A)/(B) asymmetry (M11r 3->5); law-CHARACTERIZATION/mixture only"},
	{"KsND1", 14, 4, 2, FamilyNDDiscriminator, "non-div C-shift; law DISCRIMINATING (16s vs 14s) + asymmetry"},
	{"KsND2", 26, 8, 4, FamilyNDDiscriminator, "non-div two-factor (not a pure co-scale); law DISCRIMINATING (32s vs 26s); NOT in stability stat"},
}

// CellByID returns the frozen cell with the given ID, or false if not found.
func CellByID(id string) (Cell, bool) {
	for _, c := range Lattice {
		if c.ID == id {
			return c, true
		}
	}
	return Cell{}, false
}

// ReferenceCell is the scaled anchor Ks0 — per-cell eligibility (F1) is
// measured relative to it.
var ReferenceCell = func() Cell {
	c, _ := CellByID("Ks0")
	return c
}()

// StabilityFamily is the ONLY cell set feeding CV_grid/CV_null (PREREG
// §4.3/§6.1): the pure co-scaled family, all non-divisible.
var StabilityFamily = []string{"Ks0", "Ks2", "Ks4"}

// OneFactorFamily feeds the (A)/(B) asymmetry probe (+ KsND1's C-shift).
var OneFactorFamily = []string{"KsH2", "KsHhalf", "KsC2"}

// SixNonDivisible are the law confirm/kill DISCRIMINATING cells.
var SixNonDivisible = []string{"Ks0", "Ks2", "Ks4", "KsH2", "KsND1", "KsND2"}

// ThreeDivisible are the law-CHARACTERIZATION/mixture-only cells.
var ThreeDivisible = []string{"K0", "KsHhalf", "KsC2"}

// FrozenM11RInteger is the frozen expected M11r value per cell (PREREG
// §4.3 table): non-divisible cells measure ceil(C/H) cleanly; divisible
// cells measure a non-integer mixture ~= q + p_hat (G8 caveat) but are
// tagged here with their ceil for reference.
var FrozenM11RInteger = map[string]int{
	"K0": 60, "Ks0": 3, "Ks2": 3, "Ks4": 3, "KsH2": 2,
	"KsHhalf": 5, "KsC2": 5, "KsND1": 4, "KsND2": 4,
}

// ─── Per-cell KILL-eligibility (PREREG §4.3 F1; IMPL-SPEC D4 kill_eligible_A) ──

// ClocksMoved returns which of {C,H,P} moved in this cell relative to the
// scaled anchor Ks0.
func ClocksMoved(c Cell) map[string]bool {
	moved := map[string]bool{}
	if c.C != ReferenceCell.C {
		moved["C"] = true
	}
	if c.H != ReferenceCell.H {
		moved["H"] = true
	}
	if c.P != ReferenceCell.P {
		moved["P"] = true
	}
	return moved
}

// KillEligibility is the per-cell KILL-eligibility for each (A) absolute
// (PREREG §4.3 F1). consolidation-cadence generator = {C,H};
// heartbeat-cadence generator = {H}.
type KillEligibility struct {
	ConsCadence bool
	HBCadence   bool
}

// KillEligibleA computes per-cell KILL-eligibility for cell c. Ks0 (the
// reference) has no clock moved -> both ineligible (it is the reference).
func KillEligibleA(c Cell) KillEligibility {
	moved := ClocksMoved(c)
	return KillEligibility{
		ConsCadence: moved["C"] || moved["H"],
		HBCadence:   moved["H"],
	}
}
