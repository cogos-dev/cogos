package cogfield

// DocRef represents a reference to/from another document.
type DocRef struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Relation string `json:"relation"`
	Type     string `json:"type"`
	Sector   string `json:"sector"`
}

// DocumentDetail is the full response for a document content request.
type DocumentDetail struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Type      string   `json:"type"`
	Sector    string   `json:"sector"`
	Path      string   `json:"path"`
	Created   string   `json:"created"`
	Modified  string   `json:"modified"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags"`
	Refs      []DocRef `json:"refs"`
	Backlinks []DocRef `json:"backlinks"`
}
