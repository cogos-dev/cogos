package uri

// Namespaces defines all valid cog:// namespaces.
// Each namespace maps to a projection in the kernel that resolves
// the URI to a concrete filesystem location.
var Namespaces = map[string]bool{
	// Core namespaces
	"mem":       true, // cog://mem/* → CogDocs memory corpus
	"signals":   true, // cog://signals/* → Signal field
	"context":   true, // cog://context → 4-tier context assembly
	"thread":    true, // cog://thread/* → Conversation threads
	"coherence": true, // cog://coherence → Coherence state
	"identity":  true, // cog://identity → Workspace identity
	"src":       true, // cog://src → SRC constants
	"adr":       true, // cog://adr/* → Architecture Decision Records
	"ledger":    true, // cog://ledger/* → Event ledger
	"inference": true, // cog://inference → Inference endpoint
	"kernel":    true, // cog://kernel/* → Kernel internal paths
	"hooks":     true, // cog://hooks/* → Hook definitions

	// Extended namespaces (from kernel projections)
	"spec":      true, // cog://spec/* → Specifications
	"specs":     true, // cog://specs/* → Specifications (plural alias)
	"status":    true, // cog://status/* → Status snapshots (JSON)
	"canonical": true, // cog://canonical → Holographic baseline hash
	"handoff":   true, // cog://handoff/* → Handoff documents
	"handoffs":  true, // cog://handoffs/* → Handoffs (plural alias)
	"crystal":   true, // cog://crystal → Ledger crystal state
	"role":      true, // cog://role/* → Role definitions
	"roles":     true, // cog://roles/* → Roles (plural alias)
	"skill":     true, // cog://skill/* → Skill definitions
	"skills":    true, // cog://skills/* → Skills (plural alias)
	"agent":     true, // cog://agent/* → Agent definitions
	"agents":    true, // cog://agents/* → Agents (plural alias)

	// Resource namespaces (from engine projections)
	"conf":      true, // cog://conf/* → Configuration files
	"config":    true, // cog://config/* → Configuration (alias)
	"ontology":  true, // cog://ontology/* → Ontology definitions
	"work":      true, // cog://work/* → Work items
	"artifact":  true, // cog://artifact/* → Artifacts
	"artifacts": true, // cog://artifacts/* → Artifacts (plural alias)
	"docs":      true, // cog://docs/* → Documentation
}

// IsValidNamespace reports whether ns is a recognized cog:// namespace.
func IsValidNamespace(ns string) bool {
	return Namespaces[ns]
}
