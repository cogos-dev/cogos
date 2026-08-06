package cogfield

// BusDetail is the response for GET /api/cogfield/buses/{id}.
type BusDetail struct {
	BusID        string   `json:"bus_id"`
	State        string   `json:"state"`
	Participants []string `json:"participants"`
	Created      string   `json:"created"`
	Modified     string   `json:"modified"`
	EventCount   int      `json:"event_count"`
	Events       []Block  `json:"events"`
}

// BusRegistryEntry matches the JSON format in .cog/.state/buses/registry.json.
type BusRegistryEntry struct {
	BusID        string   `json:"bus_id"`
	State        string   `json:"state"`
	Participants []string `json:"participants"`
	Transport    string   `json:"transport"`
	Endpoint     string   `json:"endpoint"`
	CreatedAt    string   `json:"created_at"`
	LastEventSeq int      `json:"last_event_seq"`
	LastEventAt  string   `json:"last_event_at"`
	EventCount   int      `json:"event_count"`
	// Generation counts size-based rotations of this bus's events.jsonl.
	// It fences registry seq writes against reordering across a rotation
	// boundary (see bus_session.go's updateRegistrySeqIfNewer / resetRegistrySeq):
	// a write is only applied when the caller's generation matches (advance)
	// or strictly exceeds (reset) the persisted value. Absent from entries
	// written before this field existed, which unmarshal it to the zero
	// value — the same value a bus that has never rotated carries, so no
	// migration is needed: an old entry and a never-rotated new entry are
	// indistinguishable, both correctly at generation 0.
	Generation int64 `json:"generation"`
}
