package engine

import "strings"

type DefaultMembranePolicy struct{}

func (p DefaultMembranePolicy) Evaluate(block *CogBlock) IngestionResult {
	result := IngestionResult{Block: block}
	if block != nil {
		result.Provenance = block.Provenance
	}
	if block == nil {
		result.Decision = Discard
		result.Reason = "missing block"
		return result
	}

	content := strings.TrimSpace(blockContent(block))
	if content == "" {
		result.Decision = Discard
		result.Reason = "empty content"
		return result
	}

	// Delegate to the Kind-specific membrane policy registry (ADR-090).
	// EvaluateKindPolicy returns (result, true) if a registered policy
	// short-circuits the default logic for this Kind.
	if kindResult, handled := EvaluateKindPolicy(block); handled {
		return kindResult
	}

	if block.TrustContext.TrustScore >= 0.8 && (block.TrustContext.Scope == "local" || block.TrustContext.Scope == "workspace") {
		result.Decision = Integrate
		result.Reason = "trusted local workspace traffic"
		return result
	}

	result.Decision = Defer
	result.Reason = "requires review"
	return result
}

func blockContent(block *CogBlock) string {
	if block == nil {
		return ""
	}
	var content strings.Builder
	content.WriteString(block.SystemPrompt)
	for _, message := range block.Messages {
		content.WriteString(message.Content)
	}
	return content.String()
}
