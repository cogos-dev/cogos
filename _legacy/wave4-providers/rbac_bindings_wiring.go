// rbac_bindings_wiring.go — registers RBACProvider with the reconcile engine.
//
// Same pattern as identity_wiring.go: an init() block constructs the provider
// with a stub bus-emit adapter and calls RegisterProvider so the reconcile
// harness picks it up automatically at startup.
//
// Bus-emit adapter: logs dropped events at debug level; a full wiring to
// AppendEvent (or the modality bus) is Wave 6c work, matching the identity
// provider's deferral.

package main

import "log"

func init() {
	emit := BusEmit(func(eventType string, data map[string]any) error {
		log.Printf("[rbac-bindings] bus event (stub-emit) type=%s\n", eventType)
		return nil
	})
	provider := NewRBACProvider(emit)
	RegisterProvider("rbac-bindings", provider)
}
