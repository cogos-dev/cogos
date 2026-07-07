// manifest.go — First Instruments Module D: the out-of-tree data store +
// manifest.json shape (IMPL-SPEC D4/§6.4/H3).
//
// Observations are written OUTSIDE any watched tree, to
// ${COGOS_WORKSPACE_ROOT:-$HOME/workspaces}/first-instruments-runs/<run_id>/
// — a sibling of all workspaces, watched by no ProjectionWatcher and
// indexed by no memory corpus (H3). Append-only writes (K7).
package experiment

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RunsRoot returns the out-of-tree data-store root
// (${COGOS_WORKSPACE_ROOT:-$HOME/workspaces}/first-instruments-runs), per
// the repo's env-var pathing convention.
func RunsRoot() (string, error) {
	root := os.Getenv("COGOS_WORKSPACE_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("experiment: resolve home dir: %w", err)
		}
		root = filepath.Join(home, "workspaces")
	}
	return filepath.Join(root, "first-instruments-runs"), nil
}

// RunDir returns the per-run directory (RunsRoot/<runID>) and ensures it
// exists.
func RunDir(runID string) (string, error) {
	root, err := RunsRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, runID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("experiment: mkdir run dir %s: %w", dir, err)
	}
	return dir, nil
}

// Observation is one append-only JSONL row written to observations.jsonl
// (IMPL-SPEC D4). Fields are a superset covering the per-cell recordings:
// M11r cadence ratio, KC-3-LAW residual, divisible-mixture characterization,
// H6 process-state tags, and per-cell kill-eligibility.
type Observation struct {
	Timestamp string `json:"timestamp"`
	RunID     string `json:"run_id"`
	CellID    string `json:"cell_id"`

	ObservedConsolidationCadenceMs float64 `json:"observed_consolidation_cadence_ms"`
	ObservedHeartbeatCadenceMs     float64 `json:"observed_heartbeat_cadence_ms"`
	M11r                           float64 `json:"m11r"`

	// Per-cell kill-eligibility (F1) — only eligible absolutes enter the
	// KILL comparison.
	KillEligibleConsCadence bool `json:"kill_eligible_cons_cadence"`
	KillEligibleHBCadence   bool `json:"kill_eligible_hb_cadence"`

	// KC-3-LAW (headline).
	Divisible                    bool     `json:"divisible"`
	LawCellDiscriminating        bool     `json:"law_cell_discriminating"`
	KC3LawResidualMs             *float64 `json:"kc3_law_residual_ms,omitempty"` // only set at non-divisible cells
	KC3LawConfirmed              *bool    `json:"kc3_law_confirmed,omitempty"`   // only set at non-divisible cells
	DivisibleMixtureFracAtC      *float64 `json:"divisible_mixture_frac_at_c,omitempty"`
	DivisibleMixtureFracAtCPlusH *float64 `json:"divisible_mixture_frac_at_c_plus_h,omitempty"`
	DivisibleMixtureMeanMs       *float64 `json:"divisible_mixture_mean_ms,omitempty"`

	// H6 process-state tag.
	ProcessState         string `json:"process_state"`
	ProcessActiveOverlap bool   `json:"process_active_overlap"`

	// K12-domination + divisibility tags (D4).
	K12QuantizationDominated bool `json:"k12_quantization_dominated"`

	// Tick-attribution guard (D2).
	TickSource string `json:"tick_source"` // "natural" | "triggered" | "ambiguous"

	// INSTRUMENT-BROKEN signal.
	RunError bool `json:"run_error"`
}

// ObservationWriter appends Observation rows to
// <run_dir>/observations.jsonl (K7 append-only discipline).
type ObservationWriter struct {
	path string
	f    *os.File
}

// NewObservationWriter opens (creating if absent) the append-only
// observations.jsonl file for runID under the out-of-tree data store.
func NewObservationWriter(runID string) (*ObservationWriter, error) {
	dir, err := RunDir(runID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "observations.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("experiment: open %s: %w", path, err)
	}
	return &ObservationWriter{path: path, f: f}, nil
}

// Write appends one observation as a JSON line.
func (w *ObservationWriter) Write(obs Observation) error {
	data, err := json.Marshal(obs)
	if err != nil {
		return fmt.Errorf("experiment: marshal observation: %w", err)
	}
	if _, err := w.f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("experiment: write observation: %w", err)
	}
	return nil
}

// Close closes the underlying file.
func (w *ObservationWriter) Close() error {
	return w.f.Close()
}

// Path returns the observations.jsonl path this writer targets.
func (w *ObservationWriter) Path() string {
	return w.path
}

// Manifest is the run-level manifest.json shape (IMPL-SPEC D4). Recorded
// once per run alongside observations.jsonl.
type Manifest struct {
	RunID     string `json:"run_id"`
	Generated string `json:"generated"`

	// Existence set (PREREG §1/§4.3): M11r is the SOLE existence candidate.
	ExistenceSet   []string `json:"existence_set"`
	EffectiveM     int      `json:"effective_m"`
	EffectiveT     int      `json:"effective_t"`
	TLookelsewhere int      `json:"t_lookelsewhere"`

	// Deliverables/characterization block (M1A, M1B, M9, M10, M13r, M2osc).
	DeliverablesCharacterization []DeliverableEntry `json:"deliverables_characterization"`

	// Headline block (KC-3-LAW).
	Headline HeadlineBlock `json:"headline"`

	// Echo-class block (Module C's 15-ratio table + literals).
	EchoClass EchoClassBlock `json:"echo_class"`

	// Controls block (PC-SYNTH + negative controls).
	Controls ControlsBlock `json:"controls"`

	// Nulls block (Gaussian-multiplicative family + jitter-model-validation gate).
	Nulls NullsBlock `json:"nulls"`

	CVNullRule        string `json:"cv_null_rule"`
	Bootstrap         string `json:"bootstrap"`
	ReplicateProtocol string `json:"replicate_protocol"`

	Cells []CellManifestEntry `json:"cells"`

	CalibratedThreshold     float64            `json:"calibrated_threshold"`
	HeldOutValidationHalfW  float64            `json:"held_out_validation_half_width"`
	PowerBatteryRejectRates map[string]float64 `json:"power_battery_reject_rates"` // keyed by delta string

	NMax    int   `json:"n_max"`
	NAnchor []int `json:"n_anchor"` // {3,5} for K0

	DecisionSurfaceSim DecisionSurfaceSimBlock `json:"decision_surface_sim"`

	KernelGitSHA      string `json:"kernel_git_sha"`
	KernelGitDirty    bool   `json:"kernel_git_dirty"`
	KernelGitDiffHash string `json:"kernel_git_diff_hash"`
	Branch            string `json:"branch"`
	Worktree          string `json:"worktree"`
}

type DeliverableEntry struct {
	ID                string `json:"id"`
	Role              string `json:"role"`
	ExistenceEligible bool   `json:"existence_eligible"`
	// Extra fields for M2osc's characterization tags.
	ClockCouplingFalsifiedByProtocol *bool `json:"clock_coupling_falsified_by_protocol,omitempty"`
}

type HeadlineBlock struct {
	Name                 string   `json:"name"`
	LawStatement         string   `json:"law_statement"`
	CleanForm            string   `json:"clean_form"`
	Grade                string   `json:"grade"`
	LawResidualTolerance string   `json:"law_residual_tolerance"`
	DiscriminatingCells  []string `json:"discriminating_cells"`
	DivisibleCells       []string `json:"divisible_cells"`
	DivisibleDisposition string   `json:"divisible_disposition"`
	M11Tap               string   `json:"m11_tap"`
	PersistentRunError   string   `json:"persistent_run_error"`
}

type EchoClassBlock struct {
	RatioCount int      `json:"ratio_count"`
	ClusterIDs []string `json:"cluster_ids"`
}

type ControlsBlock struct {
	PCSynthNoiseSigma float64 `json:"pc_synth_noise_sigma"`
	PCSynthClears     bool    `json:"pc_synth_clears"`
	LiveNProviders    int     `json:"live_n_providers"`
}

type NullsBlock struct {
	Family                    string          `json:"family"`
	Sigma                     float64         `json:"sigma"`
	ThreeDisjointSeedSets     []string        `json:"three_disjoint_seed_sets"`
	JitterModelValidationGate JitterGateBlock `json:"jitter_model_validation_gate"`
}

type JitterGateBlock struct {
	MeasuredAt    string  `json:"measured_at"`
	MinIntervals  int     `json:"min_intervals"`
	AcceptCVMax   float64 `json:"accept_cv_max"`
	AcceptKSAlpha float64 `json:"accept_ks_alpha"`
	Recalibration string  `json:"recalibration"`
}

type CellManifestEntry struct {
	ID                          string          `json:"id"`
	C                           int             `json:"c"`
	H                           int             `json:"h"`
	P                           int             `json:"p"`
	CeilCH                      int             `json:"ceil_ch"`
	Cadence                     int             `json:"cadence"`
	Divisible                   bool            `json:"divisible"`
	KillEligibleA               KillEligibility `json:"kill_eligible_a"`
	Role                        string          `json:"role"`
	NonDivisibleStabilityFamily bool            `json:"non_divisible_stability_family"`
}

type DecisionSurfaceSimBlock struct {
	Status           string `json:"status"`
	RegistrationHash string `json:"registration_hash"`
}

// BuildCellManifest constructs the manifest's cell table from the frozen
// lattice.
func BuildCellManifest() []CellManifestEntry {
	entries := make([]CellManifestEntry, 0, len(Lattice))
	stabilitySet := map[string]bool{}
	for _, id := range StabilityFamily {
		stabilitySet[id] = true
	}
	for _, c := range Lattice {
		entries = append(entries, CellManifestEntry{
			ID:                          c.ID,
			C:                           c.C,
			H:                           c.H,
			P:                           c.P,
			CeilCH:                      c.CeilCH(),
			Cadence:                     c.CadenceLaw(),
			Divisible:                   c.Divisible(),
			KillEligibleA:               KillEligibleA(c),
			Role:                        c.Role,
			NonDivisibleStabilityFamily: stabilitySet[c.ID],
		})
	}
	return entries
}

// NewRunID returns a timestamp-based run identifier.
func NewRunID(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, time.Now().UTC().Format("20060102T150405Z"))
}
