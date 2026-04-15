// modality_hud.go — Formats modality bus state for agent context injection (zone 5).
package main

import (
	"fmt"
	"strings"
	"time"
)

// ModalityHUD formats modality bus state for injection into agent context.
type ModalityHUD struct {
	bus      *ModalityBus
	channels *ChannelRegistry
}

// NewModalityHUD creates a HUD formatter.
func NewModalityHUD(bus *ModalityBus, channels *ChannelRegistry) *ModalityHUD {
	return &ModalityHUD{bus: bus, channels: channels}
}

// Format returns a compact markdown block for context injection.
func (h *ModalityHUD) Format() string {
	hud := h.bus.HUD()
	return fmt.Sprintf("## Modality Bus Status\n\n**Modules:** %s\n**Channels:** %s\n**Recent:** %s\n",
		h.fmtModules(hud), h.fmtChannels(), h.fmtRecent(hud))
}

func (h *ModalityHUD) fmtModules(hud map[string]any) string {
	modules, _ := hud["modules"].(map[string]any)
	if len(modules) == 0 {
		return "none"
	}
	order := h.bus.Order()
	var parts []string
	for _, mt := range order {
		e, ok := modules[string(mt)].(map[string]any)
		if !ok {
			continue
		}
		st, _ := e["status"].(string)
		s := string(mt) + " (" + st
		if pid, ok := e["pid"].(int); ok && pid != 0 {
			s += fmt.Sprintf(", pid:%d", pid)
		}
		if er, ok := e["error"].(string); ok && er != "" {
			s += ", err:" + er
		}
		parts = append(parts, s+")")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " | ")
}

func (h *ModalityHUD) fmtChannels() string {
	snap := h.channels.Snapshot()
	if len(snap) == 0 {
		return "none"
	}
	var parts []string
	for id, d := range snap {
		var m []string
		for _, x := range d.Input {
			m = append(m, string(x)+"-in")
		}
		for _, x := range d.Output {
			m = append(m, string(x)+"-out")
		}
		parts = append(parts, fmt.Sprintf("%s [%s]", id, strings.Join(m, ", ")))
	}
	return strings.Join(parts, " | ")
}

func (h *ModalityHUD) fmtRecent(hud map[string]any) string {
	evs, _ := hud["recent_events"].([]map[string]any)
	if len(evs) == 0 {
		return "none"
	}
	if start := len(evs) - 3; start > 0 {
		evs = evs[start:]
	}
	now := time.Now()
	var parts []string
	for _, ev := range evs {
		parts = append(parts, h.fmtEvent(ev, now))
	}
	return strings.Join(parts, " | ")
}

func (h *ModalityHUD) fmtEvent(ev map[string]any, now time.Time) string {
	typ, _ := ev["type"].(string)
	mod, _ := ev["modality"].(string)
	ch, _ := ev["channel"].(string)
	data, _ := ev["data"].(map[string]any)
	ago := ""
	if ts, ok := ev["timestamp"].(time.Time); ok {
		d := now.Sub(ts)
		if d < time.Second {
			ago = fmt.Sprintf("%dms ago", d.Milliseconds())
		} else if d < time.Minute {
			ago = fmt.Sprintf("%.1fs ago", d.Seconds())
		} else {
			ago = fmt.Sprintf("%dm ago", int(d.Minutes()))
		}
	}
	switch typ {
	case "modality.input":
		s := mod + " input from " + ch
		if c, ok := data["confidence"].(float64); ok {
			s += fmt.Sprintf(", conf:%.2f", c)
		}
		if ago != "" {
			s += ", " + ago
		}
		return s
	case "modality.output":
		s := mod + " output to " + ch
		if m, _ := data["mime_type"].(string); m != "" {
			s += ", " + m
		}
		if ago != "" {
			s += ", " + ago
		}
		return s
	case "modality.gate":
		v := "rejected"
		if a, ok := data["allowed"].(bool); ok && a {
			v = "passed"
		}
		return fmt.Sprintf("%s gate %s on %s", mod, v, ch)
	default:
		return typ
	}
}

// FormatJSON returns the HUD as structured data (for programmatic use).
func (h *ModalityHUD) FormatJSON() map[string]any {
	hud := h.bus.HUD()
	snap := h.channels.Snapshot()
	chs := make(map[string]any, len(snap))
	for id, d := range snap {
		chs[id] = map[string]any{"input": d.Input, "output": d.Output}
	}
	return map[string]any{"modules": hud["modules"], "channels": chs, "recent_events": hud["recent_events"]}
}

// TokenEstimate returns approximate token count for the HUD block (~4 tokens/word).
func (h *ModalityHUD) TokenEstimate() int { return len(strings.Fields(h.Format())) * 4 }
