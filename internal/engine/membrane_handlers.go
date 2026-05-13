// membrane_handlers.go — Kind-specific membrane policy registrations (ADR-090).
//
// Each Kind that has a non-default membrane policy registers a handler here
// via membraneKindPolicies. The DefaultMembranePolicy calls EvaluateKindPolicy
// before falling through to the general trust-score logic.
//
// To add a new Kind-specific membrane policy (e.g. for session.fork):
//
//  1. Define a function of type MembraneKindPolicy below.
//  2. Add it to the membraneKindPolicies map in init().
//
// RFC-0005 and RFC-0006 implementers: register your Kind policies here rather
// than adding switch arms to DefaultMembranePolicy.Evaluate.
package engine

import "strings"

// MembraneKindPolicy is a Kind-specific ingestion policy override.
// Return (result, true) to short-circuit the default policy with result.
// Return (IngestionResult{}, false) to fall through to the default logic.
type MembraneKindPolicy func(block *CogBlock) (IngestionResult, bool)

var membraneKindPolicies = map[CogBlockKind]MembraneKindPolicy{}

// EvaluateKindPolicy looks up and invokes the Kind-specific membrane policy
// for block.Kind. Returns (result, true) if a policy matched and short-circuited;
// returns (IngestionResult{}, false) otherwise.
func EvaluateKindPolicy(block *CogBlock) (IngestionResult, bool) {
	if block == nil {
		return IngestionResult{}, false
	}
	policy, ok := membraneKindPolicies[block.Kind]
	if !ok {
		return IngestionResult{}, false
	}
	return policy(block)
}

func init() {
	// BlockToolResult: trusted when it arrives via the MCP transport.
	membraneKindPolicies[BlockToolResult] = func(block *CogBlock) (IngestionResult, bool) {
		if block.SourceChannel == "mcp" || block.SourceTransport == "mcp" || block.Provenance.NormalizedBy == "mcp" {
			return IngestionResult{
				Block:      block,
				Provenance: block.Provenance,
				Decision:   Integrate,
				Reason:     "trusted mcp tool result",
			}, true
		}
		return IngestionResult{}, false
	}

	// BlockImport: quarantine when provenance channel is unknown.
	membraneKindPolicies[BlockImport] = func(block *CogBlock) (IngestionResult, bool) {
		if strings.TrimSpace(block.Provenance.OriginChannel) == "" {
			return IngestionResult{
				Block:            block,
				Provenance:       block.Provenance,
				Decision:         Quarantine,
				Reason:           "external import missing provenance",
				QuarantineReason: "unknown provenance",
			}, true
		}
		return IngestionResult{}, false
	}
}
