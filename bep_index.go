// bep_index.go — Version vectors, local index scanning, diff computation,
// and index persistence for BEP agent CRD sync.

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ─── Version vector ordering ────────────────────────────────────────────────────

type Ordering int

const (
	OrderEqual      Ordering = 0
	OrderGreater    Ordering = 1
	OrderLesser     Ordering = 2
	OrderConcurrent Ordering = 3
)

func (o Ordering) String() string {
	switch o {
	case OrderEqual:
		return "equal"
	case OrderGreater:
		return "greater"
	case OrderLesser:
		return "lesser"
	case OrderConcurrent:
		return "concurrent"
	default:
		return "unknown"
	}
}

// ─── VersionVector ──────────────────────────────────────────────────────────────

// VersionVector tracks causality via per-device counters.
type VersionVector struct {
	Counters map[uint64]uint64 `json:"counters"` // shortID → sequence
}

// NewVersionVector creates an empty version vector.
func NewVersionVector() *VersionVector {
	return &VersionVector{Counters: make(map[uint64]uint64)}
}

// Increment bumps the counter for the given device and returns the new value.
func (v *VersionVector) Increment(shortID uint64) uint64 {
	v.Counters[shortID]++
	return v.Counters[shortID]
}

// Merge updates this vector with the maximum of each counter.
func (v *VersionVector) Merge(other *VersionVector) {
	if other == nil {
		return
	}
	for id, val := range other.Counters {
		if val > v.Counters[id] {
			v.Counters[id] = val
		}
	}
}

// Compare determines the ordering relationship between two version vectors.
func (v *VersionVector) Compare(other *VersionVector) Ordering {
	if other == nil {
		if len(v.Counters) == 0 {
			return OrderEqual
		}
		return OrderGreater
	}

	hasGreater := false
	hasLesser := false

	// Check all keys in v.
	for id, val := range v.Counters {
		otherVal := other.Counters[id]
		if val > otherVal {
			hasGreater = true
		} else if val < otherVal {
			hasLesser = true
		}
	}

	// Check keys only in other.
	for id, otherVal := range other.Counters {
		if _, ok := v.Counters[id]; !ok && otherVal > 0 {
			hasLesser = true
		}
	}

	switch {
	case hasGreater && hasLesser:
		return OrderConcurrent
	case hasGreater:
		return OrderGreater
	case hasLesser:
		return OrderLesser
	default:
		return OrderEqual
	}
}

// ToBEP converts to the BEP wire format.
func (v *VersionVector) ToBEP() *BEPVector {
	bv := &BEPVector{}
	for id, val := range v.Counters {
		bv.Counters = append(bv.Counters, &BEPCounter{ID: id, Value: val})
	}
	// Sort for deterministic encoding.
	sort.Slice(bv.Counters, func(i, j int) bool {
		return bv.Counters[i].ID < bv.Counters[j].ID
	})
	return bv
}

// VersionVectorFromBEP converts from BEP wire format.
func VersionVectorFromBEP(bv *BEPVector) *VersionVector {
	v := NewVersionVector()
	if bv != nil {
		for _, c := range bv.Counters {
			v.Counters[c.ID] = c.Value
		}
	}
	return v
}

// ─── IndexEntry: local metadata per file ────────────────────────────────────────

// IndexEntry tracks a single agent CRD file's metadata for sync.
type IndexEntry struct {
	Name       string         `json:"name"`
	Size       int64          `json:"size"`
	ModifiedS  int64          `json:"modified_s"`
	ModifiedNs int32          `json:"modified_ns"`
	Sequence   int64          `json:"sequence"`
	Version    *VersionVector `json:"version"`
	BlocksHash []byte         `json:"blocks_hash"` // SHA-256 of file content
	Deleted    bool           `json:"deleted"`
}

// ToBEPFileInfo converts an IndexEntry to a BEPFileInfo for wire transmission.
func (e *IndexEntry) ToBEPFileInfo(shortID uint64) *BEPFileInfo {
	fi := &BEPFileInfo{
		Name:       e.Name,
		Size:       e.Size,
		ModifiedS:  e.ModifiedS,
		ModifiedNs: e.ModifiedNs,
		ModifiedBy: shortID,
		Deleted:    e.Deleted,
		Sequence:   e.Sequence,
		BlocksHash: e.BlocksHash,
	}
	if e.Version != nil {
		fi.Version = *e.Version.ToBEP()
	}
	// Single block covering entire file (agent CRDs are 1-4 KB).
	if !e.Deleted && e.Size > 0 {
		fi.Blocks = []*BEPBlockInfo{{
			Offset: 0,
			Size:   int32(e.Size),
			Hash:   e.BlocksHash,
		}}
	}
	return fi
}

// IndexEntryFromBEP creates an IndexEntry from a received BEPFileInfo.
func IndexEntryFromBEP(fi *BEPFileInfo) *IndexEntry {
	return &IndexEntry{
		Name:       fi.Name,
		Size:       fi.Size,
		ModifiedS:  fi.ModifiedS,
		ModifiedNs: fi.ModifiedNs,
		Sequence:   fi.Sequence,
		Version:    VersionVectorFromBEP(&fi.Version),
		BlocksHash: fi.BlocksHash,
		Deleted:    fi.Deleted,
	}
}

// ─── Local index scanning ───────────────────────────────────────────────────────

// ScanLocalIndex reads all *.agent.yaml files from dir and builds an index.
// Uses existing version vectors from prevIndex where available.
func ScanLocalIndex(dir string, shortID uint64, prevIndex map[string]*IndexEntry) map[string]*IndexEntry {
	index := make(map[string]*IndexEntry)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return index
	}

	for _, entry := range entries {
		if entry.IsDir() || !isAgentCRDFile(entry.Name()) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}

		hash := sha256.Sum256(data)

		ie := &IndexEntry{
			Name:       entry.Name(),
			Size:       info.Size(),
			ModifiedS:  info.ModTime().Unix(),
			ModifiedNs: int32(info.ModTime().Nanosecond()),
			BlocksHash: hash[:],
		}

		// Preserve version vector from previous index if content unchanged.
		if prev, ok := prevIndex[entry.Name()]; ok && bytesEqual(prev.BlocksHash, hash[:]) {
			ie.Version = prev.Version
			ie.Sequence = prev.Sequence
		} else {
			// Content changed or new file — bump version.
			ie.Version = NewVersionVector()
			if prev, ok := prevIndex[entry.Name()]; ok && prev.Version != nil {
				ie.Version.Merge(prev.Version)
			}
			ie.Sequence = int64(ie.Version.Increment(shortID))
		}

		index[entry.Name()] = ie
	}

	// Detect deletions: files in prevIndex not found on disk.
	for name, prev := range prevIndex {
		if _, ok := index[name]; !ok && !prev.Deleted {
			ie := &IndexEntry{
				Name:    name,
				Deleted: true,
				Version: NewVersionVector(),
			}
			if prev.Version != nil {
				ie.Version.Merge(prev.Version)
			}
			ie.Sequence = int64(ie.Version.Increment(shortID))
			index[name] = ie
		}
	}

	return index
}

// ─── Index diffing ──────────────────────────────────────────────────────────────

// DiffResult holds the result of comparing local and remote indexes.
type DiffResult struct {
	ToRequest []string // files to request from remote (remote is newer)
	Conflicts []string // files with concurrent modifications
}

// DiffIndex compares local and remote indexes to determine what needs syncing.
func DiffIndex(local, remote map[string]*IndexEntry) *DiffResult {
	result := &DiffResult{}

	for name, remoteEntry := range remote {
		localEntry, exists := local[name]

		if !exists {
			// We don't have this file — request it (unless deleted).
			if !remoteEntry.Deleted {
				result.ToRequest = append(result.ToRequest, name)
			}
			continue
		}

		// Compare version vectors.
		localVV := localEntry.Version
		remoteVV := remoteEntry.Version
		if localVV == nil {
			localVV = NewVersionVector()
		}
		if remoteVV == nil {
			remoteVV = NewVersionVector()
		}

		switch localVV.Compare(remoteVV) {
		case OrderEqual:
			// Same version — no action needed.
		case OrderLesser:
			// Remote is newer — request it.
			if remoteEntry.Deleted {
				result.ToRequest = append(result.ToRequest, name)
			} else {
				result.ToRequest = append(result.ToRequest, name)
			}
		case OrderGreater:
			// Local is newer — we'll send it via IndexUpdate (handled by model).
		case OrderConcurrent:
			// Concurrent modification — conflict.
			result.Conflicts = append(result.Conflicts, name)
		}
	}

	// Sort for deterministic ordering.
	sort.Strings(result.ToRequest)
	sort.Strings(result.Conflicts)
	return result
}

// ─── Index persistence ──────────────────────────────────────────────────────────

// PersistedIndex is the JSON structure stored in .cog/.state/bep/index.json.
type PersistedIndex struct {
	DeviceID  string                 `json:"device_id"`
	UpdatedAt string                 `json:"updated_at"`
	Files     map[string]*IndexEntry `json:"files"`
}

// PersistIndex saves the local index to disk.
func PersistIndex(stateDir string, deviceID string, index map[string]*IndexEntry) error {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	pi := &PersistedIndex{
		DeviceID:  deviceID,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Files:     index,
	}

	data, err := json.MarshalIndent(pi, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}

	path := filepath.Join(stateDir, "index.json")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write temp index: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename index: %w", err)
	}
	return nil
}

// LoadPersistedIndex reads the saved index from disk.
// Returns an empty index if the file does not exist.
func LoadPersistedIndex(stateDir string) (map[string]*IndexEntry, error) {
	path := filepath.Join(stateDir, "index.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]*IndexEntry), nil
		}
		return nil, fmt.Errorf("read index: %w", err)
	}

	var pi PersistedIndex
	if err := json.Unmarshal(data, &pi); err != nil {
		return nil, fmt.Errorf("unmarshal index: %w", err)
	}
	if pi.Files == nil {
		pi.Files = make(map[string]*IndexEntry)
	}
	return pi.Files, nil
}

// ─── ShortID ────────────────────────────────────────────────────────────────────

// ShortIDFromDeviceID derives a uint64 short ID from a DeviceID.
// Uses the first 8 bytes of the DeviceID hash.
func ShortIDFromDeviceID(id DeviceID) uint64 {
	return uint64(id[0]) | uint64(id[1])<<8 | uint64(id[2])<<16 | uint64(id[3])<<24 |
		uint64(id[4])<<32 | uint64(id[5])<<40 | uint64(id[6])<<48 | uint64(id[7])<<56
}

// ─── Helpers ────────────────────────────────────────────────────────────────────

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
