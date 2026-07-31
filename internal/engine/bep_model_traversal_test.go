// bep_model_traversal_test.go — regression test for myrgic/cogos#489 round 4:
// AgentSyncModel.HandleRequest lacked the path-separator/traversal guard its
// write-side siblings (BEPProvider.ReceiveAgentCRD / RemoveAgentCRD, in
// bep_receiver.go) already had, making it a read-only exception to an
// otherwise-uniform rule. A peer that can send a BEP Request message could
// ask for any ".agent.yaml"-suffixed file reachable by lexical traversal
// from watchDir, not just files actually inside it.
package engine

import (
	"os"
	"path/filepath"
	"testing"

	bep "github.com/myrgic/cogos/pkg/substrate/bep"
)

func TestHandleRequestRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	watchDir := filepath.Join(root, "definitions")
	if err := os.MkdirAll(watchDir, 0755); err != nil {
		t.Fatalf("MkdirAll(watchDir): %v", err)
	}

	// Plant a secret file OUTSIDE watchDir, one level up, matching the
	// ".agent.yaml" suffix bep.IsAgentCRDFile checks — the only guard the
	// pre-fix code applied.
	secretPath := filepath.Join(root, "secret.agent.yaml")
	const marker = "outside-the-watchdir-marker"
	if err := os.WriteFile(secretPath, []byte(marker), 0644); err != nil {
		t.Fatalf("WriteFile(secret): %v", err)
	}

	m := NewAgentSyncModel(nil, watchDir, filepath.Join(root, "state"), 1)

	resp := m.HandleRequest(&bep.Request{ID: 1, Name: "../secret.agent.yaml"})

	if resp.Code == bep.ErrorCodeNoError {
		t.Fatalf("HandleRequest(%q) succeeded (code=%v); want rejection", "../secret.agent.yaml", resp.Code)
	}
	if string(resp.Data) == marker {
		t.Fatalf("HandleRequest leaked the planted secret via path traversal: %q", resp.Data)
	}
}

// TestHandleRequestRejectsBackslashPathTraversal exercises the guard's
// ContainsAny(req.Name, `/\`) branch with a backslash-only name (no '/' at
// all — the shape that matters on a Windows peer, where filepath.Join
// treats '\' as a real separator). On this platform (macOS/Linux CI),
// filepath.Base and filepath.Join do NOT treat '\' as a separator, so this
// specific input is inert regardless of the guard: os.ReadFile fails with
// NoSuchFile either way, because no file is ever created with a literal
// backslash in its name. This test therefore cannot demonstrate the escape
// itself (that requires a Windows filesystem) — it only proves the guard
// still rejects the input rather than, say, panicking, and documents the
// input shape the guard is written to cover. The %-encoded-'..' and
// forward-slash cases above are the ones that actually regress-test on this
// CI platform.
func TestHandleRequestRejectsBackslashPathTraversal(t *testing.T) {
	root := t.TempDir()
	watchDir := filepath.Join(root, "definitions")
	if err := os.MkdirAll(watchDir, 0755); err != nil {
		t.Fatalf("MkdirAll(watchDir): %v", err)
	}

	secretPath := filepath.Join(root, "secret.agent.yaml")
	const marker = "outside-the-watchdir-marker"
	if err := os.WriteFile(secretPath, []byte(marker), 0644); err != nil {
		t.Fatalf("WriteFile(secret): %v", err)
	}

	m := NewAgentSyncModel(nil, watchDir, filepath.Join(root, "state"), 1)

	resp := m.HandleRequest(&bep.Request{ID: 1, Name: `..\secret.agent.yaml`})

	if resp.Code == bep.ErrorCodeNoError {
		t.Fatalf("HandleRequest(%q) succeeded (code=%v); want rejection", `..\secret.agent.yaml`, resp.Code)
	}
}

// TestHandleRequestStillServesLegitimateFiles guards against the traversal
// guard being overly strict: an ordinary, non-hostile filename inside
// watchDir must still be served.
func TestHandleRequestStillServesLegitimateFiles(t *testing.T) {
	root := t.TempDir()
	watchDir := filepath.Join(root, "definitions")
	if err := os.MkdirAll(watchDir, 0755); err != nil {
		t.Fatalf("MkdirAll(watchDir): %v", err)
	}

	const content = "apiVersion: cog.os/v1alpha1\nkind: Agent\n"
	if err := os.WriteFile(filepath.Join(watchDir, "cog.agent.yaml"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := NewAgentSyncModel(nil, watchDir, filepath.Join(root, "state"), 1)

	resp := m.HandleRequest(&bep.Request{ID: 1, Name: "cog.agent.yaml"})

	if resp.Code != bep.ErrorCodeNoError {
		t.Fatalf("HandleRequest(%q) code=%v, want ErrorCodeNoError", "cog.agent.yaml", resp.Code)
	}
	if string(resp.Data) != content {
		t.Fatalf("HandleRequest returned %q, want %q", resp.Data, content)
	}
}
