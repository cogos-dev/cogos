// projection_compiler_register_test.go — registration smoke test.
//
// Confirms RegisterProjectionCompiler installs a provider under the
// CompilerType key. Uses the external _test package so it touches only
// the public engine surface.

package engine_test

import (
	"testing"

	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

func TestProjectionCompiler_RegistrationInstallsProvider(t *testing.T) {
	// UpsertProvider semantics: idempotent install without clearing the
	// rest of the registry. Two calls in a row should still leave exactly
	// one ProjectionCompiler addressable by Type.
	engine.RegisterProjectionCompiler()
	engine.RegisterProjectionCompiler()

	if !reconcile.HasProvider(engine.CompilerType) {
		t.Fatalf("expected provider %q in registry", engine.CompilerType)
	}
	provider, err := reconcile.GetProvider(engine.CompilerType)
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if provider.Type() != engine.CompilerType {
		t.Errorf("provider Type = %q, want %q", provider.Type(), engine.CompilerType)
	}
}
