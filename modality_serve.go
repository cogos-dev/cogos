// modality_serve.go — Wiring between the modality pipeline and the serve daemon.
//
// Provides:
//   - registerHTTPModules: auto-registers HTTP modality modules from service CRDs
//   - handleSpeak:         POST /v1/speak endpoint for voice output via the pipeline

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

// registerHTTPModules scans service CRDs for services that advertise on the bus
// and expose a voice modality, then registers an HTTPModule for each one on
// the pipeline's modality bus.
func registerHTTPModules(root string, pipeline *ModalityPipeline) {
	crds, err := ListServiceCRDs(root)
	if err != nil {
		log.Printf("[modality] failed to list service CRDs: %v", err)
		return
	}

	for _, crd := range crds {
		if !crd.Spec.Bus.Advertise {
			continue
		}
		for _, mod := range crd.Spec.Modalities {
			if mod != "voice" {
				continue
			}
			port := crd.Spec.Health.Port
			if port == 0 {
				// Fall back to first declared host port
				if len(crd.Spec.Ports) > 0 {
					port = crd.Spec.Ports[0].Host
				}
			}
			if port == 0 {
				log.Printf("[modality] skipping %s: no port found for HTTP module", crd.Metadata.Name)
				continue
			}

			cfg := HTTPModuleConfig{
				BaseURL: fmt.Sprintf("http://localhost:%d", port),
			}
			module := NewHTTPModule(cfg)
			if regErr := pipeline.Bus.Register(module); regErr != nil {
				log.Printf("[modality] failed to register HTTP module for %s: %v", crd.Metadata.Name, regErr)
			} else {
				log.Printf("[modality] registered HTTP voice module: %s at port %d", crd.Metadata.Name, port)
			}
		}
	}
}

// speakRequest is the JSON body for POST /v1/speak.
type speakRequest struct {
	Text    string `json:"text"`
	Voice   string `json:"voice,omitempty"`
	Speed   string `json:"speed,omitempty"`
	Channel string `json:"channel,omitempty"`
}

// handleSpeak routes voice output through the modality pipeline.
//
//	POST /v1/speak
//	{"text":"Hello world","voice":"bm_lewis","speed":"1.25"}
//
// Returns audio bytes with the appropriate Content-Type (e.g. audio/wav).
func (s *serveServer) handleSpeak(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.pipeline == nil {
		s.writeError(w, http.StatusServiceUnavailable, "modality pipeline not initialized", "pipeline_unavailable")
		return
	}

	var req speakRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request")
		return
	}
	if req.Text == "" {
		s.writeError(w, http.StatusBadRequest, "text is required", "invalid_request")
		return
	}

	params := map[string]any{}
	if req.Voice != "" {
		params["voice"] = req.Voice
	}
	if req.Speed != "" {
		params["speed"] = req.Speed
	}

	intent := &CognitiveIntent{
		Modality: ModalityVoice,
		Content:  req.Text,
		Channel:  req.Channel,
		Params:   params,
	}

	// Try channel-based egress first (routes to all channels supporting voice).
	sessionID := "default"
	if req.Channel != "" {
		sessionID = req.Channel
	}
	outputs, err := s.pipeline.HandleEgress(sessionID, intent)
	if err == nil {
		// Return first output
		for _, output := range outputs {
			w.Header().Set("Content-Type", output.MimeType)
			w.Write(output.Data)
			return
		}
	}

	// No channels registered — fall through to direct bus encode.
	output, encErr := s.pipeline.Bus.Act(intent)
	if encErr != nil {
		s.writeError(w, http.StatusInternalServerError, "speak failed: "+encErr.Error(), "pipeline_error")
		return
	}

	w.Header().Set("Content-Type", output.MimeType)
	w.Write(output.Data)
}
