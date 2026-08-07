package bep

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPeer_NodeIdentityHash_Roundtrip covers the new optional field end to
// end: it parses when present (YAML and JSON), and is valid when absent —
// existing cluster.yaml peer entries that predate this field must keep
// parsing cleanly.
//
// There is no validation function for Peer today (LoadConfig only defaults
// Enabled/ListenPort; see internal/engine/bep_provider.go), so there is no
// "malformed value is rejected" case to cover here. Per CONTRIBUTING.md
// ("Don't add error-handling, fallbacks, or validation for scenarios that
// can't happen"), this change does not invent one.
func TestPeer_NodeIdentityHash_Roundtrip(t *testing.T) {
	const hash = "sha256:439592686bc8d8e792263de0c4ed548c05fd907e84c3f520e57b58638a36084c"

	t.Run("yaml present", func(t *testing.T) {
		src := `
deviceId: VHS7RZQ-LS4STOQ-J2GITFD-HACSCNX-YHBL5HV-OKHFIOM-IZGKI67-TFLVIAA
address: 192.168.10.191:22033
name: eclipse
trusted: true
nodeIdentityHash: ` + hash + `
`
		var p Peer
		if err := yaml.Unmarshal([]byte(src), &p); err != nil {
			t.Fatalf("yaml.Unmarshal: %v", err)
		}
		if p.NodeIdentityHash != hash {
			t.Errorf("NodeIdentityHash = %q, want %q", p.NodeIdentityHash, hash)
		}
	})

	t.Run("yaml absent is valid", func(t *testing.T) {
		src := `
deviceId: VHS7RZQ-LS4STOQ-J2GITFD-HACSCNX-YHBL5HV-OKHFIOM-IZGKI67-TFLVIAA
address: 192.168.10.191:22033
name: eclipse
trusted: true
`
		var p Peer
		if err := yaml.Unmarshal([]byte(src), &p); err != nil {
			t.Fatalf("yaml.Unmarshal: %v", err)
		}
		if p.NodeIdentityHash != "" {
			t.Errorf("NodeIdentityHash = %q, want empty", p.NodeIdentityHash)
		}

		// omitempty: a peer without the field must not gain the key on
		// re-marshal, so round-tripping a pre-existing cluster.yaml doesn't
		// inject a spurious empty key.
		out, err := yaml.Marshal(&p)
		if err != nil {
			t.Fatalf("yaml.Marshal: %v", err)
		}
		if got := string(out); strings.Contains(got, "nodeIdentityHash") {
			t.Errorf("marshaled output unexpectedly contains nodeIdentityHash: %s", got)
		}
	})

	t.Run("json present", func(t *testing.T) {
		src := `{"deviceId":"dev","address":"a:1","name":"n","trusted":true,"nodeIdentityHash":"` + hash + `"}`
		var p Peer
		if err := json.Unmarshal([]byte(src), &p); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if p.NodeIdentityHash != hash {
			t.Errorf("NodeIdentityHash = %q, want %q", p.NodeIdentityHash, hash)
		}
	})

	t.Run("json absent omitted on marshal", func(t *testing.T) {
		p := Peer{DeviceID: "dev", Address: "a:1", Name: "n", Trusted: true}
		out, err := json.Marshal(&p)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if strings.Contains(string(out), "nodeIdentityHash") {
			t.Errorf("marshaled output unexpectedly contains nodeIdentityHash: %s", out)
		}
	})
}
