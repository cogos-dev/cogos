// mcp_sessions_identity_test.go — focused tests for G0(b): the session→identity
// binding path activated by the optional "subject" field on cog_register_session.
//
// Test matrix:
//  1. TestHarnessBinding_WithSubject           — register WITH subject ⇒ binding
//                                               created; ResolveHarnessBinding returns it.
//  2. TestHarnessBinding_WithoutSubject        — register WITHOUT subject ⇒ no binding;
//                                               ResolveHarnessBinding returns (nil, false).
//  3. TestHarnessBinding_ExistingCallerUnchanged — existing caller (no subject) still
//                                               succeeds; backward-compat contract.
//  4. TestHarnessBinding_BindingTypeDefault    — subject without binding_type defaults
//                                               to "agent".
//  5. TestHarnessBinding_BackendNil            — nil harnessBackend: register with
//                                               subject still succeeds, just no binding.
package engine

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	subidentity "github.com/myrgic/cogos/pkg/substrate/identity"
)

// ─── fake HarnessAttacher ────────────────────────────────────────────────────

// fakeHarnessAttacher is an in-process HarnessAttacher for tests.
type fakeHarnessAttacher struct {
	mu       sync.Mutex
	attached map[string]*subidentity.HarnessBindingCRD
}

func newFakeHarnessAttacher() *fakeHarnessAttacher {
	return &fakeHarnessAttacher{attached: make(map[string]*subidentity.HarnessBindingCRD)}
}

func (f *fakeHarnessAttacher) AttachHarness(binding *subidentity.HarnessBindingCRD) {
	key := binding.Spec.SessionID + "/" + binding.Spec.Type
	f.mu.Lock()
	f.attached[key] = binding
	f.mu.Unlock()
}

func (f *fakeHarnessAttacher) ResolveHarnessBinding(sessionID, bindingType string) (*subidentity.HarnessBindingCRD, bool) {
	key := sessionID + "/" + bindingType
	f.mu.Lock()
	b, ok := f.attached[key]
	f.mu.Unlock()
	if !ok {
		return nil, false
	}
	cp := *b
	return &cp, true
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// newMCPServerWithHarness builds a minimal MCPServer wired with a fake
// HarnessAttacher and a real in-memory sessions backend.
func newMCPServerWithHarness(t *testing.T) (*MCPServer, *fakeHarnessAttacher) {
	t.Helper()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: root + "/.cog"}
	nucleus := &Nucleus{Name: "test-harness"}
	proc := NewProcess(cfg, nucleus)
	m := NewMCPServer(cfg, nucleus, proc)

	bus := NewBusSessionManager(root)
	sessions := NewSessionRegistry()
	handoffs := NewHandoffRegistry()
	m.SetSessionsBackend(bus, sessions, handoffs)

	fake := newFakeHarnessAttacher()
	m.SetHarnessBackend(fake)
	return m, fake
}

// callRegisterSessionMCP invokes cog_register_session via the MCPServer's
// in-process CallTool path and returns the decoded JSON result map.
func callRegisterSessionMCP(t *testing.T, m *MCPServer, args map[string]any) map[string]any {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	text, _, callErr := m.CallTool(context.Background(), "cog_register_session", argsJSON)
	if callErr != nil {
		t.Fatalf("CallTool error: %v", callErr)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("unmarshal result %q: %v", text, err)
	}
	return result
}

// ─── 1. TestHarnessBinding_WithSubject ───────────────────────────────────────

func TestHarnessBinding_WithSubject(t *testing.T) {
	t.Parallel()
	m, fake := newMCPServerWithHarness(t)

	result := callRegisterSessionMCP(t, m, map[string]any{
		"session_id": "test-agent-001",
		"workspace":  "/tmp/ws",
		"role":       "agent",
		"subject":    "chaz",
	})
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got %v", result)
	}
	if created, _ := result["binding_created"].(bool); !created {
		t.Errorf("expected binding_created=true in response, got %v", result)
	}
	if subj, _ := result["binding_subject"].(string); subj != "chaz" {
		t.Errorf("expected binding_subject=chaz, got %q", subj)
	}

	// Verify the binding is retrievable via the backend.
	binding, ok := fake.ResolveHarnessBinding("test-agent-001", "agent")
	if !ok {
		t.Fatal("expected ResolveHarnessBinding to find the binding")
	}
	if binding.Spec.Subject != "chaz" {
		t.Errorf("expected subject=chaz, got %q", binding.Spec.Subject)
	}
	if binding.Spec.SessionID != "test-agent-001" {
		t.Errorf("expected session_id=test-agent-001, got %q", binding.Spec.SessionID)
	}
	if binding.Spec.Type != "agent" {
		t.Errorf("expected type=agent (default), got %q", binding.Spec.Type)
	}
}

// ─── 2. TestHarnessBinding_WithoutSubject ────────────────────────────────────

func TestHarnessBinding_WithoutSubject(t *testing.T) {
	t.Parallel()
	m, fake := newMCPServerWithHarness(t)

	result := callRegisterSessionMCP(t, m, map[string]any{
		"session_id": "test-agent-002",
		"workspace":  "/tmp/ws",
		"role":       "agent",
		// no "subject"
	})
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got %v", result)
	}
	// No binding should be created.
	if bc, _ := result["binding_created"].(bool); bc {
		t.Error("expected binding_created to be absent/false when subject not supplied")
	}

	// Backend should have nothing for this session.
	binding, ok := fake.ResolveHarnessBinding("test-agent-002", "agent")
	if ok || binding != nil {
		t.Errorf("expected no binding for session without subject, got %v", binding)
	}
}

// ─── 3. TestHarnessBinding_ExistingCallerUnchanged ───────────────────────────

func TestHarnessBinding_ExistingCallerUnchanged(t *testing.T) {
	t.Parallel()
	m, _ := newMCPServerWithHarness(t)

	// Existing caller that does NOT pass subject — must succeed exactly as before.
	result := callRegisterSessionMCP(t, m, map[string]any{
		"session_id": "old-caller-xyz",
		"workspace":  "/tmp/ws",
		"role":       "coordinator",
		"model":      "claude-opus-4",
		"hostname":   "darkstar",
	})
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("backward-compat failure: existing caller got ok=false, result=%v", result)
	}
	if sid, _ := result["session_id"].(string); sid != "old-caller-xyz" {
		t.Errorf("expected session_id=old-caller-xyz, got %q", sid)
	}
	// No binding_created field in response for callers that don't supply subject.
	if _, present := result["binding_created"]; present {
		t.Error("binding_created should not appear in response when subject is absent")
	}
}

// ─── 4. TestHarnessBinding_BindingTypeDefault ────────────────────────────────

func TestHarnessBinding_BindingTypeDefault(t *testing.T) {
	t.Parallel()
	m, fake := newMCPServerWithHarness(t)

	// Provide subject but no binding_type — should default to "agent".
	callRegisterSessionMCP(t, m, map[string]any{
		"session_id": "test-agent-003",
		"workspace":  "/tmp/ws",
		"role":       "agent",
		"subject":    "cog",
		// no binding_type
	})

	binding, ok := fake.ResolveHarnessBinding("test-agent-003", "agent")
	if !ok {
		t.Fatal("expected binding under type=agent (default)")
	}
	if binding.Spec.Type != "agent" {
		t.Errorf("expected type=agent default, got %q", binding.Spec.Type)
	}
	// Must not appear under "user".
	_, ok = fake.ResolveHarnessBinding("test-agent-003", "user")
	if ok {
		t.Error("should not find binding under type=user when binding_type defaulted to agent")
	}
}

// ─── 5. TestHarnessBinding_BackendNil ────────────────────────────────────────

func TestHarnessBinding_BackendNil(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: root + "/.cog"}
	nucleus := &Nucleus{Name: "test-noharness"}
	proc := NewProcess(cfg, nucleus)
	m := NewMCPServer(cfg, nucleus, proc)

	bus := NewBusSessionManager(root)
	sessions := NewSessionRegistry()
	handoffs := NewHandoffRegistry()
	m.SetSessionsBackend(bus, sessions, handoffs)
	// Do NOT call m.SetHarnessBackend — harnessBackend is nil.

	// Register with a subject: must succeed without panic.
	result := callRegisterSessionMCP(t, m, map[string]any{
		"session_id": "test-nobackend-001",
		"workspace":  "/tmp/ws",
		"role":       "agent",
		"subject":    "slowbro",
	})
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("expected ok=true even with nil backend, got %v", result)
	}
	// No binding_created since backend is nil.
	if bc, _ := result["binding_created"].(bool); bc {
		t.Error("expected binding_created=false/absent when harnessBackend is nil")
	}
}
