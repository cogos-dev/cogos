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
}
