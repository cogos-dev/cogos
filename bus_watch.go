// bus_watch.go — Watch engine for cog bus watch.
//
// Connects to the kernel's SSE endpoint for live events, applies user-defined
// filters, and supports trigger conditions that break the watch loop.
// Falls back to JSONL file scanning when the kernel isn't running (--offline).

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BusWatcher is the watch engine for cog bus watch.
type BusWatcher struct {
	filter     WatchFilter
	trigger    *WatchFilter // optional break condition
	format     string       // "line", "json", "full"
	limit      int          // 0 = unlimited
	quiet      bool
	offline    bool
	busID      string
	kernelAddr string // e.g. "localhost:5100"
	root       string // workspace root
}

// Run starts the bus watcher, choosing live SSE or offline JSONL mode.
func (w *BusWatcher) Run(ctx context.Context) error {
	if w.offline {
		return w.runOffline(ctx)
	}
	return w.runLive(ctx)
}

// runLive connects to the kernel SSE endpoint and streams events.
func (w *BusWatcher) runLive(ctx context.Context) error {
	url := fmt.Sprintf("http://%s/v1/events/stream?bus_id=%s", w.kernelAddr, w.busID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	client := &http.Client{Timeout: 0} // no timeout for SSE
	resp, err := client.Do(req)
	if err != nil {
		// If kernel is unreachable, suggest offline mode
		return fmt.Errorf("connect to kernel at %s: %w\n  (use --offline to read from JSONL files)", w.kernelAddr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("kernel returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if !w.quiet {
		fmt.Fprintf(os.Stderr, "watching bus %s (live SSE from %s)\n", w.busID, w.kernelAddr)
		if len(w.filter.Types) > 0 || len(w.filter.From) > 0 || len(w.filter.Fields) > 0 {
			fmt.Fprintf(os.Stderr, "filters: %s\n", w.describeFilters())
		}
	}

	matched := 0
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			w.printSummary(matched)
			return nil
		default:
		}

		line := scanner.Text()

		// SSE format: "data: {...}\n\n" — only process data lines
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:] // strip "data: " prefix

		// Parse SSE envelope
		var envelope busEventEnvelope
		if err := json.Unmarshal([]byte(data), &envelope); err != nil {
			continue
		}
		if envelope.Data == nil {
			continue
		}

		evt := envelope.Data

		// Skip replay events if --no-replay is set
		if w.filter.NoReplay && strings.HasPrefix(envelope.ID, "replay_") {
			continue
		}

		ok, triggered := w.processEvent(evt)
		if ok {
			matched++
			fmt.Println(w.formatEvent(evt))
		}
		if triggered {
			if !w.quiet {
				fmt.Fprintf(os.Stderr, "trigger matched — stopping\n")
			}
			w.printSummary(matched)
			return nil
		}
		if w.limit > 0 && matched >= w.limit {
			w.printSummary(matched)
			return nil
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("read SSE stream: %w", err)
	}

	w.printSummary(matched)
	return nil
}

// runOffline reads events from the JSONL file on disk.
func (w *BusWatcher) runOffline(ctx context.Context) error {
	eventsFile := filepath.Join(w.root, ".cog", ".state", "buses", w.busID, "events.jsonl")

	f, err := os.Open(eventsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no events file for bus %s\n  path: %s", w.busID, eventsFile)
		}
		return fmt.Errorf("open events file: %w", err)
	}
	defer f.Close()

	if !w.quiet {
		fmt.Fprintf(os.Stderr, "watching bus %s (offline from %s)\n", w.busID, eventsFile)
	}

	matched := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			w.printSummary(matched)
			return nil
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		var evt CogBlock
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}

		ok, triggered := w.processEvent(&evt)
		if ok {
			matched++
			fmt.Println(w.formatEvent(&evt))
		}
		if triggered {
			if !w.quiet {
				fmt.Fprintf(os.Stderr, "trigger matched — stopping\n")
			}
			w.printSummary(matched)
			return nil
		}
		if w.limit > 0 && matched >= w.limit {
			w.printSummary(matched)
			return nil
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read events file: %w", err)
	}

	w.printSummary(matched)
	return nil
}

// processEvent evaluates an event against filters and trigger.
// Returns (matched, triggered).
func (w *BusWatcher) processEvent(evt *CogBlock) (bool, bool) {
	matched := w.filter.Match(evt)
	triggered := false
	if w.trigger != nil {
		triggered = w.trigger.Match(evt)
	}
	return matched, triggered
}

// formatEvent formats a CogBlock according to the selected output format.
func (w *BusWatcher) formatEvent(evt *CogBlock) string {
	switch w.format {
	case "json":
		data, err := json.Marshal(evt)
		if err != nil {
			return fmt.Sprintf("{\"error\":\"%s\"}", err)
		}
		return string(data)
	case "full":
		data, err := json.MarshalIndent(evt, "", "  ")
		if err != nil {
			return fmt.Sprintf("{\"error\":\"%s\"}", err)
		}
		return string(data)
	default: // "line"
		return formatBusEvent(evt)
	}
}

// describeFilters returns a human-readable summary of active filters.
func (w *BusWatcher) describeFilters() string {
	var parts []string
	if len(w.filter.Types) > 0 {
		parts = append(parts, fmt.Sprintf("type=%s", strings.Join(w.filter.Types, "|")))
	}
	if len(w.filter.From) > 0 {
		parts = append(parts, fmt.Sprintf("from=%s", strings.Join(w.filter.From, "|")))
	}
	if len(w.filter.To) > 0 {
		parts = append(parts, fmt.Sprintf("to=%s", strings.Join(w.filter.To, "|")))
	}
	for _, ff := range w.filter.Fields {
		parts = append(parts, describeFieldFilter(ff))
	}
	return strings.Join(parts, ", ")
}

// describeFieldFilter returns a human-readable description of a field filter.
func describeFieldFilter(ff FieldFilter) string {
	switch ff.Op {
	case OpExists:
		return ff.Key + " exists"
	case OpGlob:
		return ff.Key + "=" + ff.Val
	case OpNeq:
		return ff.Key + "!=" + ff.Val
	case OpGt:
		return ff.Key + ">" + ff.Val
	case OpLt:
		return ff.Key + "<" + ff.Val
	case OpGte:
		return ff.Key + ">=" + ff.Val
	case OpLte:
		return ff.Key + "<=" + ff.Val
	default:
		return ff.Key
	}
}

// printSummary prints a watcher summary line to stderr.
func (w *BusWatcher) printSummary(matched int) {
	if !w.quiet {
		fmt.Fprintf(os.Stderr, "%d events matched\n", matched)
	}
}

// autoDetectBus finds the best bus to watch from the registry.
// Prefers buses with "cogbus" participants, falls back to most recently active.
func autoDetectBus(root string) (string, error) {
	registryPath := filepath.Join(root, ".cog", ".state", "buses", "registry.json")
	data, err := os.ReadFile(registryPath)
	if err != nil {
		return "", fmt.Errorf("read bus registry: %w (is the workspace initialized?)", err)
	}

	var entries []busRegistryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return "", fmt.Errorf("parse bus registry: %w", err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no buses registered")
	}

	// Prefer active buses with cogbus bridge participants
	var best *busRegistryEntry
	for i := range entries {
		e := &entries[i]
		if e.State != "active" {
			continue
		}
		for _, p := range e.Participants {
			if strings.Contains(p, "cogbus") || strings.Contains(p, "openclaw") {
				return e.BusID, nil
			}
		}
		// Track the one with highest event count as fallback
		if best == nil || e.EventCount > best.EventCount {
			best = e
		}
	}

	// Fall back to any active bus
	if best != nil {
		return best.BusID, nil
	}

	// Last resort: most recent entry regardless of state
	last := entries[len(entries)-1]
	return last.BusID, nil
}

// parseSinceDuration parses a --since value as either a duration ("5m", "1h")
// or an absolute timestamp (RFC3339).
func parseSinceDuration(s string) (time.Time, error) {
	// Try RFC3339 first
	t, err := time.Parse(time.RFC3339, s)
	if err == nil {
		return t, nil
	}

	// Try RFC3339Nano
	t, err = time.Parse(time.RFC3339Nano, s)
	if err == nil {
		return t, nil
	}

	// Try as duration relative to now
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --since value %q: expected duration (5m, 1h) or RFC3339 timestamp", s)
	}
	return time.Now().Add(-d), nil
}
