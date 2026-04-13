// modality_http.go — HTTP-backed ModalityModule for remote services (e.g., Mod³).
//
// Instead of subprocess IPC (modality_wire.go), this wraps an HTTP service
// as a ModalityModule. The bus routes through it identically — same
// Gate/Decoder/Encoder interface, different transport.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// HTTPModuleConfig configures an HTTP-backed modality module.
type HTTPModuleConfig struct {
	BaseURL        string        // e.g. "http://localhost:7860"
	HealthInterval time.Duration // polling interval (default 10s)
	HealthTimeout  time.Duration // per-request timeout for health (default 5s)
	RequestTimeout time.Duration // timeout for encode/gate calls (default 30s)
}

func (c *HTTPModuleConfig) defaults() {
	if c.HealthInterval <= 0 {
		c.HealthInterval = 10 * time.Second
	}
	if c.HealthTimeout <= 0 {
		c.HealthTimeout = 5 * time.Second
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 30 * time.Second
	}
}

// HTTPModule wraps a remote HTTP service as a ModalityModule.
type HTTPModule struct {
	cfg    HTTPModuleConfig
	client *http.Client

	mu        sync.RWMutex
	state     ModuleState
	startedAt time.Time

	cancel context.CancelFunc // stops health loop
}

// NewHTTPModule creates an HTTP-backed voice modality module.
func NewHTTPModule(cfg HTTPModuleConfig) *HTTPModule {
	cfg.defaults()
	return &HTTPModule{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.RequestTimeout,
		},
		state: ModuleState{
			Status:   ModuleStatusStopped,
			Modality: ModalityVoice,
		},
	}
}

func (m *HTTPModule) Type() ModalityType { return ModalityVoice }

func (m *HTTPModule) Gate() Gate {
	return &httpGate{module: m}
}

func (m *HTTPModule) Decoder() Decoder { return nil }

func (m *HTTPModule) Encoder() Encoder {
	return &httpEncoder{module: m}
}

func (m *HTTPModule) State() *ModuleState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.state // copy
	if !m.startedAt.IsZero() && s.Status == ModuleStatusHealthy {
		s.Uptime = time.Since(m.startedAt)
	}
	return &s
}

func (m *HTTPModule) Health() ModuleStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state.Status
}

// Start polls GET /health until the remote service is reachable or ctx expires.
func (m *HTTPModule) Start(ctx context.Context) error {
	m.mu.Lock()
	m.state.Status = ModuleStatusStarting
	m.state.LastError = ""
	m.mu.Unlock()

	// Poll until healthy or context deadline.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(30 * time.Second)

	for {
		if m.probe() {
			break
		}
		select {
		case <-ctx.Done():
			m.setStatus(ModuleStatusStopped, "start cancelled")
			return ctx.Err()
		case <-timeout:
			m.setStatus(ModuleStatusCrashed, "remote service not reachable within 30s")
			return fmt.Errorf("http module: %s not reachable within 30s", m.cfg.BaseURL)
		case <-ticker.C:
		}
	}

	m.mu.Lock()
	m.state.Status = ModuleStatusHealthy
	m.state.LastError = ""
	m.startedAt = time.Now()
	m.mu.Unlock()

	// Start background health poller.
	hctx, hcancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancel = hcancel
	m.mu.Unlock()
	go m.healthLoop(hctx)

	log.Printf("[http-module] connected to %s", m.cfg.BaseURL)
	return nil
}

// Stop sends POST /shutdown to the remote service and stops health polling.
func (m *HTTPModule) Stop(_ context.Context) error {
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	// Best-effort shutdown call.
	ctx, cf := context.WithTimeout(context.Background(), 5*time.Second)
	defer cf()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.BaseURL+"/shutdown", nil)
	if err == nil {
		resp, rerr := m.client.Do(req)
		if rerr == nil {
			resp.Body.Close()
		}
	}

	m.setStatus(ModuleStatusStopped, "")
	log.Printf("[http-module] stopped (%s)", m.cfg.BaseURL)
	return err
}

// probe sends a single GET /health and returns true if 200.
func (m *HTTPModule) probe() bool {
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.HealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.BaseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (m *HTTPModule) healthLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.HealthInterval)
	defer ticker.Stop()
	failures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if m.probe() {
			if failures > 0 {
				log.Printf("[http-module] recovered after %d failures", failures)
				failures = 0
				m.setStatus(ModuleStatusHealthy, "")
			}
			continue
		}

		failures++
		m.mu.Lock()
		m.state.LastError = fmt.Sprintf("health check failed (%d consecutive)", failures)
		m.mu.Unlock()

		if failures == 3 {
			log.Printf("[http-module] degraded: 3 consecutive health failures")
			m.setStatus(ModuleStatusDegraded, m.state.LastError)
		}
	}
}

func (m *HTTPModule) setStatus(status ModuleStatus, lastErr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Status = status
	if lastErr != "" {
		m.state.LastError = lastErr
	}
}

// httpEncoder implements Encoder via POST /v1/synthesize.
type httpEncoder struct {
	module *HTTPModule
}

func (e *httpEncoder) Encode(intent *CognitiveIntent) (*EncodedOutput, error) {
	body := map[string]any{
		"text":   intent.Content,
		"format": "wav",
	}
	if v, ok := intent.Params["voice"]; ok {
		body["voice"] = v
	}
	if v, ok := intent.Params["speed"]; ok {
		body["speed"] = v
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("http encoder: marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, e.module.cfg.BaseURL+"/v1/synthesize", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("http encoder: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.module.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http encoder: POST /v1/synthesize: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("http encoder: synthesize returned %d: %s", resp.StatusCode, msg)
	}

	wav, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("http encoder: read body: %w", err)
	}

	out := &EncodedOutput{
		Modality: ModalityVoice,
		Data:     wav,
		MimeType: "audio/wav",
		Metadata: make(map[string]any),
	}

	// Extract duration from response header if present.
	if dur := resp.Header.Get("X-Mod3-Duration-Sec"); dur != "" {
		if secs, err := strconv.ParseFloat(dur, 64); err == nil {
			out.Duration = time.Duration(secs * float64(time.Second))
			out.Metadata["duration_sec"] = secs
		}
	}

	return out, nil
}

// httpGate implements Gate via POST /v1/vad.
type httpGate struct {
	module *HTTPModule
}

func (g *httpGate) Check(raw []byte, _ ModalityType) (*GateResult, error) {
	// POST raw audio bytes — Mod³ /v1/vad accepts audio/wav body directly.
	req, err := http.NewRequest(http.MethodPost, g.module.cfg.BaseURL+"/v1/vad", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("http gate: request: %w", err)
	}
	req.Header.Set("Content-Type", "audio/wav")

	resp, err := g.module.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http gate: POST /v1/vad: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("http gate: vad returned %d: %s", resp.StatusCode, msg)
	}

	var result struct {
		SpeechDetected bool    `json:"speech_detected"`
		Confidence     float64 `json:"confidence"`
		Reason         string  `json:"reason,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("http gate: decode response: %w", err)
	}

	return &GateResult{
		Allowed:    result.SpeechDetected,
		Confidence: result.Confidence,
		Reason:     result.Reason,
	}, nil
}
