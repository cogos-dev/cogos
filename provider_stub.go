// provider_stub.go — StubProvider for testing
//
// In-memory provider with configurable responses, error injection, and
// latency simulation. Used in unit tests and for offline development.
package main

import (
	"context"
	"time"
)

// StubProvider is an in-memory Provider for testing.
type StubProvider struct {
	name         string
	response     string
	err          error
	latency      time.Duration
	available    bool
	capabilities ProviderCapabilities
	chunks       []string // if set, Stream sends these chunks instead of response
}

// NewStubProvider creates a StubProvider that returns the given response.
func NewStubProvider(name, response string) *StubProvider {
	return &StubProvider{
		name:      name,
		response:  response,
		available: true,
		capabilities: ProviderCapabilities{
			Capabilities:    []Capability{CapStreaming, CapToolUse, CapVision, CapJSON},
			MaxContextTokens: 128000,
			MaxOutputTokens:  4096,
			IsLocal:          true,
		},
	}
}

func (s *StubProvider) Name() string                              { return s.name }
func (s *StubProvider) Available(_ context.Context) bool          { return s.available }
func (s *StubProvider) Capabilities() ProviderCapabilities        { return s.capabilities }

func (s *StubProvider) Ping(_ context.Context) (time.Duration, error) {
	if s.err != nil {
		return 0, s.err
	}
	return s.latency, nil
}

func (s *StubProvider) Complete(_ context.Context, _ *CompletionRequest) (*CompletionResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.latency > 0 {
		time.Sleep(s.latency)
	}
	return &CompletionResponse{
		Content:    s.response,
		StopReason: "end_turn",
		ProviderMeta: ProviderMeta{
			Provider: s.name,
			Model:    "stub",
		},
	}, nil
}

func (s *StubProvider) Stream(_ context.Context, _ *CompletionRequest) (<-chan StreamChunk, error) {
	if s.err != nil {
		return nil, s.err
	}
	chunks := s.chunks
	if len(chunks) == 0 {
		chunks = []string{s.response}
	}
	ch := make(chan StreamChunk, len(chunks)+1)
	for _, c := range chunks {
		ch <- StreamChunk{Delta: c}
	}
	ch <- StreamChunk{
		Done: true,
		ProviderMeta: &ProviderMeta{Provider: s.name, Model: "stub"},
	}
	close(ch)
	return ch, nil
}
