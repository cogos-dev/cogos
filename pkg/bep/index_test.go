package bep

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// ─── VersionVector.Compare ──────────────────────────────────────────────────────

func TestVersionVectorCompareEqual(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 3}}
	b := &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 3}}
	if got := a.Compare(b); got != OrderEqual {
		t.Errorf("Compare = %v, want Equal", got)
	}
}

func TestVersionVectorCompareGreater(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 6, 2: 3}}
	b := &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 3}}
	if got := a.Compare(b); got != OrderGreater {
		t.Errorf("Compare = %v, want Greater", got)
	}
}

func TestVersionVectorCompareLesser(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 4, 2: 3}}
	b := &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 3}}
	if got := a.Compare(b); got != OrderLesser {
		t.Errorf("Compare = %v, want Lesser", got)
	}
}

func TestVersionVectorCompareConcurrent(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 6, 2: 2}}
	b := &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 3}}
	if got := a.Compare(b); got != OrderConcurrent {
		t.Errorf("Compare = %v, want Concurrent", got)
	}
}

func TestVersionVectorCompareNil(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 5}}
	if got := a.Compare(nil); got != OrderGreater {
		t.Errorf("Compare(nil) = %v, want Greater", got)
	}

	empty := NewVersionVector()
	if got := empty.Compare(nil); got != OrderEqual {
		t.Errorf("empty.Compare(nil) = %v, want Equal", got)
	}
}

// ─── VersionVector.Increment / Merge ────────────────────────────────────────────

func TestVersionVectorIncrement(t *testing.T) {
	v := NewVersionVector()
	val := v.Increment(42)
	if val != 1 {
		t.Errorf("first Increment = %d, want 1", val)
	}
	val = v.Increment(42)
	if val != 2 {
		t.Errorf("second Increment = %d, want 2", val)
	}
}

func TestVersionVectorMerge(t *testing.T) {
	a := &VersionVector{Counters: map[uint64]uint64{1: 3, 2: 5}}
	b := &VersionVector{Counters: map[uint64]uint64{1: 5, 3: 7}}
	a.Merge(b)

	if a.Counters[1] != 5 {
		t.Errorf("Counters[1] = %d, want 5", a.Counters[1])
	}
	if a.Counters[2] != 5 {
		t.Errorf("Counters[2] = %d, want 5 (unchanged)", a.Counters[2])
	}
	if a.Counters[3] != 7 {
		t.Errorf("Counters[3] = %d, want 7 (new)", a.Counters[3])
	}
}

// ─── VersionVector BEP round-trip ───────────────────────────────────────────────

func TestVersionVectorBEPRoundTrip(t *testing.T) {
	orig := &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 3}}
	bv := orig.ToBEP()
	got := VersionVectorFromBEP(bv)

	if got.Counters[1] != 5 || got.Counters[2] != 3 {
		t.Errorf("round-trip failed: got %v", got.Counters)
	}
}

// ─── IndexEntry <-> FileInfo ────────────────────────────────────────────────────

func TestIndexEntryToBEPFileInfo(t *testing.T) {
	entry := &IndexEntry{
		Name:       "test.agent.yaml",
		Size:       256,
		ModifiedS:  1700000000,
		ModifiedNs: 500,
		Sequence:   3,
		Version:    &VersionVector{Counters: map[uint64]uint64{42: 3}},
		BlocksHash: []byte("hash"),
	}

	fi := entry.ToBEPFileInfo(42)
	if fi.Name != "test.agent.yaml" {
		t.Errorf("Name = %q", fi.Name)
	}
	if fi.Size != 256 {
		t.Errorf("Size = %d", fi.Size)
	}
	if fi.ModifiedBy != 42 {
		t.Errorf("ModifiedBy = %d", fi.ModifiedBy)
	}
	if len(fi.Blocks) != 1 {
		t.Fatalf("Blocks count = %d, want 1", len(fi.Blocks))
	}
}

func TestIndexEntryFromBEP(t *testing.T) {
	fi := &FileInfo{
		Name:       "test.agent.yaml",
		Size:       256,
		ModifiedS:  1700000000,
		Sequence:   5,
		Version:    Vector{Counters: []*Counter{{ID: 42, Value: 5}}},
		BlocksHash: []byte("hash"),
	}

	entry := IndexEntryFromBEP(fi)
	if entry.Name != fi.Name {
		t.Errorf("Name = %q", entry.Name)
	}
	if entry.Sequence != 5 {
		t.Errorf("Sequence = %d", entry.Sequence)
	}
	if entry.Version.Counters[42] != 5 {
		t.Errorf("Version[42] = %d", entry.Version.Counters[42])
	}
}

// ─── DiffIndex ──────────────────────────────────────────────────────────────────

func TestDiffIndexRemoteNewer(t *testing.T) {
	local := map[string]*IndexEntry{
		"a.agent.yaml": {Name: "a.agent.yaml", Version: &VersionVector{Counters: map[uint64]uint64{1: 3}}},
	}
	remote := map[string]*IndexEntry{
		"a.agent.yaml": {Name: "a.agent.yaml", Version: &VersionVector{Counters: map[uint64]uint64{1: 5}}},
	}

	diff := DiffIndex(local, remote)
	if len(diff.ToRequest) != 1 {
		t.Errorf("ToRequest = %v, want [a.agent.yaml]", diff.ToRequest)
	}
	if len(diff.Conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none", diff.Conflicts)
	}
}

func TestDiffIndexConflict(t *testing.T) {
	local := map[string]*IndexEntry{
		"a.agent.yaml": {Name: "a.agent.yaml", Version: &VersionVector{Counters: map[uint64]uint64{1: 5, 2: 2}}},
	}
	remote := map[string]*IndexEntry{
		"a.agent.yaml": {Name: "a.agent.yaml", Version: &VersionVector{Counters: map[uint64]uint64{1: 3, 2: 4}}},
	}

	diff := DiffIndex(local, remote)
	if len(diff.Conflicts) != 1 {
		t.Errorf("Conflicts = %v, want [a.agent.yaml]", diff.Conflicts)
	}
}

func TestDiffIndexNewRemoteFile(t *testing.T) {
	local := map[string]*IndexEntry{}
	remote := map[string]*IndexEntry{
		"new.agent.yaml": {Name: "new.agent.yaml", Version: NewVersionVector()},
	}

	diff := DiffIndex(local, remote)
	if len(diff.ToRequest) != 1 {
		t.Errorf("ToRequest = %v, want [new.agent.yaml]", diff.ToRequest)
	}
}

// ─── Index persistence ──────────────────────────────────────────────────────────

func TestPersistAndLoadIndex(t *testing.T) {
	dir := t.TempDir()

	index := map[string]*IndexEntry{
		"a.agent.yaml": {
			Name:    "a.agent.yaml",
			Size:    100,
			Version: &VersionVector{Counters: map[uint64]uint64{1: 5}},
		},
		"b.agent.yaml": {
			Name:    "b.agent.yaml",
			Size:    200,
			Deleted: true,
			Version: &VersionVector{Counters: map[uint64]uint64{2: 3}},
		},
	}

	if err := PersistIndex(dir, "TEST-ID", index); err != nil {
		t.Fatalf("PersistIndex: %v", err)
	}

	loaded, err := LoadPersistedIndex(dir)
	if err != nil {
		t.Fatalf("LoadPersistedIndex: %v", err)
	}

	if len(loaded) != 2 {
		t.Fatalf("loaded %d entries, want 2", len(loaded))
	}
	if loaded["a.agent.yaml"].Size != 100 {
		t.Errorf("a.agent.yaml Size = %d", loaded["a.agent.yaml"].Size)
	}
	if !loaded["b.agent.yaml"].Deleted {
		t.Error("b.agent.yaml should be deleted")
	}
}

func TestLoadPersistedIndexMissing(t *testing.T) {
	dir := t.TempDir()
	loaded, err := LoadPersistedIndex(filepath.Join(dir, "nonexistent"))
	if err != nil {
		t.Fatalf("LoadPersistedIndex: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("expected empty index, got %d entries", len(loaded))
	}
}

// ─── ScanLocalIndex ─────────────────────────────────────────────────────────────

func TestScanLocalIndex(t *testing.T) {
	dir := t.TempDir()

	// Write test agent CRD files.
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("agent%d.agent.yaml", i)
		content := fmt.Sprintf("apiVersion: cog.os/v1alpha1\nkind: Agent\nmetadata:\n  name: agent%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// Write a non-CRD file that should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("ignore"), 0644); err != nil {
		t.Fatalf("write readme.txt: %v", err)
	}

	index := ScanLocalIndex(dir, 1, nil)

	if len(index) != 3 {
		t.Errorf("index has %d entries, want 3", len(index))
	}
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("agent%d.agent.yaml", i)
		if _, ok := index[name]; !ok {
			t.Errorf("missing entry for %s", name)
		}
	}
}

func TestScanLocalIndexDetectsDeletion(t *testing.T) {
	dir := t.TempDir()

	prevIndex := map[string]*IndexEntry{
		"deleted.agent.yaml": {
			Name:    "deleted.agent.yaml",
			Version: &VersionVector{Counters: map[uint64]uint64{1: 3}},
		},
	}

	index := ScanLocalIndex(dir, 1, prevIndex)

	entry, ok := index["deleted.agent.yaml"]
	if !ok {
		t.Fatal("expected deletion entry")
	}
	if !entry.Deleted {
		t.Error("entry should be marked as deleted")
	}
}

// ─── IsAgentCRDFile ─────────────────────────────────────────────────────────────

func TestIsAgentCRDFile(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"test.agent.yaml", true},
		{"my-agent.agent.yaml", true},
		{"test.yaml", false},
		{"test.agent.json", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsAgentCRDFile(tc.name); got != tc.want {
			t.Errorf("IsAgentCRDFile(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ─── Ordering.String ────────────────────────────────────────────────────────────

func TestOrderingString(t *testing.T) {
	cases := []struct {
		o    Ordering
		want string
	}{
		{OrderEqual, "equal"},
		{OrderGreater, "greater"},
		{OrderLesser, "lesser"},
		{OrderConcurrent, "concurrent"},
		{Ordering(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.o.String(); got != tc.want {
			t.Errorf("Ordering(%d).String() = %q, want %q", tc.o, got, tc.want)
		}
	}
}
