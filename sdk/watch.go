package sdk

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchEventType indicates the type of change that occurred.
type WatchEventType string

const (
	// WatchCreated indicates a resource was created.
	WatchCreated WatchEventType = "created"

	// WatchModified indicates a resource was modified.
	WatchModified WatchEventType = "modified"

	// WatchDeleted indicates a resource was deleted.
	WatchDeleted WatchEventType = "deleted"
)

// WatchEvent represents a change to a watched resource.
type WatchEvent struct {
	// URI is the cog:// URI of the changed resource.
	URI string `json:"uri"`

	// Type indicates what kind of change occurred.
	Type WatchEventType `json:"type"`

	// Timestamp is when the event was detected.
	Timestamp time.Time `json:"timestamp"`

	// Resource is the updated resource (nil for Deleted events).
	Resource *Resource `json:"resource,omitempty"`

	// FilePath is the underlying file path that changed.
	FilePath string `json:"file_path,omitempty"`
}

// Watcher observes URI changes and emits events.
type Watcher struct {
	// URI is the URI pattern being watched.
	URI string

	// Events is the channel that receives watch events.
	Events <-chan WatchEvent

	// internal fields
	events   chan WatchEvent
	fsWatch  *fsnotify.Watcher
	cancel   context.CancelFunc
	done     chan struct{}
	kernel   *Kernel
	mu       sync.Mutex
	closed   bool
}

// Close stops watching and releases resources.
func (w *Watcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true

	// Cancel context to signal goroutines to stop
	if w.cancel != nil {
		w.cancel()
	}

	// Close the fsnotify watcher
	var err error
	if w.fsWatch != nil {
		err = w.fsWatch.Close()
	}

	// Wait for event loop to finish
	if w.done != nil {
		<-w.done
	}

	return err
}

// WatchURI subscribes to changes on a URI pattern.
//
// Supports wildcards in the path:
//   - cog://mem/semantic/* watches all files in semantic/
//   - cog://mem/semantic/** watches all files recursively
//   - cog://signals/* watches all signal namespaces
//
// The returned Watcher's Events channel will emit WatchEvents
// when resources matching the pattern change.
//
// Call Watcher.Close() when done to release resources.
//
// Example:
//
//	watcher, err := kernel.WatchURI(ctx, "cog://mem/semantic/*")
//	if err != nil {
//	    return err
//	}
//	defer watcher.Close()
//
//	for event := range watcher.Events {
//	    fmt.Printf("Changed: %s (%s)\n", event.URI, event.Type)
//	}
func (k *Kernel) WatchURI(ctx context.Context, uriPattern string) (*Watcher, error) {
	if k.closed.Load() {
		return nil, NewError("Watch", ErrNotConnected)
	}

	// Parse the URI pattern
	parsed, err := parseWatchPattern(uriPattern)
	if err != nil {
		return nil, err
	}

	// Resolve to file paths
	watchPaths, pathMapping, err := k.resolveWatchPaths(parsed)
	if err != nil {
		return nil, err
	}

	// Create fsnotify watcher
	fsWatch, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, NewError("Watch", err)
	}

	// Add paths to watcher
	for _, path := range watchPaths {
		if err := fsWatch.Add(path); err != nil {
			fsWatch.Close()
			return nil, NewPathError("Watch", path, err)
		}
	}

	// Create cancellable context
	ctx, cancel := context.WithCancel(ctx)

	// Create watcher
	events := make(chan WatchEvent, 100)
	w := &Watcher{
		URI:     uriPattern,
		Events:  events,
		events:  events,
		fsWatch: fsWatch,
		cancel:  cancel,
		done:    make(chan struct{}),
		kernel:  k,
	}

	// Start event loop
	go w.eventLoop(ctx, parsed, pathMapping)

	return w, nil
}

// eventLoop processes fsnotify events and converts them to WatchEvents.
func (w *Watcher) eventLoop(ctx context.Context, pattern *watchPattern, pathMapping map[string]string) {
	defer close(w.done)
	defer close(w.events)

	// Debounce map to coalesce rapid changes
	debounce := make(map[string]time.Time)
	const debounceWindow = 50 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return

		case event, ok := <-w.fsWatch.Events:
			if !ok {
				return
			}

			// Check debounce
			now := time.Now()
			if lastTime, exists := debounce[event.Name]; exists {
				if now.Sub(lastTime) < debounceWindow {
					continue
				}
			}
			debounce[event.Name] = now

			// Convert to WatchEvent
			watchEvent := w.convertFSEvent(event, pattern, pathMapping)
			if watchEvent != nil {
				select {
				case w.events <- *watchEvent:
				case <-ctx.Done():
					return
				default:
					// Channel full, drop event
				}
			}

		case err, ok := <-w.fsWatch.Errors:
			if !ok {
				return
			}
			// Log error but continue
			_ = err
		}
	}
}

// convertFSEvent converts an fsnotify event to a WatchEvent.
func (w *Watcher) convertFSEvent(event fsnotify.Event, pattern *watchPattern, pathMapping map[string]string) *WatchEvent {
	// Determine event type
	var eventType WatchEventType
	switch {
	case event.Has(fsnotify.Create):
		eventType = WatchCreated
	case event.Has(fsnotify.Write):
		eventType = WatchModified
	case event.Has(fsnotify.Remove):
		eventType = WatchDeleted
	case event.Has(fsnotify.Rename):
		eventType = WatchDeleted
	default:
		return nil
	}

	// Convert file path to URI
	uri := w.pathToURI(event.Name, pattern, pathMapping)
	if uri == "" {
		return nil
	}

	watchEvent := &WatchEvent{
		URI:       uri,
		Type:      eventType,
		Timestamp: time.Now(),
		FilePath:  event.Name,
	}

	// Try to resolve the resource (except for deletions)
	if eventType != WatchDeleted {
		if resource, err := w.kernel.Resolve(uri); err == nil {
			watchEvent.Resource = resource
		}
	}

	return watchEvent
}

// pathToURI converts a file path back to a cog:// URI.
func (w *Watcher) pathToURI(filePath string, pattern *watchPattern, pathMapping map[string]string) string {
	// Check if path matches any known mapping
	for basePath, baseURI := range pathMapping {
		if strings.HasPrefix(filePath, basePath) {
			relPath := strings.TrimPrefix(filePath, basePath)
			relPath = strings.TrimPrefix(relPath, "/")

			// Remove .cog.md or .md extension for memory URIs
			if pattern.namespace == "memory" {
				relPath = strings.TrimSuffix(relPath, ".cog.md")
				relPath = strings.TrimSuffix(relPath, ".md")
			}

			if relPath == "" {
				return baseURI
			}
			return baseURI + "/" + relPath
		}
	}

	// Try to infer from kernel paths
	cogDir := w.kernel.CogDir()
	memDir := w.kernel.MemoryDir()

	if strings.HasPrefix(filePath, memDir) {
		relPath := strings.TrimPrefix(filePath, memDir+"/")
		relPath = strings.TrimSuffix(relPath, ".cog.md")
		relPath = strings.TrimSuffix(relPath, ".md")
		return "cog://mem/" + relPath
	}

	if strings.HasPrefix(filePath, cogDir) {
		relPath := strings.TrimPrefix(filePath, cogDir+"/")
		// Determine namespace from path
		parts := strings.SplitN(relPath, "/", 2)
		if len(parts) > 0 {
			switch parts[0] {
			case "adr":
				if len(parts) > 1 {
					name := strings.TrimSuffix(parts[1], ".md")
					return "cog://adr/" + name
				}
				return "cog://adr"
			case "ledger":
				if len(parts) > 1 {
					return "cog://ledger/" + parts[1]
				}
				return "cog://ledger"
			case "signals":
				if len(parts) > 1 {
					return "cog://signals/" + parts[1]
				}
				return "cog://signals"
			}
		}
	}

	return ""
}

// watchPattern represents a parsed watch URI pattern.
type watchPattern struct {
	namespace string
	path      string
	wildcard  bool
	recursive bool
	raw       string
}

// parseWatchPattern parses a cog:// URI pattern for watching.
func parseWatchPattern(pattern string) (*watchPattern, error) {
	if !strings.HasPrefix(pattern, "cog://") {
		return nil, InvalidURIError(pattern, "must start with cog://")
	}

	rest := strings.TrimPrefix(pattern, "cog://")
	parts := strings.SplitN(rest, "/", 2)

	if len(parts) == 0 || parts[0] == "" {
		return nil, InvalidURIError(pattern, "missing namespace")
	}

	namespace := parts[0]

	// Check for valid namespace (allow wildcards in validation)
	if namespace != "*" && !Namespaces[namespace] {
		return nil, NewURIError("Watch", pattern, ErrUnknownNamespace)
	}

	wp := &watchPattern{
		namespace: namespace,
		raw:       pattern,
	}

	if len(parts) > 1 {
		path := parts[1]
		if path == "**" {
			// cog://namespace/** - recursive wildcard at root
			wp.path = ""
			wp.wildcard = true
			wp.recursive = true
		} else if path == "*" {
			// cog://namespace/* - wildcard at root
			wp.path = ""
			wp.wildcard = true
			wp.recursive = false
		} else if strings.HasSuffix(path, "/**") {
			wp.path = strings.TrimSuffix(path, "/**")
			wp.wildcard = true
			wp.recursive = true
		} else if strings.HasSuffix(path, "/*") {
			wp.path = strings.TrimSuffix(path, "/*")
			wp.wildcard = true
			wp.recursive = false
		} else {
			wp.path = path
		}
	}

	return wp, nil
}

// resolveWatchPaths converts a watch pattern to file system paths.
func (k *Kernel) resolveWatchPaths(pattern *watchPattern) ([]string, map[string]string, error) {
	paths := []string{}
	mapping := make(map[string]string)

	switch pattern.namespace {
	case "memory":
		basePath := k.MemoryDir()
		if pattern.path != "" {
			basePath = filepath.Join(basePath, pattern.path)
		}
		baseURI := "cog://memory"
		if pattern.path != "" {
			baseURI += "/" + pattern.path
		}
		paths = append(paths, basePath)
		mapping[basePath] = baseURI

		// If recursive, also add subdirectories
		if pattern.recursive {
			entries, err := filepath.Glob(filepath.Join(basePath, "*"))
			if err == nil {
				for _, entry := range entries {
					paths = append(paths, entry)
					relPath := strings.TrimPrefix(entry, k.MemoryDir()+"/")
					mapping[entry] = "cog://mem/" + relPath
				}
			}
		}

	case "signals":
		basePath := filepath.Join(k.CogDir(), "signals")
		paths = append(paths, basePath)
		mapping[basePath] = "cog://signals"

	case "ledger":
		basePath := filepath.Join(k.CogDir(), "ledger")
		if pattern.path != "" {
			basePath = filepath.Join(basePath, pattern.path)
		}
		paths = append(paths, basePath)
		mapping[basePath] = "cog://ledger"
		if pattern.path != "" {
			mapping[basePath] = "cog://ledger/" + pattern.path
		}

	case "adr":
		basePath := filepath.Join(k.CogDir(), "adr")
		paths = append(paths, basePath)
		mapping[basePath] = "cog://adr"

	case "coherence":
		statePath := k.StateDir()
		paths = append(paths, statePath)
		mapping[statePath] = "cog://coherence"

	default:
		return nil, nil, NewURIError("Watch", pattern.raw, ErrUnknownNamespace)
	}

	return paths, mapping, nil
}
