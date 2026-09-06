// build_tags.go — runtime feature probe for the kernel's compile-time build tags.
//
// WHY THIS EXISTS
//
// On 2026-09-06 the kernel serving this workspace had been running for an
// unknown period built WITHOUT `-tags fts5`. Every `/memory/search` silently
// degraded to an unranked linear grep over 41k files: every hit scored 0, the
// result set was capped at whatever the scan reached, multi-term queries
// returned zero, and a repeat probe took 30s. Nothing surfaced the condition.
// Prior-art checks — "does the corpus already contain this?" — returned "no"
// when the honest answer was "the index is not there".
//
// A silent retrieval failure is worse than a loud one: it is indistinguishable
// from a true negative.
//
// WHY A RUNTIME PROBE AND NOT THE LDFLAGS CLAIM
//
// The build can *say* fts5 three different ways and still be wrong:
//
//   - `-tags fts5` without CGO_ENABLED=1 compiles, links the pure-Go
//     go-sqlite3 stub, and the FTS5 module is absent at runtime.
//   - A caller can pass `-ldflags "-X ...BuildTags=fts5"` on a binary built
//     without the tag. Self-reported provenance is not evidence.
//   - Counting `fts5` symbols with `strings` proves the string is in the
//     binary, not that sqlite will accept `USING fts5`. The broken binary
//     above carried exactly one stray `fts5` symbol.
//
// So this probe does the only thing that actually settles the question: it
// asks SQLite, at runtime, to create an FTS5 virtual table in an in-memory
// database. If that DDL succeeds the module is loaded and usable. If it fails
// the module is not there, whatever the build flags claimed.
//
// The probe runs once (sync.Once) against `:memory:` and is cheap — a
// connection open, one CREATE VIRTUAL TABLE, one close.
package engine

import (
	"database/sql"
	"strings"
	"sync"
)

var (
	fts5ProbeOnce  sync.Once
	fts5Available  bool
	fts5ProbeError string
)

// DeclaredBuildTags is the build-tag list the linker was TOLD to use, injected
// via -ldflags "-X ...engine.DeclaredBuildTags=fts5" by the Makefile. It is a
// claim, not evidence: it is reported alongside the runtime probe purely so a
// disagreement between the two becomes visible. Never gate on this alone.
var DeclaredBuildTags = ""

// FTS5Available reports whether the SQLite driver compiled into THIS binary
// actually provides the FTS5 module, determined by executing FTS5 DDL rather
// than by trusting build flags. The result is computed once and cached.
func FTS5Available() bool {
	fts5ProbeOnce.Do(func() {
		fts5Available, fts5ProbeError = probeFTS5()
	})
	return fts5Available
}

// FTS5ProbeError returns the error text from the runtime probe, or "" when the
// probe succeeded. Surfaced on /health so an operator sees WHY the module is
// missing instead of only that it is.
func FTS5ProbeError() string {
	FTS5Available()
	return fts5ProbeError
}

// probeFTS5 opens a scratch in-memory database and attempts to create an FTS5
// virtual table. Any error at any step means FTS5 is unusable.
func probeFTS5() (bool, string) {
	return probeFTS5At(":memory:")
}

// probeFTS5At is probeFTS5 parameterised by DSN so tests can drive the failure
// path with an unopenable database. A probe that cannot be made to return
// false is not a probe.
func probeFTS5At(dsn string) (bool, string) {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return false, "sql.Open: " + err.Error()
	}
	defer func() { _ = db.Close() }()

	// sql.Open is lazy; force a real connection so a broken driver fails here
	// rather than masquerading as a DDL error below.
	if err := db.Ping(); err != nil {
		return false, "ping: " + err.Error()
	}

	if _, err := db.Exec(`CREATE VIRTUAL TABLE fts5_probe USING fts5(content)`); err != nil {
		return false, "CREATE VIRTUAL TABLE ... USING fts5: " + err.Error()
	}
	return true, ""
}

// BuildTagReport is the /health "build_tags" object.
//
// The report deliberately carries BOTH the declared claim and the probed
// result so they can be compared. Consumers must gate on FTS5 (the probe).
type BuildTagReport struct {
	// FTS5 is true iff CREATE VIRTUAL TABLE ... USING fts5 succeeded at
	// runtime in this process. When false, memory search falls back to an
	// unranked scan and prior-art results must not be trusted as negatives.
	FTS5 bool `json:"fts5"`

	// FTS5Error is the probe's failure reason, empty when FTS5 is true.
	FTS5Error string `json:"fts5_error,omitempty"`

	// Declared echoes the -ldflags build-tag claim, for comparison only.
	Declared string `json:"declared"`

	// Mismatch is true when the build claimed fts5 but the probe disproved
	// it — i.e. the exact 2026-09-06 failure, made loud.
	Mismatch bool `json:"mismatch"`
}

// buildTags returns the probed compile-time feature report for /health.
func buildTags() BuildTagReport {
	probed := FTS5Available()
	declared := DeclaredBuildTags
	return BuildTagReport{
		FTS5:      probed,
		FTS5Error: FTS5ProbeError(),
		Declared:  declared,
		Mismatch:  strings.Contains(declared, "fts5") && !probed,
	}
}
