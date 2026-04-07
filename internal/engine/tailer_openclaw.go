package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

const DefaultOpenClawTailerScanInterval = time.Second

// OpenClawTailer watches OpenClaw JSONL logs and emits normalized CogBlocks.
type OpenClawTailer struct {
	Watcher      *FileWatcher
	ScanInterval time.Duration
}

func (t *OpenClawTailer) Name() string {
	return "openclaw"
}

func (t *OpenClawTailer) Tail(ctx context.Context, path string, out chan<- CogBlock) error {
	if out == nil {
		return errors.New("openclaw tailer: nil output channel")
	}

	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		return t.tailDirectory(ctx, path, out)
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("openclaw tailer: stat path %q: %w", path, err)
	}

	return t.tailFile(ctx, path, out)
}

func (t *OpenClawTailer) tailDirectory(ctx context.Context, dir string, out chan<- CogBlock) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ticker := time.NewTicker(t.scanInterval())
	defer ticker.Stop()

	errCh := make(chan error, 1)
	active := map[string]context.CancelFunc{}
	var mu sync.Mutex

	startFile := func(filePath string) {
		mu.Lock()
		if _, exists := active[filePath]; exists {
			mu.Unlock()
			return
		}
		fileCtx, fileCancel := context.WithCancel(runCtx)
		active[filePath] = fileCancel
		mu.Unlock()

		go func() {
			if err := t.tailFile(fileCtx, filePath, out); err != nil {
				select {
				case errCh <- err:
				default:
				}
				cancel()
			}
			mu.Lock()
			delete(active, filePath)
			mu.Unlock()
		}()
	}

	scan := func() error {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			return fmt.Errorf("openclaw tailer: read directory %q: %w", dir, readErr)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
				continue
			}
			startFile(filepath.Join(dir, entry.Name()))
		}
		return nil
	}

	if err := scan(); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			mu.Lock()
			for _, stop := range active {
				stop()
			}
			mu.Unlock()
			return nil
		case err := <-errCh:
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			return err
		case <-ticker.C:
			if err := scan(); err != nil {
				return err
			}
		}
	}
}

func (t *OpenClawTailer) tailFile(ctx context.Context, path string, out chan<- CogBlock) error {
	return t.watcher().Watch(ctx, path, func(line []byte) error {
		block, ok := normalizeOpenClawLine(line)
		if !ok {
			return nil
		}

		select {
		case out <- block:
			return nil
		case <-ctx.Done():
			return nil
		}
	})
}

func (t *OpenClawTailer) watcher() *FileWatcher {
	if t != nil && t.Watcher != nil {
		return t.Watcher
	}
	return NewFileWatcher(DefaultFileWatcherPollInterval)
}

func (t *OpenClawTailer) scanInterval() time.Duration {
	if t == nil || t.ScanInterval <= 0 {
		return DefaultOpenClawTailerScanInterval
	}
	return t.ScanInterval
}

type openClawJSONLLine struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Timestamp string          `json:"timestamp"`
	SessionID string          `json:"session_id"`
	ThreadID  string          `json:"thread_id"`
}

func normalizeOpenClawLine(line []byte) (CogBlock, bool) {
	var entry openClawJSONLLine
	if err := json.Unmarshal(line, &entry); err != nil {
		return CogBlock{}, false
	}

	kind, ok := openClawKind(entry.Type)
	if !ok {
		return CogBlock{}, false
	}

	now := time.Now().UTC()
	ts := now
	if entry.Timestamp != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, entry.Timestamp); err == nil {
			ts = parsed.UTC()
		}
	}

	blockID := entry.ID
	if blockID == "" {
		blockID = uuid.NewString()
	}

	content := openClawContent(entry.Content)
	block := CogBlock{
		ID:              blockID,
		Timestamp:       ts,
		SessionID:       entry.SessionID,
		ThreadID:        entry.ThreadID,
		SourceChannel:   "openclaw",
		SourceTransport: "jsonl",
		Kind:            kind,
		RawPayload:      append(json.RawMessage(nil), line...),
		Provenance: BlockProvenance{
			OriginSession: entry.SessionID,
			OriginChannel: "openclaw",
			IngestedAt:    now,
			NormalizedBy:  "tailer-openclaw",
		},
		TrustContext: TrustContext{
			Authenticated: true,
			TrustScore:    1.0,
			Scope:         "local",
		},
	}

	if entry.Role != "" || content != "" {
		block.Messages = []ProviderMessage{{
			Role:    entry.Role,
			Content: content,
		}}
	}

	return block, true
}

func openClawKind(value string) (CogBlockKind, bool) {
	switch value {
	case string(BlockMessage):
		return BlockMessage, true
	case string(BlockToolCall):
		return BlockToolCall, true
	case string(BlockToolResult):
		return BlockToolResult, true
	default:
		return "", false
	}
}

func openClawContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}

	compact := make(json.RawMessage, len(raw))
	copy(compact, raw)
	if json.Valid(compact) {
		return string(compact)
	}

	return ""
}
