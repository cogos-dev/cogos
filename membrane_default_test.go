//go:build mcpserver

package main

import "testing"

func TestDefaultMembranePolicyDecisions(t *testing.T) {
	t.Parallel()
	policy := DefaultMembranePolicy{}

	tests := []struct {
		name  string
		block *CogBlock
		want  AssimilationDecision
	}{
		{
			name:  "discard empty content",
			block: &CogBlock{Kind: BlockMessage},
			want:  Discard,
		},
		{
			name: "integrate local workspace traffic",
			block: &CogBlock{
				Kind:         BlockMessage,
				Messages:     []ProviderMessage{{Role: "user", Content: "hello"}},
				TrustContext: TrustContext{TrustScore: 1.0, Scope: "local"},
			},
			want: Integrate,
		},
		{
			name: "integrate mcp tool results",
			block: &CogBlock{
				Kind:            BlockToolResult,
				SourceChannel:   "mcp",
				SourceTransport: "mcp",
				Messages:        []ProviderMessage{{Role: "tool", Content: "done"}},
			},
			want: Integrate,
		},
		{
			name: "quarantine unknown external imports",
			block: &CogBlock{
				Kind:         BlockImport,
				Messages:     []ProviderMessage{{Role: "user", Content: "remote data"}},
				TrustContext: TrustContext{TrustScore: 0.4, Scope: "network"},
			},
			want: Quarantine,
		},
		{
			name: "defer everything else",
			block: &CogBlock{
				Kind:         BlockMessage,
				Messages:     []ProviderMessage{{Role: "user", Content: "review me"}},
				TrustContext: TrustContext{TrustScore: 0.3, Scope: "network"},
				Provenance:   BlockProvenance{OriginChannel: "external"},
			},
			want: Defer,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := policy.Evaluate(tc.block)
			if got.Decision != tc.want {
				t.Fatalf("Decision = %q; want %q", got.Decision, tc.want)
			}
		})
	}
}
