//go:build mcpserver

package main

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

	if block.Kind == BlockToolResult && (block.SourceChannel == "mcp" || block.SourceTransport == "mcp" || block.Provenance.NormalizedBy == "mcp") {
		result.Decision = Integrate
		result.Reason = "trusted mcp tool result"
		return result
	}

	if block.Kind == BlockImport && strings.TrimSpace(block.Provenance.OriginChannel) == "" {
		result.Decision = Quarantine
		result.Reason = "external import missing provenance"
		result.QuarantineReason = "unknown provenance"
		return result
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
