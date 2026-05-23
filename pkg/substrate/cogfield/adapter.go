package cogfield

import "strings"

// BlockAdapter lets any data source produce graph nodes for CogField.
type BlockAdapter interface {
	ID() string
	NodeConfig() AdapterNodeConfig
	SummaryNodes(root string) ([]Node, []Edge)
	ExpandNode(root, nodeID string) ([]Node, []Edge, error)
}

// AdapterNodeConfig describes how an adapter's block types map to graph rendering.
type AdapterNodeConfig struct {
	BlockTypes    map[string]BlockTypeConfig `json:"block_types"`
	DefaultSector string                     `json:"default_sector"`
	ChainThread   string                     `json:"chain_thread"`
}

// BlockTypeConfig describes the visual config for a block type.
type BlockTypeConfig struct {
	EntityType string `json:"entity_type"`
	Shape      string `json:"shape"`
	Color      string `json:"color,omitempty"`
	Label      string `json:"label"`
}

// ExpandNodeResponse is the response for GET /api/cogfield/expand/{nodeId}.
type ExpandNodeResponse struct {
	ParentID string            `json:"parent_id"`
	Nodes    []Node            `json:"nodes"`
	Edges    []Edge            `json:"edges"`
	Config   AdapterNodeConfig `json:"config"`
}

// GraphBlockToNode converts a GraphBlock into a Node for graph rendering.
func GraphBlockToNode(block GraphBlock) Node {
	sector := "sessions"
	if strings.HasPrefix(block.Type, "bus.") {
		sector = "buses"
	}

	label := ""
	if content, ok := block.Payload["content"]; ok {
		if s, ok := content.(string); ok {
			label = s
			if len(label) > 60 {
				label = label[:60] + "..."
			}
		}
	}
	if label == "" {
		label = block.Type
	}

	meta := map[string]interface{}{
		"block_type": block.Type,
	}
	if block.Hash != "" {
		meta["hash"] = block.Hash
	}
	if block.PrevHash != "" {
		meta["prev_hash"] = block.PrevHash
	}
	if block.From != "" {
		meta["from"] = block.From
	}
	if content, ok := block.Payload["content"]; ok {
		meta["full_content"] = content
	}
	for k, v := range block.Meta {
		meta[k] = v
	}

	return Node{
		ID:         block.URI,
		Label:      label,
		EntityType: block.Type,
		Sector:     sector,
		Tags:       []string{},
		Created:    block.Ts,
		Modified:   block.Ts,
		Strength:   3.0,
		Meta:       meta,
	}
}
