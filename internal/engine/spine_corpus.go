// spine_corpus.go — load the decision corpus (the spine's vertebrae) from disk.
//
// Where the theoretical-lineage projections read .cog/mem/semantic/lineage/nodes/,
// the decision-lineage projection reads the architecture's actual decision
// records:
//
//   - .cog/architecture/adrs/*.cog.md   — ratified / proposed ADRs
//   - .cog/architecture/rfcs/*.cog.md   — in-flight RFCs
//   - .cog/mem/semantic/insights/*.cog.md (kind: decision-insight only)
//
// Each file's YAML frontmatter carries the id/slug/title/status/created and the
// `refs:` block whose `rel`/`uri` pairs are the spine's edges. Edge targets are
// resolved to the last path segment of the cog:// URI so a reference to
// `cog://architecture/adrs/cogblock-protocol` resolves to the decision whose
// slug/id is `cogblock-protocol`.
package engine

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// decisionFrontmatter is the subset of decision-cogdoc frontmatter the spine
// computation needs. Decision corpora are heterogeneous (ADRs, RFCs, insights
// with different field sets) so every field is optional and we fall back across
// id / adr-number / slug to find a stable key.
type decisionFrontmatter struct {
	Type    string `yaml:"type"`
	Kind    string `yaml:"kind"`
	ID      string `yaml:"id"`
	ADR     any    `yaml:"adr"` // int or string in different files
	RFC     any    `yaml:"rfc"`
	Title   string `yaml:"title"`
	Status  string `yaml:"status"`
	Created string `yaml:"created"`
	Slug    string `yaml:"slug"`
	Refs    []struct {
		URI         string `yaml:"uri"`
		Rel         string `yaml:"rel"`
		Description string `yaml:"description"`
		Note        string `yaml:"note"`
	} `yaml:"refs"`
}

// DecisionCorpusDirs returns the decision-corpus source directories under root,
// in deterministic order. Missing directories are simply skipped by the loader.
func DecisionCorpusDirs(root string) []string {
	base := filepath.Join(root, ".cog")
	return []string{
		filepath.Join(base, "architecture", "adrs"),
		filepath.Join(base, "architecture", "rfcs"),
		filepath.Join(base, "mem", "semantic", "insights"),
	}
}

// LoadDecisionCorpus walks the decision-corpus directories under root and
// returns the parsed decisions, sorted by id for determinism.
//
// Insight files are included only when kind/type marks them as a decision
// (kind or type containing "decision"); the insights directory holds many
// non-decision notes that would otherwise pollute the spine.
//
// Files that fail to parse are skipped (not fatal): a malformed cogdoc should
// not take down the whole field.
func LoadDecisionCorpus(root string) ([]Decision, error) {
	var decisions []Decision
	seen := make(map[string]bool)

	for _, dir := range DecisionCorpusDirs(root) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		isInsightDir := strings.HasSuffix(dir, filepath.Join("semantic", "insights"))

		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".cog.md") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			d, ok := parseDecision(path)
			if !ok {
				continue
			}
			// In the insights dir, keep only decision-class insights.
			if isInsightDir {
				k := strings.ToLower(d.Kind)
				if !strings.Contains(k, "decision") {
					continue
				}
			}
			if seen[d.ID] {
				continue
			}
			seen[d.ID] = true
			decisions = append(decisions, d)
		}
	}

	return decisions, nil
}

// parseDecision reads one decision cogdoc and returns it. ok=false means the
// file was unparseable or had no usable identity key.
func parseDecision(path string) (Decision, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Decision{}, false
	}
	content := string(data)
	if !strings.HasPrefix(content, "---") {
		return Decision{}, false
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	scanner.Scan() // opening ---
	var fmLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "---" {
			break
		}
		fmLines = append(fmLines, line)
	}

	var fm decisionFrontmatter
	if err := yaml.Unmarshal([]byte(strings.Join(fmLines, "\n")), &fm); err != nil {
		return Decision{}, false
	}

	id := decisionKey(fm, path)
	if id == "" {
		return Decision{}, false
	}

	kind := strings.ToLower(strings.TrimSpace(fm.Type))
	if kind == "" {
		kind = strings.ToLower(strings.TrimSpace(fm.Kind))
	}

	d := Decision{
		ID:      id,
		Title:   strings.TrimSpace(fm.Title),
		Kind:    kind,
		Status:  strings.TrimSpace(fm.Status),
		Created: strings.TrimSpace(fm.Created),
		Path:    path,
	}
	for _, r := range fm.Refs {
		if r.URI == "" {
			continue
		}
		d.Edges = append(d.Edges, DecisionEdge{
			Rel:    r.Rel,
			Target: resolveEdgeTarget(r.URI),
			URI:    r.URI,
		})
	}
	return d, true
}

// decisionKey picks a stable identity key for a decision, preferring the slug
// (the human-legible cross-reference key used in cog:// URIs), then id, then
// the filename stem. This must match what edge targets resolve to so the graph
// closes over itself.
func decisionKey(fm decisionFrontmatter, path string) string {
	if s := strings.TrimSpace(fm.Slug); s != "" {
		return lastURISegment(s)
	}
	if s := strings.TrimSpace(fm.ID); s != "" {
		return s
	}
	// Filename stem: ADR-cogblock-protocol.cog.md → cogblock-protocol
	base := strings.TrimSuffix(filepath.Base(path), ".cog.md")
	base = strings.TrimPrefix(base, "ADR-")
	base = strings.TrimPrefix(base, "RFC-")
	return strings.ToLower(base)
}

// resolveEdgeTarget reduces a cog:// URI to the decision key it points at: the
// last path segment. e.g. cog://architecture/adrs/cogblock-protocol →
// cogblock-protocol.
func resolveEdgeTarget(uri string) string {
	return lastURISegment(uri)
}

// lastURISegment returns the final '/'-separated segment of a URI/slug,
// stripping any scheme and trailing slash.
func lastURISegment(uri string) string {
	s := strings.TrimSpace(uri)
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}
