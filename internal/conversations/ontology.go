// ontology.go — L0/L1/L2 ontology loading and enforcement.
//
// Build order (per spec 2026-06-10):
//   L0: cogos.ontology/v1 — the meta-grammar; ONLY thing we hard-code knowledge of.
//   L1: cogos.conversations@1.0.0 — component class declarations.
//   L2: mappings/*.v1.yaml — per-source mapping rules with coverage_baseline.
//
// The observatory loads L0/L1/L2 at provider LoadConfig from
// <workspace>/.cog/observatory/ontology/ (configurable via ontology_dir).
//
// Enforcement contract (from L0 § enforcement):
//   - Records declaring an unknown (ontology id, major version) are rejected,
//     logged, counted.
//   - Components no L2 mapping speaks are quarantined with provenance (never
//     guessed, never dropped).
//   - Unmapped-component count per source is a metric.
//   - L3 records carry the (ontology, mapping) versions that produced them.
package conversations

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ─── L0 constants ─────────────────────────────────────────────────────────────

// l0GrammarVersion is the ONLY L0 grammar version this codebase speaks.
// Reject any L1 that declares a different ontology: value.
const l0GrammarVersion = "cogos.ontology/v1"

// ─── L1 types ────────────────────────────────────────────────────────────────

// OntologyDoc is the parsed form of an L1 YAML file conforming to the L0
// grammar (cogos.ontology/v1).
type OntologyDoc struct {
	// Ontology is the L0 grammar version claim ("cogos.ontology/v1").
	Ontology string `yaml:"ontology"`

	// ID is the dotted ontology identifier (e.g. "cogos.conversations").
	ID string `yaml:"id"`

	// Version is the semver of this L1 instance (e.g. "1.0.0").
	Version string `yaml:"version"`

	// Entities maps entity name → EntityDecl.
	Entities map[string]EntityDecl `yaml:"entities"`

	// Components maps component name → ComponentDecl.
	Components map[string]ComponentDecl `yaml:"components"`

	// Relations maps relation name → RelationDecl.
	Relations map[string]RelationDecl `yaml:"relations"`
}

// EntityDecl is a session-grade container declaration.
type EntityDecl struct {
	Description string   `yaml:"description"`
	Keys        []string `yaml:"keys"`
}

// ComponentDecl is a component class declaration.
type ComponentDecl struct {
	Description string            `yaml:"description"`
	Fields      map[string]string `yaml:"fields"`
	Required    []string          `yaml:"required"`
	Relations   map[string]string `yaml:"relations"`
	Policy      *ComponentPolicy  `yaml:"policy,omitempty"`
}

// ComponentPolicy carries indexing/visibility policy for a component class.
type ComponentPolicy struct {
	Index        *bool  `yaml:"index,omitempty"`
	DefaultViews string `yaml:"default_views,omitempty"`
}

// RelationDecl is a relation declaration.
type RelationDecl struct {
	Description string `yaml:"description"`
	From        string `yaml:"from"`
	To          string `yaml:"to"`
	Cardinality string `yaml:"cardinality"`
}

// majorVersion extracts the major version number from a semver string like
// "1.0.0". Returns 0 on parse failure; the caller can treat 0 as unknown.
func majorVersion(semver string) int {
	if semver == "" {
		return 0
	}
	parts := strings.SplitN(semver, ".", 3)
	n := 0
	for _, c := range parts[0] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ParseL1Ontology parses and validates an L1 YAML document. Returns an error
// if:
//   - the file does not parse as valid YAML
//   - the ontology: field declares an L0 grammar version we don't recognise
//   - required top-level fields (id, version) are absent
func ParseL1Ontology(data []byte) (*OntologyDoc, error) {
	var doc OntologyDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("ontology: yaml parse: %w", err)
	}

	// Reject unknown L0 grammar versions.
	if doc.Ontology != l0GrammarVersion {
		return nil, fmt.Errorf("ontology: unknown L0 grammar version %q (only %q is supported)", doc.Ontology, l0GrammarVersion)
	}

	if doc.ID == "" {
		return nil, fmt.Errorf("ontology: missing required field 'id'")
	}
	if doc.Version == "" {
		return nil, fmt.Errorf("ontology: missing required field 'version'")
	}

	return &doc, nil
}

// ─── L2 types ─────────────────────────────────────────────────────────────────

// MappingDoc is the parsed form of an L2 mapping YAML file.
type MappingDoc struct {
	Mapping MappingMeta `yaml:"mapping"`

	// Rules is the list of declared mapping rules (top-level, as in
	// claude-code-jsonl.v1.yaml).
	Rules []MappingRule `yaml:"rules,omitempty"`

	// CurrentMapping carries mapping metadata and rules nested under a
	// current_mapping: key (as in hermes-statedb.v1.yaml).
	// When Rules is empty, EffectiveRules() returns CurrentMapping.Rules.
	CurrentMapping *CurrentMappingSection `yaml:"current_mapping,omitempty"`

	// Unmapped is the list of explicitly unmapped source components.
	Unmapped []UnmappedEntry `yaml:"unmapped,omitempty"`

	// CoverageBaseline carries the day-one metric snapshot.
	CoverageBaseline *CoverageBaseline `yaml:"coverage_baseline,omitempty"`
}

// CurrentMappingSection represents the nested current_mapping: block used by
// the hermes-statedb mapping spec.
type CurrentMappingSection struct {
	Description string        `yaml:"description,omitempty"`
	Rules       []MappingRule `yaml:"rules,omitempty"`
}

// EffectiveRules returns the mapping rules regardless of whether they are
// declared at the top level (Rules) or nested under current_mapping (Rules is
// empty and CurrentMapping.Rules is non-empty).
func (md *MappingDoc) EffectiveRules() []MappingRule {
	if len(md.Rules) > 0 {
		return md.Rules
	}
	if md.CurrentMapping != nil {
		return md.CurrentMapping.Rules
	}
	return nil
}

// MappingMeta carries the mapping header fields.
type MappingMeta struct {
	ID         string `yaml:"id"`
	Version    string `yaml:"version"`
	Source     string `yaml:"source,omitempty"`
	Sources    []string `yaml:"sources,omitempty"`
	Ontology   string `yaml:"ontology"` // e.g. "cogos.conversations@^1"
	Status     string `yaml:"status,omitempty"`
	ParserRef  string `yaml:"parser_ref,omitempty"`
	Created    string `yaml:"created,omitempty"`
	Observer   string `yaml:"observer,omitempty"`
	IngestSchema string `yaml:"ingest_schema,omitempty"`
	RecordIdentity string `yaml:"record_identity,omitempty"`
}

// MappingRule is one declared mapping rule inside an L2 mapping.
type MappingRule struct {
	ID    string `yaml:"id"`
	Match struct {
		RecordType  string   `yaml:"record_type,omitempty"`
		Condition   string   `yaml:"condition,omitempty"`
		ContentPath string   `yaml:"content_path,omitempty"`
		BlockTypes  []string `yaml:"block_types,omitempty"`
	} `yaml:"match,omitempty"`
	Emit  map[string]any `yaml:"emit,omitempty"`
	Notes string         `yaml:"notes,omitempty"`

	// For hermes-statedb rules, which use these fields directly.
	SourceCondition string   `yaml:"source_condition,omitempty"`
	TargetClass     string   `yaml:"target_class,omitempty"`
	FieldMap        map[string]string `yaml:"field_map,omitempty"`
	Quality         string   `yaml:"quality,omitempty"`
}

// UnmappedEntry describes a source component that the mapping explicitly
// declares as unmapped (but does not silently drop — it is quarantine-bound).
type UnmappedEntry struct {
	ID              string `yaml:"id"`
	JSONLLocation   string `yaml:"jsonl_location,omitempty"`
	CorpusCount     string `yaml:"corpus_count,omitempty"`
	ParserBehavior  string `yaml:"parser_behavior,omitempty"`
	TargetL1Class   string `yaml:"target_l1_class,omitempty"`
	V2Note          string `yaml:"v2_note,omitempty"`
	SourceColumns   []string `yaml:"source_columns,omitempty"`
	Description     string `yaml:"description,omitempty"`
	RowScope        string `yaml:"row_scope,omitempty"`
	SourceCondition string `yaml:"source_condition,omitempty"`
	Quality         string `yaml:"quality,omitempty"`
}

// CoverageBaseline carries the baseline metrics from the mapping file.
type CoverageBaseline struct {
	SnapshotDate    string `yaml:"snapshot_date,omitempty"`
	SourcesMeasured []string `yaml:"sources_measured,omitempty"`
	Corpus          *struct {
		Files       int    `yaml:"files,omitempty"`
		Records     int    `yaml:"records,omitempty"`
		Bytes       int64  `yaml:"bytes,omitempty"`
		SampledDate string `yaml:"sampled_date,omitempty"`
	} `yaml:"corpus,omitempty"`
}

// ─── Loaded ontology set ─────────────────────────────────────────────────────

// LoadedOntology is the in-memory representation of a loaded L1+L2 set.
type LoadedOntology struct {
	// L1 is the parsed ontology document.
	L1 *OntologyDoc

	// L2 maps source-id → mapping document. Source-id matches the observer
	// source declaration in ingest records (e.g. "claude-code-jsonl",
	// "hermes-node-a", "hermes-cog").
	L2 map[string]*MappingDoc

	// MappedComponents is the union of L1 component names that any loaded
	// L2 mapping maps to. Used for quarantine gating.
	MappedComponents map[string]struct{}

	// OntologyRef is the canonical version reference for L3 tagging:
	// "<id>@<version>", e.g. "cogos.conversations@1.0.0".
	OntologyRef string
}

// ParseMappingDoc parses and validates an L2 YAML mapping document.
func ParseMappingDoc(data []byte) (*MappingDoc, error) {
	var doc MappingDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("mapping: yaml parse: %w", err)
	}
	if doc.Mapping.ID == "" {
		return nil, fmt.Errorf("mapping: missing required field mapping.id")
	}
	if doc.Mapping.Ontology == "" {
		return nil, fmt.Errorf("mapping: missing required field mapping.ontology")
	}
	return &doc, nil
}

// LoadOntologyDir loads L1 + L2 YAML files from ontologyDir:
//   - <ontologyDir>/*.yaml (not under mappings/) is treated as an L1 instance
//     if it declares ontology: cogos.ontology/v1 and has a non-"meta_schema" top key.
//   - <ontologyDir>/mappings/*.yaml are treated as L2 mapping docs.
//
// Non-YAML files are silently skipped. Missing ontologyDir is not an error
// (returns nil with empty maps). An L1 file failing L0 grammar validation
// returns an error.
//
// Source routing for L2: a mapping doc that declares mapping.source is keyed
// under that source id; a mapping doc with mapping.sources (list) is keyed
// under each. The hermes-statedb mapping declares sources: [hermes-node-a,
// hermes-cog], so both sources share the same mapping doc.
func LoadOntologyDir(ontologyDir string) (*LoadedOntology, error) {
	if _, err := os.Stat(ontologyDir); os.IsNotExist(err) {
		// Absent dir is OK — enforcement just won't fire.
		return &LoadedOntology{
			L2:               make(map[string]*MappingDoc),
			MappedComponents: make(map[string]struct{}),
		}, nil
	}

	lo := &LoadedOntology{
		L2:               make(map[string]*MappingDoc),
		MappedComponents: make(map[string]struct{}),
	}

	// ── Load L1 instances from the root of ontologyDir ───────────────────────
	entries, err := os.ReadDir(ontologyDir)
	if err != nil {
		return nil, fmt.Errorf("ontology: read dir %s: %w", ontologyDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(ontologyDir, e.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("ontology: read %s: %w", path, readErr)
		}

		// Quick check: does this YAML start with an 'ontology:' or 'meta_schema:' key?
		// We only parse files that look like L1 instances (have 'ontology:' field).
		var peek map[string]any
		if peekErr := yaml.Unmarshal(data, &peek); peekErr != nil {
			continue // skip non-YAML or unparseable files
		}
		if _, hasMeta := peek["meta_schema"]; hasMeta {
			continue // this is the L0 meta-schema itself, not an L1 instance
		}
		if _, hasOntology := peek["ontology"]; !hasOntology {
			continue // not an L1 ontology instance
		}

		doc, parseErr := ParseL1Ontology(data)
		if parseErr != nil {
			return nil, fmt.Errorf("ontology: parse L1 %s: %w", e.Name(), parseErr)
		}
		// Last one wins if multiple L1 files exist (unexpected, but handle cleanly).
		lo.L1 = doc
		lo.OntologyRef = doc.ID + "@" + doc.Version
	}

	// ── Load L2 mappings from <ontologyDir>/mappings/ ─────────────────────────
	mappingsDir := filepath.Join(ontologyDir, "mappings")
	mappingEntries, err := os.ReadDir(mappingsDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No mappings dir is OK.
			return lo, nil
		}
		return nil, fmt.Errorf("ontology: read mappings dir %s: %w", mappingsDir, err)
	}
	for _, e := range mappingEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(mappingsDir, e.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("ontology: read mapping %s: %w", path, readErr)
		}
		mdoc, parseErr := ParseMappingDoc(data)
		if parseErr != nil {
			return nil, fmt.Errorf("ontology: parse L2 %s: %w", e.Name(), parseErr)
		}

		// Index by source id(s).
		sources := mdoc.Mapping.Sources
		if mdoc.Mapping.Source != "" {
			sources = append(sources, mdoc.Mapping.Source)
		}
		// Fallback: use mapping.id as source key.
		if len(sources) == 0 {
			sources = []string{mdoc.Mapping.ID}
		}
		for _, src := range sources {
			lo.L2[src] = mdoc
		}

		// Accumulate mapped L1 component names from declared rules.
		for _, rule := range mdoc.EffectiveRules() {
			// claude-code-jsonl rules use emit.component; hermes uses target_class.
			if emitMap, ok := rule.Emit["component"].(string); ok && emitMap != "" {
				lo.MappedComponents[emitMap] = struct{}{}
			}
			if rule.TargetClass != "" {
				lo.MappedComponents[rule.TargetClass] = struct{}{}
			}
		}
	}

	// Always include session.turn in MappedComponents since both mappings emit it.
	lo.MappedComponents["session.turn"] = struct{}{}

	return lo, nil
}

// L3Tag is the version tagging appended to newly-ingested records per the L3
// spec: each record carries the (ontology, mapping) versions that produced it
// so re-mapping under a new version is always possible.
type L3Tag struct {
	// Ontology is the L1 version reference, e.g. "cogos.conversations@1.0.0".
	Ontology string `json:"ontology,omitempty"`
	// Mapping is the L2 mapping id + version, e.g. "claude-code-jsonl@1.0.0".
	Mapping string `json:"mapping,omitempty"`
}

// OntologyVersionCheck validates that a requested ontology= URI param value
// matches the loaded L1's (id, major version). Returns an error with an
// explicit mismatch message when they don't match.
func (lo *LoadedOntology) OntologyVersionCheck(requested string) error {
	if lo == nil || lo.L1 == nil {
		return fmt.Errorf("no ontology loaded; cannot validate ontology=%q", requested)
	}
	// requested may be "cogos.conversations@1.0.0" or "cogos.conversations@^1".
	// We validate id match and major-version compatibility.
	id, ver, _ := strings.Cut(requested, "@")
	if id != lo.L1.ID {
		return fmt.Errorf("ontology mismatch: requested %q but loaded ontology is %q", requested, lo.OntologyRef)
	}
	if ver == "" {
		return nil // no version constraint — id match is sufficient
	}
	// Strip caret prefix for semver range (e.g. "^1").
	ver = strings.TrimPrefix(ver, "^")
	reqMajor := majorVersion(ver)
	loadedMajor := majorVersion(lo.L1.Version)
	if reqMajor != 0 && reqMajor != loadedMajor {
		return fmt.Errorf("ontology major version mismatch: requested major %d but loaded %s is major %d",
			reqMajor, lo.OntologyRef, loadedMajor)
	}
	return nil
}

// ComponentClass validates that a requested component= URI param value names
// a known L1 component class. Returns an error when unknown.
func (lo *LoadedOntology) ComponentClass(requested string) error {
	if lo == nil || lo.L1 == nil {
		return fmt.Errorf("no ontology loaded; cannot validate component=%q", requested)
	}
	if _, ok := lo.L1.Components[requested]; !ok {
		known := make([]string, 0, len(lo.L1.Components))
		for k := range lo.L1.Components {
			known = append(known, k)
		}
		sortStrings(known)
		return fmt.Errorf("unknown component class %q; known classes: %s",
			requested, strings.Join(known, ", "))
	}
	return nil
}

// MappingVersionRef returns the "<id>@<version>" string for the L2 mapping
// associated with the given source, or "" when no mapping is loaded.
func (lo *LoadedOntology) MappingVersionRef(source string) string {
	if lo == nil {
		return ""
	}
	md, ok := lo.L2[source]
	if !ok {
		return ""
	}
	return md.Mapping.ID + "@" + md.Mapping.Version
}

// IsDegenerateRecord reports whether a record from source with the given role
// matches a degenerate mapping rule in the L2 spec.
//
// A rule is degenerate when MappingRule.Quality == "degenerate".  Role
// matching is derived from the rule's SourceCondition: we look for the pattern
// role = '<role>' anywhere in the condition string (case-sensitive, single
// quotes, exact role value).  This covers all current mapping rules whose
// degenerate classification is role-keyed (e.g. hermes-statedb text_tool_degenerate:
// source_condition = "role = 'tool' AND content IS NOT NULL AND content != ''").
//
// Returns false when no mapping is loaded for source, or when no degenerate
// rule matches the given role.
func (lo *LoadedOntology) IsDegenerateRecord(source, role string) bool {
	if lo == nil {
		return false
	}
	md, ok := lo.L2[source]
	if !ok {
		return false
	}
	needle := "role = '" + role + "'"
	for _, rule := range md.EffectiveRules() {
		if rule.Quality != "degenerate" {
			continue
		}
		if strings.Contains(rule.SourceCondition, needle) {
			return true
		}
	}
	return false
}
