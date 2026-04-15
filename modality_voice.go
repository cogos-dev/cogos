// modality_voice.go — Voice modality module: subprocess-backed TTS/VAD/STT.
// Skeleton wiring ModalityModule to Python inference via the wire protocol.

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

const (
	voiceTTS = "tts"
	voiceVAD = "vad"
	voiceSTT = "stt"
)

var voiceSubprocesses = []SupervisorConfig{
	{Name: voiceTTS, Command: "python3", Args: []string{"-m", "mod3.worker", "tts"}},
	{Name: voiceVAD, Command: "python3", Args: []string{"-m", "mod3.worker", "vad"}},
	{Name: voiceSTT, Command: "python3", Args: []string{"-m", "mod3.worker", "stt"}},
}

// VoiceModule implements ModalityModule for voice (VAD/STT/TTS via Python).
type VoiceModule struct {
	mu         sync.RWMutex
	supervisor *ProcessSupervisor
	status     ModuleStatus
	startedAt  time.Time
}

func NewVoiceModule(supervisor *ProcessSupervisor) *VoiceModule {
	return &VoiceModule{supervisor: supervisor, status: ModuleStatusStopped}
}

func (m *VoiceModule) Type() ModalityType { return ModalityVoice }
func (m *VoiceModule) Gate() Gate          { return &voiceGate{supervisor: m.supervisor} }
func (m *VoiceModule) Decoder() Decoder    { return &voiceDecoder{supervisor: m.supervisor} }
func (m *VoiceModule) Encoder() Encoder    { return &voiceEncoder{supervisor: m.supervisor} }

func (m *VoiceModule) State() *ModuleState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var uptime time.Duration
	if !m.startedAt.IsZero() && m.status == ModuleStatusHealthy {
		uptime = time.Since(m.startedAt)
	}
	return &ModuleState{Status: m.status, Modality: ModalityVoice, Uptime: uptime}
}

func (m *VoiceModule) Start(ctx context.Context) error {
	m.mu.Lock()
	m.status = ModuleStatusStarting
	m.mu.Unlock()
	for i := range voiceSubprocesses {
		cfg := voiceSubprocesses[i]
		_ = m.supervisor.Register(&cfg) // ignore "already registered"
	}
	for _, cfg := range voiceSubprocesses {
		if err := m.supervisor.Start(ctx, cfg.Name); err != nil {
			m.mu.Lock()
			m.status = ModuleStatusDegraded
			m.mu.Unlock()
			return fmt.Errorf("voice: start %s: %w", cfg.Name, err)
		}
	}
	m.mu.Lock()
	m.status = ModuleStatusHealthy
	m.startedAt = time.Now()
	m.mu.Unlock()
	return nil
}

func (m *VoiceModule) Stop(ctx context.Context) error {
	var firstErr error
	for _, cfg := range voiceSubprocesses {
		if err := m.supervisor.Stop(ctx, cfg.Name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.mu.Lock()
	m.status = ModuleStatusStopped
	m.startedAt = time.Time{}
	m.mu.Unlock()
	return firstErr
}

func (m *VoiceModule) Health() ModuleStatus {
	worst := ModuleStatusHealthy
	for _, cfg := range voiceSubprocesses {
		st, err := m.supervisor.ModuleStatus(cfg.Name)
		if err != nil {
			return ModuleStatusStopped
		}
		if statusSeverity(st.Status) > statusSeverity(worst) {
			worst = st.Status
		}
	}
	return worst
}

var severityMap = map[ModuleStatus]int{
	ModuleStatusHealthy: 0, ModuleStatusStarting: 1, ModuleStatusDegraded: 2,
	ModuleStatusStopped: 3, ModuleStatusCrashed: 4,
}

func statusSeverity(s ModuleStatus) int {
	if v, ok := severityMap[s]; ok {
		return v
	}
	return 5
}

func supervisorConn(sup *ProcessSupervisor, name string) *SubprocessConn {
	return sup.Conn(name)
}

// --- voiceGate (VAD) — implements Gate ---

type voiceGate struct{ supervisor *ProcessSupervisor }

func (g *voiceGate) Check(raw []byte, _ ModalityType) (*GateResult, error) {
	conn := supervisorConn(g.supervisor, voiceVAD)
	if conn == nil {
		return &GateResult{Allowed: true, Confidence: 0, Reason: "vad unavailable"}, nil
	}
	resp, err := conn.Request("vad", "detect", map[string]any{
		"audio_b64": base64.StdEncoding.EncodeToString(raw), "sample_rate": 16000,
	})
	if err != nil {
		return &GateResult{Allowed: true, Confidence: 0, Reason: err.Error()}, nil
	}
	hasSpeech, _ := resp.Result["has_speech"].(bool)
	confidence, _ := resp.Result["confidence"].(float64)
	return &GateResult{Allowed: hasSpeech, Confidence: confidence}, nil
}

// --- voiceDecoder (STT) — implements Decoder ---

type voiceDecoder struct{ supervisor *ProcessSupervisor }

func (d *voiceDecoder) Decode(raw []byte, _ ModalityType, channel string) (*CognitiveEvent, error) {
	conn := supervisorConn(d.supervisor, voiceSTT)
	if conn == nil {
		return nil, fmt.Errorf("voice: stt subprocess not available")
	}
	resp, err := conn.Request("stt", "transcribe", map[string]any{
		"audio_b64": base64.StdEncoding.EncodeToString(raw), "sample_rate": 16000,
	})
	if err != nil {
		return nil, fmt.Errorf("voice: stt: %w", err)
	}
	transcript, _ := resp.Result["transcript"].(string)
	confidence, _ := resp.Result["confidence"].(float64)
	return &CognitiveEvent{
		Modality: ModalityVoice, Channel: channel,
		Content: transcript, Confidence: confidence, Timestamp: time.Now(),
	}, nil
}

// --- voiceEncoder (TTS) — implements Encoder ---

type voiceEncoder struct{ supervisor *ProcessSupervisor }

func (e *voiceEncoder) Encode(intent *CognitiveIntent) (*EncodedOutput, error) {
	conn := supervisorConn(e.supervisor, voiceTTS)
	if conn == nil {
		return nil, fmt.Errorf("voice: tts subprocess not available")
	}
	voice, speed := "bm_lewis", 1.25
	if v, ok := intent.Params["voice"].(string); ok && v != "" {
		voice = v
	}
	if s, ok := intent.Params["speed"].(float64); ok && s > 0 {
		speed = s
	}
	resp, err := conn.Request("tts", "synthesize", map[string]any{
		"text": intent.Content, "voice": voice, "speed": speed,
	})
	if err != nil {
		return nil, fmt.Errorf("voice: tts: %w", err)
	}
	audioB64, _ := resp.Result["audio_b64"].(string)
	durationSec, _ := resp.Result["duration_sec"].(float64)
	audioBytes, err := base64.StdEncoding.DecodeString(audioB64)
	if err != nil {
		return nil, fmt.Errorf("voice: tts: decode audio: %w", err)
	}
	return &EncodedOutput{
		Modality: ModalityVoice, Data: audioBytes, MimeType: "audio/wav",
		Duration: time.Duration(durationSec * float64(time.Second)),
	}, nil
}
