package projection

// Event is one compiled distinction projected from a reflective cogdoc.
//
// Per ADR projection-compiler-primitive §4, each compiled event encodes a
// single load-bearing distinction extracted from a source cogdoc block.
// Events are cogblock-shaped: idempotency-keyed by ContentHash, addressable
// by Source URI, and emitted to the Observatory event bus.
//
// Two emission modes share this type:
//
//   - Pointer mode (free): for unchanged blocks, the compiler emits an Event
//     whose CompileModel is "pointer" and whose ContentHash matches a prior
//     event. No LLM call; no thermodynamic cost.
//   - Extraction mode (cost): for new or changed blocks with explicit
//     boundaries, the compiler emits one Event per boundary via structural
//     parsing (v0: no LLM). CompileModel is "structural".
type Event struct {
	// Source is the cog:// URI of the source distinction, e.g.
	// "cog://mem/reflective/<slug>#distinction-N" or "#quote-N".
	Source string `json:"source"`

	// Distinction is the load-bearing claim, one sentence to one short
	// paragraph. For quote-boundary blocks this is the blockquote text;
	// for Distinction-N sections this is the heading text after the
	// "Distinction N:" prefix.
	Distinction string `json:"distinction"`

	// Relations are cog:// URIs referenced or implied by the distinction.
	// v0 inherits relations from the source cogdoc's refs/related/relates-to
	// frontmatter fields. Per-distinction inference is deferred.
	Relations []string `json:"relations,omitempty"`

	// Salience is "high", "medium", or "low", inherited from the source
	// cogdoc's frontmatter salience field. Per-distinction override is
	// deferred.
	Salience string `json:"salience,omitempty"`

	// Tags inherits the source cogdoc's frontmatter tags. Per-distinction
	// tag extraction is deferred.
	Tags []string `json:"tags,omitempty"`

	// Authors carries the source cogdoc's frontmatter authors field.
	Authors []string `json:"authors,omitempty"`

	// Date is the source cogdoc's date (created or date field) in
	// ISO 8601. This is the date the distinction was articulated, not
	// the date it was compiled.
	Date string `json:"date,omitempty"`

	// ContentHash is the idempotency key: sha256 over
	// (Source + Distinction + Relations). Re-running the compiler on
	// unchanged content emits Events with identical ContentHash values.
	ContentHash string `json:"content_hash"`

	// LedgerHash is the chain-hash assigned by the cogblock ledger on
	// emission. Empty until the event reaches the ledger writer.
	LedgerHash string `json:"ledger_hash,omitempty"`

	// CompileModel records the compilation strategy:
	//   - "pointer"    — unchanged content; no LLM call
	//   - "structural" — explicit-boundary extraction (v0 path)
	//   - "gemma4:e4b" / "llama3.3:70b" / ... — LLM extraction (deferred)
	CompileModel string `json:"compile_model"`

	// SourceFile is the absolute filesystem path of the source cogdoc.
	// Carried for ledger-side audit; not part of ContentHash.
	SourceFile string `json:"source_file,omitempty"`
}

// FrictionEvent records a contradiction, refinement, or supersession
// detected between two compiled Events. Per ADR §6, friction events flow
// to the foveation engine's attention stack with elevated priority; they
// do not suppress either A or B.
//
// v0 of the Projection Compiler does NOT emit FrictionEvents — the
// coherence-checker pass is deferred. The type is defined here so callers
// (Observatory, foveation engine) can wire against the stable contract.
type FrictionEvent struct {
	// A is the source URI of the prior event in the pair.
	A string `json:"a"`

	// B is the source URI of the new event in the pair.
	B string `json:"b"`

	// Kind is "contradiction", "refinement", or "supersession".
	Kind string `json:"kind"`

	// Salience is max(A.salience, B.salience) per ADR §6.
	Salience string `json:"salience,omitempty"`
}
