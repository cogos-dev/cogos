// registry_indexer.go - Index operational state into constellation for unified search
//
// Projects all CogOS subsystem state (fleet, research, agent CRDs, service CRDs,
// coordination, coherence, signals) into the constellation knowledge graph.
// Each subsystem keeps its existing file persistence; constellation is the
// unified read/search/graph layer.
//
// Pattern: follows bus_indexer.go / constellation_sessions.go — deterministic IDs,
// synthetic paths, INSERT OR REPLACE into documents + FTS.

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/myrgic/cogos/sdk/constellation"
)

// ---------------------------------------------------------------------------
// Shared helper
// ---------------------------------------------------------------------------

// regDocEntry is the common shape for all registry documents.
type regDocEntry struct {
	ID      string // deterministic document ID (e.g. "fleet:abc123")
	Path    string // synthetic path for uniqueness constraint
	DocType string // document type (e.g. "fleet_entry", "agent_crd")
	Title   string // search result display title
	Content string // searchable text content
	Tags    string // space-separated tags
	Sector  string // typically "operational"
	Created string // RFC3339
	Updated string // RFC3339
}

// registryEdge represents a typed relationship between documents.
type registryEdge struct {
	SourceID  string
	TargetURI string
	Relation  string
}

// indexRegistryEntry inserts a single registry entry into constellation.
// Idempotent via INSERT OR REPLACE keyed on deterministic ID.
func indexRegistryEntry(c *constellation.Constellation, e regDocEntry) error {
	if e.Content == "" {
		return nil
	}

	now := time.Now().Format(time.RFC3339)
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(e.Content)))[:32]
	wordCount := len(strings.Fields(e.Content))
	lineCount := strings.Count(e.Content, "\n") + 1
	contentBytes := len(e.Content)

	if e.Sector == "" {
		e.Sector = "operational"
	}
	if e.Created == "" {
		e.Created = now
	}
	if e.Updated == "" {
		e.Updated = e.Created
	}

	_, err := c.DB().Exec(`
		INSERT OR REPLACE INTO documents (
			id, path, type, title, created, updated, sector, status,
			content, content_hash, word_count, line_count,
			indexed_at, file_mtime,
			frontmatter_bytes, content_bytes, substance_ratio, ref_count, ref_density
		) VALUES (?, ?, ?, ?, ?, ?, ?, '',
			?, ?, ?, ?,
			?, ?,
			0, ?, 1.0, 0, 0.0)
	`, e.ID, e.Path, e.DocType, e.Title, e.Created, e.Updated, e.Sector,
		e.Content, contentHash, wordCount, lineCount,
		now, e.Updated,
		contentBytes)
	if err != nil {
		return fmt.Errorf("insert %s: %w", e.ID, err)
	}

	// FTS (delete + insert since FTS5 doesn't support INSERT OR REPLACE)
	if _, err := c.DB().Exec("DELETE FROM documents_fts WHERE id = ?", e.ID); err != nil {
		return fmt.Errorf("clear FTS %s: %w", e.ID, err)
	}
	_, err = c.DB().Exec(`
		INSERT INTO documents_fts(id, title, content, tags, sector, type)
		VALUES (?, ?, ?, ?, ?, ?)
	`, e.ID, e.Title, e.Content, e.Tags, e.Sector, e.DocType)
	if err != nil {
		return fmt.Errorf("insert FTS %s: %w", e.ID, err)
	}

	return nil
}

// indexRegistryEdge inserts a typed edge between documents.
func indexRegistryEdge(c *constellation.Constellation, edge registryEdge) error {
	_, err := c.DB().Exec(`
		INSERT OR REPLACE INTO doc_references (source_id, target_uri, relation)
		VALUES (?, ?, ?)
	`, edge.SourceID, edge.TargetURI, edge.Relation)
	return err
}

// ---------------------------------------------------------------------------
// Fleet indexer
// ---------------------------------------------------------------------------

// indexFleetRegistry indexes all fleet entries from the registry.
func indexFleetRegistry(c *constellation.Constellation, root string) (int, error) {
	registry, err := loadFleetRegistry(root)
	if err != nil {
		return 0, fmt.Errorf("load fleet registry: %w", err)
	}

	// Also try .cog/run/fleet/registry.json (actual location on some workspaces)
	if len(registry.Fleets) == 0 {
		altPath := filepath.Join(root, ".cog", "run", "fleet", "registry.json")
		if data, err := os.ReadFile(altPath); err == nil {
			var altRegistry FleetRegistry
			if err := json.Unmarshal(data, &altRegistry); err == nil && len(altRegistry.Fleets) > 0 {
				registry = &altRegistry
			}
		}
	}

	count := 0
	for _, fleet := range registry.Fleets {
		docID := fmt.Sprintf("fleet:%s", fleet.ID)

		// Build searchable content from fleet metadata
		var content strings.Builder
		fmt.Fprintf(&content, "Fleet: %s\n", fleet.ID)
		fmt.Fprintf(&content, "Config: %s\n", fleet.Config)
		fmt.Fprintf(&content, "State: %s\n", fleet.State)
		fmt.Fprintf(&content, "Agents: %d (completed: %d, failed: %d)\n",
			fleet.AgentCount, fleet.Completed, fleet.Failed)
		if fleet.Task != "" {
			// Truncate task to keep content reasonable
			task := fleet.Task
			if len(task) > 2000 {
				task = task[:2000] + "..."
			}
			fmt.Fprintf(&content, "\nTask:\n%s\n", task)
		}

		tags := strings.Join([]string{
			"fleet", string(fleet.State), fleet.Config,
		}, " ")

		title := fmt.Sprintf("Fleet %s (%s) — %s", fleet.ID, fleet.Config, fleet.State)

		err := indexRegistryEntry(c, regDocEntry{
			ID:      docID,
			Path:    fmt.Sprintf(".cog/run/fleet/%s", fleet.ID),
			DocType: "fleet_entry",
			Title:   title,
			Content: content.String(),
			Tags:    tags,
			Created: fleet.CreatedAt,
			Updated: fleet.UpdatedAt,
		})
		if err != nil {
			return count, err
		}
		count++
	}

	return count, nil
}

// ---------------------------------------------------------------------------
// Research indexer
// ---------------------------------------------------------------------------

// indexResearchRuns indexes all research runs and their experiments.
func indexResearchRuns(c *constellation.Constellation, root string) (int, error) {
	runs, err := listResearchRuns(root)
	if err != nil {
		return 0, fmt.Errorf("list research runs: %w", err)
	}

	count := 0
	for _, run := range runs {
		docID := fmt.Sprintf("research_run:%s", run.ID)

		var content strings.Builder
		fmt.Fprintf(&content, "Research Run: %s\n", run.ID)
		fmt.Fprintf(&content, "State: %s\n", run.State)
		fmt.Fprintf(&content, "Branch: %s\n", run.Branch)
		fmt.Fprintf(&content, "Program: %s\n", run.Program)
		fmt.Fprintf(&content, "Experiments: %d (kept: %d, discarded: %d, crashed: %d)\n",
			run.Experiments, run.Kept, run.Discarded, run.Crashed)
		if run.BestRWE > 0 {
			fmt.Fprintf(&content, "Best RWE: %.1f (commit: %s)\n", run.BestRWE, run.BestCommit)
		}
		if run.BaselineRWE > 0 {
			fmt.Fprintf(&content, "Baseline RWE: %.1f\n", run.BaselineRWE)
		}

		tags := strings.Join([]string{
			"research", string(run.State), run.Branch,
		}, " ")

		title := fmt.Sprintf("Research %s — %s (best RWE: %.1f)", run.ID, run.State, run.BestRWE)

		err := indexRegistryEntry(c, regDocEntry{
			ID:      docID,
			Path:    fmt.Sprintf(".cog/run/research/runs/%s/state.json", run.ID),
			DocType: "research_run",
			Title:   title,
			Content: content.String(),
			Tags:    tags,
			Created: run.CreatedAt,
			Updated: run.UpdatedAt,
		})
		if err != nil {
			return count, err
		}
		count++

		// Index individual experiments
		results, err := loadExperimentResults(root, run.ID)
		if err != nil {
			continue // skip experiments if results file missing
		}

		for _, exp := range results {
			expID := fmt.Sprintf("research_exp:%s:%s", run.ID, exp.Commit)

			var expContent strings.Builder
			fmt.Fprintf(&expContent, "Experiment: %s\n", exp.Commit)
			fmt.Fprintf(&expContent, "Status: %s\n", exp.Status)
			fmt.Fprintf(&expContent, "RWE: %.1f\n", exp.RWE)
			fmt.Fprintf(&expContent, "Retention: %.3f\n", exp.Retention)
			fmt.Fprintf(&expContent, "Mean Tokens: %d\n", exp.MeanTokens)
			if exp.Description != "" {
				fmt.Fprintf(&expContent, "Description: %s\n", exp.Description)
			}

			expTags := strings.Join([]string{
				"research", "experiment", exp.Status, exp.Commit,
			}, " ")

			expTitle := fmt.Sprintf("Experiment %s — %s (RWE: %.1f)", exp.Commit, exp.Status, exp.RWE)

			err := indexRegistryEntry(c, regDocEntry{
				ID:      expID,
				Path:    fmt.Sprintf(".cog/run/research/runs/%s/results.tsv#%s", run.ID, exp.Commit),
				DocType: "research_experiment",
				Title:   expTitle,
				Content: expContent.String(),
				Tags:    expTags,
				Created: exp.Timestamp,
				Updated: exp.Timestamp,
			})
			if err != nil {
				continue
			}

			// Edge: experiment belongs_to run
			indexRegistryEdge(c, registryEdge{
				SourceID:  expID,
				TargetURI: docID,
				Relation:  "belongs_to",
			})
			count++
		}
	}

	return count, nil
}

// ---------------------------------------------------------------------------
// Agent CRD indexer
// ---------------------------------------------------------------------------

// indexAgentCRDs indexes agent definitions from .cog/bin/agents/definitions/.
func indexAgentCRDs(c *constellation.Constellation, root string) (int, error) {
	agentDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read agent definitions: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".agent.yaml") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(agentDir, entry.Name()))
		if err != nil {
			continue
		}

		// Extract name from filename (e.g. "spark.agent.yaml" -> "spark")
		name := strings.TrimSuffix(entry.Name(), ".agent.yaml")
		docID := fmt.Sprintf("agent_crd:%s", name)

		content := string(data)
		info, _ := entry.Info()
		mtime := time.Now().Format(time.RFC3339)
		if info != nil {
			mtime = info.ModTime().Format(time.RFC3339)
		}

		// Extract identity card URI for edge
		identityURI := ""
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "card:") {
				identityURI = strings.TrimSpace(strings.TrimPrefix(line, "card:"))
				break
			}
		}

		// Build tags from labels and capabilities
		tags := []string{"agent", "crd", name}
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "tier:") {
				tags = append(tags, strings.TrimSpace(strings.TrimPrefix(line, "tier:")))
			}
			if strings.HasPrefix(line, "domain:") {
				tags = append(tags, strings.TrimSpace(strings.TrimPrefix(line, "domain:")))
			}
		}

		title := fmt.Sprintf("Agent CRD: %s", name)

		err = indexRegistryEntry(c, regDocEntry{
			ID:      docID,
			Path:    fmt.Sprintf(".cog/bin/agents/definitions/%s", entry.Name()),
			DocType: "agent_crd",
			Title:   title,
			Content: content,
			Tags:    strings.Join(tags, " "),
			Created: mtime,
			Updated: mtime,
		})
		if err != nil {
			return count, err
		}

		// Edge: agent has_identity -> identity card
		if identityURI != "" {
			indexRegistryEdge(c, registryEdge{
				SourceID:  docID,
				TargetURI: identityURI,
				Relation:  "has_identity",
			})
		}
		count++
	}

	return count, nil
}

// ---------------------------------------------------------------------------
// Service CRD indexer
// ---------------------------------------------------------------------------

// indexServiceCRDs indexes service definitions from .cog/config/services/.
func indexServiceCRDs(c *constellation.Constellation, root string) (int, error) {
	serviceDir := filepath.Join(root, ".cog", "config", "services")
	entries, err := os.ReadDir(serviceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read service definitions: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".service.yaml") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(serviceDir, entry.Name()))
		if err != nil {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".service.yaml")
		docID := fmt.Sprintf("service_crd:%s", name)

		content := string(data)
		info, _ := entry.Info()
		mtime := time.Now().Format(time.RFC3339)
		if info != nil {
			mtime = info.ModTime().Format(time.RFC3339)
		}

		tags := []string{"service", "crd", name}
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "domain:") {
				tags = append(tags, strings.TrimSpace(strings.TrimPrefix(line, "domain:")))
			}
		}

		title := fmt.Sprintf("Service CRD: %s", name)

		err = indexRegistryEntry(c, regDocEntry{
			ID:      docID,
			Path:    fmt.Sprintf(".cog/config/services/%s", entry.Name()),
			DocType: "service_crd",
			Title:   title,
			Content: content,
			Tags:    strings.Join(tags, " "),
			Created: mtime,
			Updated: mtime,
		})
		if err != nil {
			return count, err
		}
		count++
	}

	return count, nil
}

// ---------------------------------------------------------------------------
// Coordination indexers
// ---------------------------------------------------------------------------

// indexCoordinationClaims indexes active file claims.
func indexCoordinationClaims(c *constellation.Constellation, root string) (int, error) {
	claimsDir := filepath.Join(root, ".cog", "claims")
	entries, err := os.ReadDir(claimsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read claims: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".claim") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(claimsDir, entry.Name()))
		if err != nil {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".claim")
		docID := fmt.Sprintf("claim:%s", name)

		var claim struct {
			Path      string `json:"path"`
			Agent     string `json:"agent"`
			Reason    string `json:"reason"`
			Timestamp string `json:"timestamp"`
		}
		json.Unmarshal(data, &claim)

		var content strings.Builder
		fmt.Fprintf(&content, "Claim: %s\n", name)
		fmt.Fprintf(&content, "Path: %s\n", claim.Path)
		fmt.Fprintf(&content, "Agent: %s\n", claim.Agent)
		fmt.Fprintf(&content, "Reason: %s\n", claim.Reason)

		tags := strings.Join([]string{"coordination", "claim", claim.Agent}, " ")
		title := fmt.Sprintf("Claim: %s by %s", claim.Path, claim.Agent)

		err = indexRegistryEntry(c, regDocEntry{
			ID:      docID,
			Path:    fmt.Sprintf(".cog/claims/%s", entry.Name()),
			DocType: "coordination_claim",
			Title:   title,
			Content: content.String(),
			Tags:    tags,
			Created: claim.Timestamp,
			Updated: claim.Timestamp,
		})
		if err != nil {
			return count, err
		}

		// Edge: claim locked_by agent
		if claim.Agent != "" {
			indexRegistryEdge(c, registryEdge{
				SourceID:  docID,
				TargetURI: fmt.Sprintf("agent_crd:%s", claim.Agent),
				Relation:  "locked_by",
			})
		}
		count++
	}

	return count, nil
}

// indexCoordinationHandoffs indexes agent-to-agent handoffs.
func indexCoordinationHandoffs(c *constellation.Constellation, root string) (int, error) {
	handoffDir := filepath.Join(root, ".cog", "handoffs")
	entries, err := os.ReadDir(handoffDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read handoffs: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(handoffDir, entry.Name()))
		if err != nil {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".json")
		docID := fmt.Sprintf("handoff:%s", name)

		var handoff struct {
			From      string `json:"from"`
			To        string `json:"to"`
			Artifact  string `json:"artifact"`
			Message   string `json:"message"`
			Timestamp string `json:"timestamp"`
		}
		json.Unmarshal(data, &handoff)

		var content strings.Builder
		fmt.Fprintf(&content, "Handoff: %s → %s\n", handoff.From, handoff.To)
		if handoff.Message != "" {
			fmt.Fprintf(&content, "Message: %s\n", handoff.Message)
		}
		if handoff.Artifact != "" {
			fmt.Fprintf(&content, "Artifact: %s\n", handoff.Artifact)
		}

		tags := strings.Join([]string{"coordination", "handoff", handoff.From, handoff.To}, " ")
		title := fmt.Sprintf("Handoff: %s → %s", handoff.From, handoff.To)

		err = indexRegistryEntry(c, regDocEntry{
			ID:      docID,
			Path:    fmt.Sprintf(".cog/handoffs/%s", entry.Name()),
			DocType: "coordination_handoff",
			Title:   title,
			Content: content.String(),
			Tags:    tags,
			Created: handoff.Timestamp,
			Updated: handoff.Timestamp,
		})
		if err != nil {
			return count, err
		}

		// Edges: handoff from/to agents
		if handoff.From != "" {
			indexRegistryEdge(c, registryEdge{
				SourceID:  docID,
				TargetURI: fmt.Sprintf("agent_crd:%s", handoff.From),
				Relation:  "from_agent",
			})
		}
		if handoff.To != "" {
			indexRegistryEdge(c, registryEdge{
				SourceID:  docID,
				TargetURI: fmt.Sprintf("agent_crd:%s", handoff.To),
				Relation:  "to_agent",
			})
		}
		count++
	}

	return count, nil
}

// indexCoordinationCheckpoints indexes synchronization checkpoints.
func indexCoordinationCheckpoints(c *constellation.Constellation, root string) (int, error) {
	cpDir := filepath.Join(root, ".cog", "signals", "checkpoint")
	entries, err := os.ReadDir(cpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read checkpoints: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		docID := fmt.Sprintf("checkpoint:%s", name)

		// List agents that reached this checkpoint
		agentDir := filepath.Join(cpDir, name)
		agents, err := os.ReadDir(agentDir)
		if err != nil {
			continue
		}

		var content strings.Builder
		var agentNames []string
		fmt.Fprintf(&content, "Checkpoint: %s\n", name)
		fmt.Fprintf(&content, "Agents reached: %d\n", len(agents))
		for _, a := range agents {
			agentNames = append(agentNames, a.Name())
			fmt.Fprintf(&content, "  - %s\n", a.Name())
		}

		tags := append([]string{"coordination", "checkpoint", name}, agentNames...)
		title := fmt.Sprintf("Checkpoint: %s (%d agents)", name, len(agents))

		info, _ := entry.Info()
		mtime := time.Now().Format(time.RFC3339)
		if info != nil {
			mtime = info.ModTime().Format(time.RFC3339)
		}

		err = indexRegistryEntry(c, regDocEntry{
			ID:      docID,
			Path:    fmt.Sprintf(".cog/signals/checkpoint/%s", name),
			DocType: "coordination_checkpoint",
			Title:   title,
			Content: content.String(),
			Tags:    strings.Join(tags, " "),
			Created: mtime,
			Updated: mtime,
		})
		if err != nil {
			return count, err
		}
		count++
	}

	return count, nil
}

// ---------------------------------------------------------------------------
// Coherence indexer
// ---------------------------------------------------------------------------

// indexCoherenceState indexes the current coherence state (not full history).
func indexCoherenceState(c *constellation.Constellation, root string) (int, error) {
	coherencePath := filepath.Join(root, ".cog", "run", "coherence", "coherence.json")
	data, err := os.ReadFile(coherencePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read coherence: %w", err)
	}

	// Parse only the current block to avoid indexing the full 1.7MB history
	var state struct {
		Current struct {
			Coherent      bool     `json:"coherent"`
			CanonicalHash *string  `json:"canonical_hash"`
			CurrentHash   string   `json:"current_hash"`
			Timestamp     string   `json:"timestamp"`
			Drift         []string `json:"drift"`
		} `json:"current"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return 0, fmt.Errorf("parse coherence: %w", err)
	}

	var content strings.Builder
	fmt.Fprintf(&content, "Coherence State\n")
	if state.Current.Coherent {
		fmt.Fprintf(&content, "Status: COHERENT\n")
	} else {
		fmt.Fprintf(&content, "Status: DRIFTED\n")
	}
	fmt.Fprintf(&content, "Current Hash: %s\n", state.Current.CurrentHash)
	if state.Current.CanonicalHash != nil {
		fmt.Fprintf(&content, "Canonical Hash: %s\n", *state.Current.CanonicalHash)
	}
	if len(state.Current.Drift) > 0 {
		fmt.Fprintf(&content, "Drifted files: %d\n", len(state.Current.Drift))
		// Show first 20 drifted files
		for i, d := range state.Current.Drift {
			if i >= 20 {
				fmt.Fprintf(&content, "  ... and %d more\n", len(state.Current.Drift)-20)
				break
			}
			fmt.Fprintf(&content, "  - %s\n", d)
		}
	}

	status := "coherent"
	if !state.Current.Coherent {
		status = "drifted"
	}
	tags := strings.Join([]string{"coherence", status}, " ")
	title := fmt.Sprintf("Coherence: %s (hash: %s)", status, state.Current.CurrentHash[:8])

	err = indexRegistryEntry(c, regDocEntry{
		ID:      "coherence:current",
		Path:    ".cog/run/coherence/coherence.json",
		DocType: "coherence_state",
		Title:   title,
		Content: content.String(),
		Tags:    tags,
		Created: state.Current.Timestamp,
		Updated: state.Current.Timestamp,
	})
	if err != nil {
		return 0, err
	}

	return 1, nil
}

// ---------------------------------------------------------------------------
// Signal field indexer
// ---------------------------------------------------------------------------

// indexSignalField indexes health, presence, and field state signals.
func indexSignalField(c *constellation.Constellation, root string) (int, error) {
	signalDir := filepath.Join(root, ".cog", "run", "signals")
	entries, err := os.ReadDir(signalDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read signals: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		// Skip lock files
		if strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(signalDir, entry.Name()))
		if err != nil {
			continue
		}

		// Parse signal to get type and node
		var signal struct {
			SignalType string                 `json:"signal_type"`
			NodeID     string                 `json:"node_id"`
			Created    string                 `json:"created"`
			Expires    string                 `json:"expires"`
			TTL        int                    `json:"ttl"`
			Data       map[string]interface{} `json:"data"`
			Version    int                    `json:"version"`
		}
		if err := json.Unmarshal(data, &signal); err != nil {
			continue
		}

		// Build deterministic ID from signal type + node
		nodeShort := signal.NodeID
		if len(nodeShort) > 12 {
			nodeShort = nodeShort[len(nodeShort)-12:]
		}
		signalType := signal.SignalType
		if signalType == "" {
			signalType = strings.TrimSuffix(entry.Name(), ".json")
		}
		docID := fmt.Sprintf("signal:%s:%s", signalType, nodeShort)

		var content strings.Builder
		fmt.Fprintf(&content, "Signal: %s\n", signalType)
		fmt.Fprintf(&content, "Node: %s\n", signal.NodeID)
		fmt.Fprintf(&content, "TTL: %ds\n", signal.TTL)
		if signal.Data != nil {
			for k, v := range signal.Data {
				fmt.Fprintf(&content, "%s: %v\n", k, v)
			}
		}

		tags := strings.Join([]string{"signal", signalType}, " ")
		title := fmt.Sprintf("Signal: %s (%s)", signalType, nodeShort)

		err = indexRegistryEntry(c, regDocEntry{
			ID:      docID,
			Path:    fmt.Sprintf(".cog/run/signals/%s", entry.Name()),
			DocType: fmt.Sprintf("signal_%s", signalType),
			Title:   title,
			Content: content.String(),
			Tags:    tags,
			Created: signal.Created,
			Updated: signal.Created,
		})
		if err != nil {
			continue
		}
		count++
	}

	return count, nil
}

// ---------------------------------------------------------------------------
// Master indexer — called from cmd_constellation.go
// ---------------------------------------------------------------------------

// indexAllRegistries indexes all operational state into constellation.
// Returns total indexed count and first error encountered.
func indexAllRegistries(c *constellation.Constellation, root string) (int, error) {
	total := 0

	type indexer struct {
		name string
		fn   func(*constellation.Constellation, string) (int, error)
	}

	indexers := []indexer{
		{"fleet entries", indexFleetRegistry},
		{"research runs", indexResearchRuns},
		{"agent CRDs", indexAgentCRDs},
		{"service CRDs", indexServiceCRDs},
		{"claims", indexCoordinationClaims},
		{"handoffs", indexCoordinationHandoffs},
		{"checkpoints", indexCoordinationCheckpoints},
		{"coherence", indexCoherenceState},
		{"signals", indexSignalField},
	}

	for _, idx := range indexers {
		n, err := idx.fn(c, root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %s indexing failed: %v\n", idx.name, err)
			continue
		}
		if n > 0 {
			fmt.Printf("  Indexed %d %s\n", n, idx.name)
		}
		total += n
	}

	return total, nil
}
