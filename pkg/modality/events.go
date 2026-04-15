// events.go — Modality event type constants and data structures.
//
// The event data structs are stdlib-only. Event construction functions
// that depend on the kernel ledger (EventPayload) remain in the kernel.

package modality

// Modality event type constants for the CogOS ledger.
const (
	EventInput       = "modality.input"
	EventOutput      = "modality.output"
	EventTransform   = "modality.transform"
	EventGate        = "modality.gate"
	EventStateChange = "modality.state_change"
	EventError       = "modality.error"
)

// InputData is the Data payload for a modality.input event.
type InputData struct {
	Modality       string  `json:"modality"`
	Channel        string  `json:"channel"`
	Transcript     string  `json:"transcript"`
	GateConfidence float64 `json:"gate_confidence,omitempty"`
	SpeechRatio    float64 `json:"speech_ratio,omitempty"`
	LatencyMs      int     `json:"latency_ms,omitempty"`
	Engine         string  `json:"engine,omitempty"`
}

// OutputData is the Data payload for a modality.output event.
type OutputData struct {
	Modality    string  `json:"modality"`
	Channel     string  `json:"channel"`
	Text        string  `json:"text"`
	Engine      string  `json:"engine,omitempty"`
	Voice       string  `json:"voice,omitempty"`
	RTF         float64 `json:"rtf,omitempty"`
	DurationSec float64 `json:"duration_sec,omitempty"`
	LatencyMs   int     `json:"latency_ms,omitempty"`
}

// TransformData is the Data payload for a modality.transform event.
type TransformData struct {
	FromModality string `json:"from_modality"`
	ToModality   string `json:"to_modality"`
	Step         string `json:"step"`
	Engine       string `json:"engine,omitempty"`
	LatencyMs    int    `json:"latency_ms,omitempty"`
	InputBytes   int    `json:"input_bytes,omitempty"`
	OutputChars  int    `json:"output_chars,omitempty"`
}

// GateData is the Data payload for a modality.gate event.
type GateData struct {
	Modality    string  `json:"modality"`
	Channel     string  `json:"channel"`
	Decision    string  `json:"decision"`
	Confidence  float64 `json:"confidence"`
	SpeechRatio float64 `json:"speech_ratio,omitempty"`
	DurationMs  int     `json:"duration_ms,omitempty"`
	Gate        string  `json:"gate,omitempty"`
}

// StateChangeData is the Data payload for a modality.state_change event.
type StateChangeData struct {
	Modality  string `json:"modality"`
	Module    string `json:"module"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	PID       int    `json:"pid,omitempty"`
}

// ErrorData is the Data payload for a modality.error event.
type ErrorData struct {
	Modality    string `json:"modality"`
	Module      string `json:"module"`
	Error       string `json:"error"`
	ErrorType   string `json:"error_type,omitempty"`
	Recoverable bool   `json:"recoverable,omitempty"`
}

// RequireFields returns an error naming the first empty field, or nil.
func RequireFields(eventType string, pairs ...string) error {
	for i := 0; i < len(pairs)-1; i += 2 {
		if pairs[i+1] == "" {
			return &FieldRequiredError{EventType: eventType, Field: pairs[i]}
		}
	}
	return nil
}

// FieldRequiredError is returned when a required event field is empty.
type FieldRequiredError struct {
	EventType string
	Field     string
}

func (e *FieldRequiredError) Error() string {
	return e.EventType + ": " + e.Field + " is required"
}
