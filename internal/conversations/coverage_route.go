// coverage_route.go — HTTP handler for GET /v1/observatory/coverage.
//
// Returns per-source coverage metrics for the ontology enforcement layer.
// Smallest honest surface: one GET endpoint, JSON response.
//
// Response shape:
//
//	{
//	  "ontology_ref": "cogos.conversations@1.0.0",
//	  "sources": {
//	    "hermes-node-a": {
//	      "mapped": 12453,
//	      "degenerate": 16547,
//	      "quarantined": 0,
//	      "unmapped_component_counts": {},
//	      "ontology_ref": "cogos.conversations@1.0.0",
//	      "mapping_ref": "hermes-statedb.v1@1.0.0"
//	    }
//	  }
//	}
//
// Note: v0.1 records are all session.turn. Sources with no loaded mapping
// will have quarantined > 0 and empty mapped.
package conversations

import (
	"encoding/json"
	"net/http"
)

// CoverageHTTPHandler returns an http.Handler that serves
// GET /v1/observatory/coverage.
// p may be nil — returns {"error": "provider not wired"} in that case.
func CoverageHTTPHandler(p *Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if p == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "conversations provider not wired",
			})
			return
		}

		cov := p.Coverage()
		ont := p.Ontology()

		ontRef := ""
		if ont != nil {
			ontRef = ont.OntologyRef
		}

		resp := map[string]any{
			"ontology_ref": ontRef,
			"sources":      cov,
			"note":         "v0.1 records are all session.turn; v0.2 introduces additional component classes",
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
