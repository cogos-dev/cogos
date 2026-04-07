package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const DefaultSyncWatcherPollInterval = 5 * time.Second

// SyncEnvelope describes a bridge envelope dropped into .cog/sync/inbox/.
type SyncEnvelope struct {
	Version      int    `json:"version"`
	OriginNodeID string `json:"origin_node_id"`
	TargetNodeID string `json:"target_node_id"`
	BlobHash     string `json:"blob_hash"`
	Timestamp    string `json:"timestamp"`
	Kind         string `json:"kind"`
	Signature    string `json:"signature"`
}

// SyncEvent reports a discovered sync envelope and its structural validation result.
type SyncEvent struct {
	Envelope        SyncEnvelope
	FilePath        string
	Valid           bool
	ValidationError string
	AlreadyHave     bool
}

// SyncWatcher polls a Syncthing inbox directory for new SyncEnvelope files.
type SyncWatcher struct {
	BlobStore    *BlobStore
	PollInterval time.Duration
}

// NewSyncWatcher creates a polling sync watcher.
func NewSyncWatcher(blobStore *BlobStore, pollInterval time.Duration) *SyncWatcher {
	return &SyncWatcher{BlobStore: blobStore, PollInterval: pollInterval}
}

// Watch monitors path for new .json envelopes and emits SyncEvents to out.
func (w *SyncWatcher) Watch(ctx context.Context, path string, out chan<- SyncEvent) error {
	if out == nil {
		return errors.New("sync watcher: nil output channel")
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("sync watcher: empty inbox path")
	}
	if w == nil || w.BlobStore == nil {
		return errors.New("sync watcher: nil blob store")
	}

	state := syncWatcherState{
		path: path,
		seen: make(map[string]struct{}),
	}

	ticker := time.NewTicker(w.pollInterval())
	defer ticker.Stop()

	for {
		err := state.poll(func(filePath string) error {
			event := w.inspectFile(filePath)
			select {
			case out <- event:
				return nil
			case <-ctx.Done():
				return context.Canceled
			}
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *SyncWatcher) pollInterval() time.Duration {
	if w == nil || w.PollInterval <= 0 {
		return DefaultSyncWatcherPollInterval
	}
	return w.PollInterval
}

func (w *SyncWatcher) inspectFile(path string) SyncEvent {
	event := SyncEvent{FilePath: path}

	data, err := os.ReadFile(path)
	if err != nil {
		event.ValidationError = fmt.Sprintf("read envelope: %v", err)
		return event
	}

	if err := json.Unmarshal(data, &event.Envelope); err != nil {
		event.ValidationError = fmt.Sprintf("parse envelope: %v", err)
		return event
	}

	if err := validateSyncEnvelope(event.Envelope); err != nil {
		event.ValidationError = err.Error()
		return event
	}

	event.Valid = true
	event.AlreadyHave = w.BlobStore.Exists(strings.ToLower(strings.TrimSpace(event.Envelope.BlobHash)))
	return event
}

func validateSyncEnvelope(envelope SyncEnvelope) error {
	if envelope.Version != 1 {
		return fmt.Errorf("version must be 1")
	}
	if strings.TrimSpace(envelope.OriginNodeID) == "" {
		return errors.New("origin_node_id is required")
	}
	if strings.TrimSpace(envelope.TargetNodeID) == "" {
		return errors.New("target_node_id is required")
	}
	if strings.TrimSpace(envelope.BlobHash) == "" {
		return errors.New("blob_hash is required")
	}
	if !isSHA256Hex(envelope.BlobHash) {
		return errors.New("blob_hash must be a 64-character hex SHA-256")
	}
	if strings.TrimSpace(envelope.Timestamp) == "" {
		return errors.New("timestamp is required")
	}
	if _, err := time.Parse(time.RFC3339, envelope.Timestamp); err != nil {
		return fmt.Errorf("timestamp must be RFC3339: %w", err)
	}
	if strings.TrimSpace(envelope.Kind) == "" {
		return errors.New("kind is required")
	}
	if !isSupportedSyncKind(envelope.Kind) {
		return fmt.Errorf("kind %q is not supported", envelope.Kind)
	}
	if strings.TrimSpace(envelope.Signature) == "" {
		return errors.New("signature is required")
	}
	return nil
}

func isSupportedSyncKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "block", "event", "manifest":
		return true
	default:
		return false
	}
}

func isSHA256Hex(hash string) bool {
	hash = strings.TrimSpace(hash)
	if len(hash) != 64 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

type syncWatcherState struct {
	path string
	seen map[string]struct{}
}

func (s *syncWatcherState) poll(onFile func(string) error) error {
	entries, err := os.ReadDir(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read sync inbox %q: %w", s.path, err)
	}

	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		paths = append(paths, filepath.Join(s.path, entry.Name()))
	}
	sort.Strings(paths)

	for _, path := range paths {
		if _, ok := s.seen[path]; ok {
			continue
		}
		if err := onFile(path); err != nil {
			return err
		}
		s.seen[path] = struct{}{}
	}

	return nil
}
