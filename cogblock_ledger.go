package main

import (
	"fmt"
	"log/slog"
	"time"
)

// RecordBlock writes a CogBlock to the process ledger and returns the ledger ref.
func (p *Process) RecordBlock(block *CogBlock) string {
	if block == nil {
		return ""
	}

	if block.SessionID == "" {
		block.SessionID = p.sessionID
	}

	data := map[string]interface{}{
		"block_id":         block.ID,
		"kind":             string(block.Kind),
		"source_channel":   block.SourceChannel,
		"source_transport": block.SourceTransport,
		"source_identity":  block.SourceIdentity,
		"target_identity":  block.TargetIdentity,
		"workspace_id":     block.WorkspaceID,
		"message_count":    len(block.Messages),
	}

	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      "cogblock.ingest",
			Timestamp: block.Timestamp.UTC().Format(time.RFC3339),
			SessionID: block.SessionID,
			Data:      data,
		},
		Metadata: EventMetadata{
			Source: "kernel-v3",
		},
	}

	if env.HashedPayload.Timestamp == "" {
		env.HashedPayload.Timestamp = nowISO()
	}

	if err := AppendEvent(p.cfg.WorkspaceRoot, block.SessionID, env); err != nil {
		slog.Debug("process: cogblock ledger append failed", "err", fmt.Sprintf("%v", err))
		return ""
	}

	block.LedgerRef = env.Metadata.Hash
	block.Artifacts = append(block.Artifacts, BlockArtifact{
		Kind: "ledger_entry",
		Ref:  env.Metadata.Hash,
	})
	return block.LedgerRef
}
