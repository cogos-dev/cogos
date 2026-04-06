//go:build mcpserver

package main

type AssimilationDecision string

const (
	Integrate  AssimilationDecision = "integrate"
	Quarantine AssimilationDecision = "quarantine"
	Defer      AssimilationDecision = "defer"
	Discard    AssimilationDecision = "discard"
)

type IngestionResult struct {
	Decision         AssimilationDecision `json:"decision"`
	Block            *CogBlock            `json:"block,omitempty"`
	Reason           string               `json:"reason,omitempty"`
	Provenance       BlockProvenance      `json:"provenance"`
	QuarantineReason string               `json:"quarantine_reason,omitempty"`
}

type MembranePolicy interface {
	Evaluate(block *CogBlock) IngestionResult
}
