package cogfield

// Block is the canonical content atom for the CogOS bus protocol (ADR-059).
// V1 blocks use PrevHash (string); V2 blocks use Prev ([]string) for DAG-style linking.
// Both fields are written during the transition period for backward compatibility.
type Block struct {
	V        int                    `json:"v"`
	ID       string                 `json:"id,omitempty"`
	BusID    string                 `json:"bus_id,omitempty"`
	Seq      int                    `json:"seq,omitempty"`
	Ts       string                 `json:"ts"`
	From     string                 `json:"from"`
	To       string                 `json:"to,omitempty"`
	Type     string                 `json:"type"`
	Payload  map[string]interface{} `json:"payload"`
	Prev     []string               `json:"prev,omitempty"`
	PrevHash string                 `json:"prev_hash,omitempty"` // V1 compat
	Hash     string                 `json:"hash"`
	Merkle   string                 `json:"merkle,omitempty"`
	Sig      string                 `json:"sig,omitempty"`
	Size     int                    `json:"size,omitempty"`
}

// GraphBlock is the intermediate representation for CogField graph rendering.
// Adapters convert their native data into GraphBlocks for visualization.
type GraphBlock struct {
	URI      string                 // cog://bus/{busID}/{seq}
	Type     string                 // bus.message, session.turn, etc.
	From     string
	Ts       string
	Hash     string
	PrevHash string
	Payload  map[string]interface{}
	Meta     map[string]interface{}
}
