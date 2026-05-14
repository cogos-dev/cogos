// cogdoc_review_types.go
// CogdocReview Reconcilable type design: Class, Claim, and supporting schema.
//
// The CogdocReviewReconciler enforces the deterministic review pipeline
// for cogdoc authoring. Before a proposed cogdoc reaches the constellation
// index, the Reconciler runs a grep-embed similarity search against the
// existing corpus and requires explicit acknowledgment per candidate.
//
// RFC-034 Reconcilable Binding Pattern shape:
//   - Class:               CogdocReviewClass (review policy declaration)
//   - Claim:               CogdocProposal (proposed cogdoc intent)
//   - PhysicalInstantiation: the committed cogdoc with provenance recorded
//   - Reconciler:          CogdocReviewReconciler (the gate)
//
// See also:
//   - pkg/reconcile/types.go (Reconcilable interface)
//   - pkg/cogblock/cogdoc_review.go (Reconciler implementation, T04)
//   - ADR-052 (executable cogdoc workflow primitive)
//   - RFC-034 (Reconcilable Binding Pattern)

package reconcile

import "time"

// --- Class (review policy) ---

// CogdocReviewClass declares the review policy for a workspace.
// It maps to the "Class" in the RFC-034 Reconcilable Binding Pattern.
// One Class per workspace; loaded from .cog/hooks/hook-config.yaml
// under the cogdoc_review key.
type CogdocReviewClass struct {
	// Enabled turns the gate on or off.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// SimilarityThreshold is the cosine-similarity score above which a
	// candidate triggers the forced-ACK gate. Range [0.0, 1.0].
	// Default: 0.70 (calibrated in T16).
	SimilarityThreshold float64 `yaml:"threshold" json:"threshold"`

	// TopN is the maximum number of similar candidates to surface.
	// Default: 5.
	TopN int `yaml:"top_n" json:"top_n"`

	// EmbedModel overrides the default embedding model.
	// Empty means use the workspace default (bge-m3:latest).
	EmbedModel string `yaml:"embed_model" json:"embed_model,omitempty"`

	// OllamaEndpoint overrides the Ollama server URL.
	// Empty means http://localhost:11434.
	OllamaEndpoint string `yaml:"ollama_endpoint" json:"ollama_endpoint,omitempty"`

	// CorpusPaths are the root directories to search for existing cogdocs.
	// Relative paths are resolved from the workspace root.
	// Default: [".cog/mem/"] if empty.
	CorpusPaths []string `yaml:"corpus_paths" json:"corpus_paths,omitempty"`
}

// DefaultCogdocReviewClass returns a CogdocReviewClass with safe defaults.
func DefaultCogdocReviewClass() CogdocReviewClass {
	return CogdocReviewClass{
		Enabled:             true,
		SimilarityThreshold: 0.70,
		TopN:                5,
	}
}

// --- Claim (proposed cogdoc) ---

// CogdocProposal is the "Claim" in the RFC-034 Binding Pattern.
// It represents the author's intent to create a new cogdoc.
// The Reconciler validates the proposal before it becomes a committed cogdoc.
type CogdocProposal struct {
	// ProposedID is the intended cogdoc ID (e.g., "my-new-insight").
	ProposedID string `json:"proposed_id"`

	// Title is the proposed cogdoc title (used as the primary query text).
	Title string `json:"title"`

	// Type is the cogdoc type (insight, adr, rfc, workflow, etc.).
	Type string `json:"type"`

	// Abstract is a short description of the proposed cogdoc's content.
	// Used alongside Title for embedding-based similarity search.
	Abstract string `json:"abstract,omitempty"`

	// Tags are the proposed frontmatter tags.
	Tags []string `json:"tags,omitempty"`

	// FilePath is the workspace-relative path where the cogdoc will be written.
	FilePath string `json:"file_path,omitempty"`

	// ProposedAt is when the proposal was made.
	ProposedAt time.Time `json:"proposed_at"`

	// SessionID traces the authoring session.
	SessionID string `json:"session_id,omitempty"`

	// OperatorID is the identity of the author.
	OperatorID string `json:"operator_id,omitempty"`
}

// QueryText returns the text used for embedding-based similarity search.
// Combines title and abstract for maximum signal.
func (p CogdocProposal) QueryText() string {
	if p.Abstract != "" {
		return p.Title + "\n\n" + p.Abstract
	}
	return p.Title
}

// --- SimilarityCandidate ---

// SimilarityCandidate is a single result from the similarity search.
// The Reconciler surfaces the top-N candidates above threshold.
type SimilarityCandidate struct {
	// CogdocID is the ID from the candidate's frontmatter.
	CogdocID string `json:"cogdoc_id"`

	// FilePath is the workspace-relative path of the candidate.
	FilePath string `json:"file_path"`

	// Title is the candidate's frontmatter title.
	Title string `json:"title"`

	// Type is the candidate's cogdoc type.
	Type string `json:"type"`

	// Score is the cosine similarity score [0.0, 1.0].
	Score float64 `json:"score"`

	// StructuralRelationship is an optional description of the semantic
	// relationship between the proposal and this candidate.
	// Set during the acknowledgment phase by the skill layer.
	StructuralRelationship string `json:"structural_relationship,omitempty"`
}

// --- Acknowledgment ---

// AcknowledgmentDecision is the author's decision for a single candidate.
type AcknowledgmentDecision string

const (
	// AckReadDistinct means the author has read the candidate and determined
	// the proposed cogdoc is genuinely distinct. Authoring proceeds.
	AckReadDistinct AcknowledgmentDecision = "read+distinct"

	// AckAmendInstead means the author will amend the candidate rather than
	// creating a new document. Routes to the amendment workflow.
	AckAmendInstead AcknowledgmentDecision = "amend-instead"
)

// CandidateAcknowledgment records the author's decision for one candidate.
type CandidateAcknowledgment struct {
	// CogdocID of the candidate being acknowledged.
	CogdocID string `json:"cogdoc_id"`

	// Decision is the author's choice.
	Decision AcknowledgmentDecision `json:"decision"`

	// Rationale is optional free-text explaining the decision.
	Rationale string `json:"rationale,omitempty"`

	// AcknowledgedAt is the timestamp of the acknowledgment.
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}

// --- ActionTaken ---

// ReviewActionTaken records what happened after all acknowledgments.
type ReviewActionTaken string

const (
	// ReviewActionAuthored means the proposal passed review and was authored.
	ReviewActionAuthored ReviewActionTaken = "authored"

	// ReviewActionAmended means at least one candidate was "amend-instead" and
	// the workflow was routed to the amendment flow.
	ReviewActionAmended ReviewActionTaken = "amended"

	// ReviewActionAbandoned means the author abandoned the proposal.
	ReviewActionAbandoned ReviewActionTaken = "abandoned"
)

// --- ProvenanceRecord (PhysicalInstantiation metadata) ---

// ProvenanceRecord is written to the cogdoc frontmatter's `provenance` field
// after all acknowledgments are complete. It is the record that the review
// pipeline ran and the gate was satisfied.
//
// This is the "PhysicalInstantiation" evidence in RFC-034 terms:
// the committed cogdoc with this provenance embedded is the physical
// manifestation of the reviewed + acknowledged proposal.
type ProvenanceRecord struct {
	// PipelineVersion identifies the review pipeline version that ran.
	PipelineVersion string `json:"pipeline_version" yaml:"pipeline_version"`

	// ProposedAt is when the proposal was submitted to the pipeline.
	ProposedAt time.Time `json:"proposed_at" yaml:"proposed_at"`

	// ReviewedAt is when all acknowledgments were complete.
	ReviewedAt time.Time `json:"reviewed_at" yaml:"reviewed_at"`

	// CandidatesSurfaced is the count of candidates above threshold.
	CandidatesSurfaced int `json:"candidates_surfaced" yaml:"candidates_surfaced"`

	// Acknowledgments records the per-candidate decisions.
	Acknowledgments []CandidateAcknowledgment `json:"acknowledgments" yaml:"acknowledgments"`

	// ActionTaken is the final authoring decision.
	ActionTaken ReviewActionTaken `json:"action_taken" yaml:"action_taken"`

	// SessionID traces the authoring session.
	SessionID string `json:"session_id,omitempty" yaml:"session_id,omitempty"`
}

// AllDistinct returns true if all acknowledgments are "read+distinct".
func (p ProvenanceRecord) AllDistinct() bool {
	for _, ack := range p.Acknowledgments {
		if ack.Decision != AckReadDistinct {
			return false
		}
	}
	return true
}

// HasAmendDecision returns true if any candidate was marked "amend-instead".
func (p ProvenanceRecord) HasAmendDecision() bool {
	for _, ack := range p.Acknowledgments {
		if ack.Decision == AckAmendInstead {
			return true
		}
	}
	return false
}

// --- TRM Training Tuple ---

// ReviewTRMTuple is the labeled data record emitted by the pipeline for
// TRM training. See insight cogdoc §TRM Training Signal for the schema design.
//
// Forward work: the TRM hookup is not implemented in this wave.
// The struct is defined now to ensure schema stability before training data
// accumulates.
type ReviewTRMTuple struct {
	// ProposedCogdocID is the ID of the proposed cogdoc.
	ProposedCogdocID string `json:"proposed_cogdoc_id"`

	// QueryText is the title + abstract used for similarity search.
	QueryText string `json:"query_text"`

	// CandidatesSurfaced are the top-N candidates returned by the search.
	CandidatesSurfaced []SimilarityCandidate `json:"candidates_surfaced"`

	// Acknowledgments are the per-candidate decisions.
	Acknowledgments []CandidateAcknowledgment `json:"acknowledgments"`

	// ActionTaken is the final authoring action.
	ActionTaken ReviewActionTaken `json:"action_taken"`

	// SessionID traces the authoring session.
	SessionID string `json:"session_id,omitempty"`

	// OperatorID is the author's identity.
	OperatorID string `json:"operator_id,omitempty"`

	// RecordedAt is when the tuple was emitted.
	RecordedAt time.Time `json:"recorded_at"`
}
