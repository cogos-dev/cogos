// bep_model.go — AgentSyncModel: handles Index exchange, Request/Response for
// file transfer, and conflict detection. Bridges BEP protocol messages to the
// existing BEPProvider's ReceiveAgentCRD/RemoveAgentCRD for local writes.

package main

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

const agentSyncFolderID = "cogos-agent-defs"

// ─── AgentSyncModel ─────────────────────────────────────────────────────────────

// AgentSyncModel manages the sync state for agent CRD files between peers.
// It owns the local index, processes incoming Index/IndexUpdate messages,
// and dispatches Request/Response messages to transfer files.
type AgentSyncModel struct {
	mu sync.Mutex

	engine   *BEPEngine
	folderID string
	shortID  uint64
	watchDir string
	stateDir string

	localIndex map[string]*IndexEntry            // filename → entry
	peerIndex  map[DeviceID]map[string]*IndexEntry // per-peer index

	nextRequestID atomic.Int32
	pendingReqs   map[int32]*pendingRequest
	pendingMu     sync.Mutex

	emitEvent func(SyncEvent) // event emission callback
}

type pendingRequest struct {
	filename string
	peerID   DeviceID
	entry    *IndexEntry
}

// NewAgentSyncModel creates a sync model for the given engine.
func NewAgentSyncModel(engine *BEPEngine, watchDir, stateDir string, shortID uint64) *AgentSyncModel {
	return &AgentSyncModel{
		engine:      engine,
		folderID:    agentSyncFolderID,
		shortID:     shortID,
		watchDir:    watchDir,
		stateDir:    stateDir,
		localIndex:  make(map[string]*IndexEntry),
		peerIndex:   make(map[DeviceID]map[string]*IndexEntry),
		pendingReqs: make(map[int32]*pendingRequest),
	}
}

// SetEventEmitter sets the callback for sync event emission.
func (m *AgentSyncModel) SetEventEmitter(fn func(SyncEvent)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.emitEvent = fn
}

// ─── Index lifecycle ────────────────────────────────────────────────────────────

// LoadAndScanIndex loads the persisted index, scans disk for changes, and
// returns the current local index as BEPFileInfo list for initial Index message.
func (m *AgentSyncModel) LoadAndScanIndex() []*BEPFileInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Load previously persisted index.
	prev, err := LoadPersistedIndex(m.stateDir)
	if err != nil {
		log.Printf("[bep-model] failed to load persisted index: %v", err)
		prev = make(map[string]*IndexEntry)
	}

	// Scan disk and detect changes since last run.
	m.localIndex = ScanLocalIndex(m.watchDir, m.shortID, prev)

	// Persist updated index.
	deviceIDStr := ""
	if m.engine != nil {
		deviceIDStr = FormatDeviceID(m.engine.deviceID)
	}
	if err := PersistIndex(m.stateDir, deviceIDStr, m.localIndex); err != nil {
		log.Printf("[bep-model] failed to persist index: %v", err)
	}

	// Convert to BEP format.
	var files []*BEPFileInfo
	for _, entry := range m.localIndex {
		files = append(files, entry.ToBEPFileInfo(m.shortID))
	}
	return files
}

// ─── Handle incoming Index ──────────────────────────────────────────────────────

// HandleIndex processes a full Index message from a peer. Diffs against local
// state and sends Requests for files we need.
func (m *AgentSyncModel) HandleIndex(peerID DeviceID, files []*BEPFileInfo) {
	m.mu.Lock()

	// Build peer index.
	peerIdx := make(map[string]*IndexEntry, len(files))
	for _, fi := range files {
		peerIdx[fi.Name] = IndexEntryFromBEP(fi)
	}
	m.peerIndex[peerID] = peerIdx

	// Diff against local.
	diff := DiffIndex(m.localIndex, peerIdx)
	m.mu.Unlock()

	// Emit event.
	if m.emitEvent != nil {
		m.emitEvent(SyncEvent{
			Type: SyncEventIndexComplete,
			Summary: map[string]any{
				"peer":       FormatDeviceID(peerID)[:7],
				"files":      len(files),
				"to_request": len(diff.ToRequest),
				"conflicts":  len(diff.Conflicts),
			},
		})
	}

	// Handle conflicts.
	for _, name := range diff.Conflicts {
		log.Printf("[bep-model] conflict detected: %s (peer %s)", name, FormatDeviceID(peerID)[:7])
		if m.emitEvent != nil {
			m.emitEvent(SyncEvent{
				Type:    SyncEventConflict,
				Summary: map[string]any{"file": name, "peer": FormatDeviceID(peerID)[:7]},
			})
		}
	}

	// Request missing/outdated files (or apply deletions directly).
	for _, name := range diff.ToRequest {
		entry := peerIdx[name]
		if entry != nil && entry.Deleted {
			m.applyRemoteDeletion(peerID, name, entry)
		} else {
			m.sendRequest(peerID, name, entry)
		}
	}
}

// HandleIndexUpdate processes an IndexUpdate from a peer (incremental).
func (m *AgentSyncModel) HandleIndexUpdate(peerID DeviceID, files []*BEPFileInfo) {
	m.mu.Lock()

	// Update peer index.
	if _, ok := m.peerIndex[peerID]; !ok {
		m.peerIndex[peerID] = make(map[string]*IndexEntry)
	}
	for _, fi := range files {
		m.peerIndex[peerID][fi.Name] = IndexEntryFromBEP(fi)
	}

	// Diff the updated entries against local.
	updatedRemote := make(map[string]*IndexEntry, len(files))
	for _, fi := range files {
		updatedRemote[fi.Name] = IndexEntryFromBEP(fi)
	}
	diff := DiffIndex(m.localIndex, updatedRemote)
	m.mu.Unlock()

	// Handle conflicts.
	for _, name := range diff.Conflicts {
		log.Printf("[bep-model] conflict on update: %s (peer %s)", name, FormatDeviceID(peerID)[:7])
		if m.emitEvent != nil {
			m.emitEvent(SyncEvent{
				Type:    SyncEventConflict,
				Summary: map[string]any{"file": name, "peer": FormatDeviceID(peerID)[:7]},
			})
		}
	}

	// Request files (or apply deletions directly).
	for _, name := range diff.ToRequest {
		entry := updatedRemote[name]
		if entry != nil && entry.Deleted {
			m.applyRemoteDeletion(peerID, name, entry)
		} else {
			m.sendRequest(peerID, name, entry)
		}
	}
}

// ─── Remote deletion ────────────────────────────────────────────────────────────

// applyRemoteDeletion handles a file marked Deleted in the remote index.
// Instead of sending a Request (file no longer exists on peer), removes locally.
func (m *AgentSyncModel) applyRemoteDeletion(peerID DeviceID, filename string, entry *IndexEntry) {
	peerShort := FormatDeviceID(peerID)[:7]

	if m.engine != nil && m.engine.provider != nil {
		if err := m.engine.provider.RemoveAgentCRD(peerShort, filename); err != nil {
			log.Printf("[bep-model] failed to remove %s on deletion sync: %v", filename, err)
		}
	}

	log.Printf("[bep-model] applied remote deletion of %s from peer %s", filename, peerShort)
	if m.emitEvent != nil {
		m.emitEvent(SyncEvent{
			Type:    SyncEventFileReceived,
			Summary: map[string]any{"file": filename, "peer": peerShort, "action": "delete"},
		})
	}

	m.updateLocalIndex(filename, entry)
}

// ─── Request / Response ─────────────────────────────────────────────────────────

func (m *AgentSyncModel) sendRequest(peerID DeviceID, filename string, entry *IndexEntry) {
	reqID := m.nextRequestID.Add(1)

	m.pendingMu.Lock()
	m.pendingReqs[reqID] = &pendingRequest{
		filename: filename,
		peerID:   peerID,
		entry:    entry,
	}
	m.pendingMu.Unlock()

	req := &BEPRequest{
		ID:     reqID,
		Folder: m.folderID,
		Name:   filename,
		Offset: 0,
		Size:   int32(entry.Size),
	}

	if m.engine != nil {
		m.engine.SendToPeer(peerID, MessageTypeRequest, req.Marshal())
	}
}

// HandleRequest processes an incoming file request from a peer.
// Reads the file from disk and returns a Response.
func (m *AgentSyncModel) HandleRequest(req *BEPRequest) *BEPResponse {
	resp := &BEPResponse{ID: req.ID}

	// Validate filename.
	if !isAgentCRDFile(req.Name) {
		resp.Code = ErrorCodeInvalidFile
		return resp
	}

	filePath := filepath.Join(m.watchDir, req.Name)
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			resp.Code = ErrorCodeNoSuchFile
		} else {
			resp.Code = ErrorCodeGeneric
		}
		return resp
	}

	resp.Data = data
	resp.Code = ErrorCodeNoError
	return resp
}

// HandleResponse processes a file response from a peer.
// Writes the received data via BEPProvider for atomic file operations.
func (m *AgentSyncModel) HandleResponse(resp *BEPResponse) {
	m.pendingMu.Lock()
	pending, ok := m.pendingReqs[resp.ID]
	if ok {
		delete(m.pendingReqs, resp.ID)
	}
	m.pendingMu.Unlock()

	if !ok {
		log.Printf("[bep-model] received response for unknown request %d", resp.ID)
		return
	}

	peerShort := FormatDeviceID(pending.peerID)[:7]

	if resp.Code != ErrorCodeNoError {
		log.Printf("[bep-model] peer %s returned error %d for %s", peerShort, resp.Code, pending.filename)
		return
	}

	// Handle deletion.
	if pending.entry != nil && pending.entry.Deleted {
		if m.engine != nil && m.engine.provider != nil {
			if err := m.engine.provider.RemoveAgentCRD(peerShort, pending.filename); err != nil {
				log.Printf("[bep-model] failed to remove %s: %v", pending.filename, err)
			}
		}
		if m.emitEvent != nil {
			m.emitEvent(SyncEvent{
				Type:    SyncEventFileReceived,
				Summary: map[string]any{"file": pending.filename, "peer": peerShort, "action": "delete"},
			})
		}
		m.updateLocalIndex(pending.filename, pending.entry)
		return
	}

	// Write received file via provider (atomic write + validation).
	if m.engine != nil && m.engine.provider != nil {
		if err := m.engine.provider.ReceiveAgentCRD(peerShort, pending.filename, resp.Data); err != nil {
			log.Printf("[bep-model] failed to write %s: %v", pending.filename, err)
			return
		}
	}

	log.Printf("[bep-model] received %s from peer %s (%d bytes)", pending.filename, peerShort, len(resp.Data))
	if m.emitEvent != nil {
		m.emitEvent(SyncEvent{
			Type:    SyncEventFileReceived,
			Summary: map[string]any{"file": pending.filename, "peer": peerShort, "size": len(resp.Data)},
		})
	}

	// Update local index with the received entry.
	m.updateLocalIndex(pending.filename, pending.entry)
}

func (m *AgentSyncModel) updateLocalIndex(filename string, entry *IndexEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if entry.Deleted {
		if existing, ok := m.localIndex[filename]; ok {
			existing.Deleted = true
			existing.Version.Merge(entry.Version)
		}
	} else {
		m.localIndex[filename] = entry
	}

	// Persist.
	deviceIDStr := ""
	if m.engine != nil {
		deviceIDStr = FormatDeviceID(m.engine.deviceID)
	}
	if err := PersistIndex(m.stateDir, deviceIDStr, m.localIndex); err != nil {
		log.Printf("[bep-model] failed to persist index: %v", err)
	}
}

// ─── Local change notification ──────────────────────────────────────────────────

// NotifyLocalChange handles a local file change detected by BEPProvider.
// Re-scans the changed file and sends IndexUpdate to all peers.
func (m *AgentSyncModel) NotifyLocalChange(filename string) {
	m.mu.Lock()

	// Re-scan just this file.
	prev := m.localIndex
	m.localIndex = ScanLocalIndex(m.watchDir, m.shortID, prev)

	// Find changed entry.
	entry, ok := m.localIndex[filename]
	if !ok {
		// File might have been deleted — check.
		if _, existed := prev[filename]; existed {
			entry = &IndexEntry{
				Name:    filename,
				Deleted: true,
				Version: NewVersionVector(),
			}
			if prevEntry := prev[filename]; prevEntry != nil && prevEntry.Version != nil {
				entry.Version.Merge(prevEntry.Version)
			}
			entry.Sequence = int64(entry.Version.Increment(m.shortID))
			m.localIndex[filename] = entry
		} else {
			m.mu.Unlock()
			return
		}
	}

	// Convert to BEP.
	fi := entry.ToBEPFileInfo(m.shortID)

	// Persist.
	deviceIDStr := ""
	if m.engine != nil {
		deviceIDStr = FormatDeviceID(m.engine.deviceID)
	}
	_ = PersistIndex(m.stateDir, deviceIDStr, m.localIndex)
	m.mu.Unlock()

	// Send IndexUpdate to all connected peers.
	update := &BEPIndex{
		Folder: m.folderID,
		Files:  []*BEPFileInfo{fi},
	}

	if m.engine != nil {
		m.engine.BroadcastMessage(MessageTypeIndexUpdate, update.Marshal())
	}

	if m.emitEvent != nil {
		m.emitEvent(SyncEvent{
			Type:    SyncEventFileSent,
			Summary: map[string]any{"file": filename, "deleted": entry.Deleted},
		})
	}
}
