// cli_selfupdate_kerneltoml.go — write-ahead maintenance of .cog/conf/kernel.toml.
//
// kernel.toml is intended as the single source of truth for the pinned kernel
// binary version + per-platform sha256 checksums: an installer seeds it once and
// the daemon self-update path is expected to keep it reconciled with the running
// binary so the recorded version never drifts ahead of what is actually running
// (issue #442). This file provides the daemon side of that contract.
//
// DORMANT-BY-DESIGN until the SOT lands. As of this change the repo does NOT yet
// carry a .cog/conf/kernel.toml, an installer that writes one, or any reader of
// [kernel.checksums]; those arrive in a separate change (and, for an external
// installer, may live outside this repo). writeAheadKernelTOML therefore treats
// an ABSENT kernel.toml as a silent no-op: on every current workspace the swap
// sequence is byte-for-byte unchanged and there is no maintenance to perform.
// The maintainer only becomes active once a kernel.toml is present to reconcile.
//
// When a kernel.toml IS present, this file gives the darwin updater a
// write-ahead (ledger-first) maintainer: BEFORE downloading the new binary, the
// updater records the new [kernel].version + the current platform's sha256 into
// kernel.toml (the ledger entry the swap then reconciles toward). If any later
// step fails — checksum mismatch, smoke-test, swap, restart, or health
// verification — the prior kernel.toml bytes (and file mode) are restored so the
// recorded version never claims a binary that is not actually running.
//
// The edit is deliberately surgical (line-based) rather than a full TOML
// round-trip: the repo carries no TOML library, and a byte-preserving edit is
// the only way to keep comments, the release/checksums URL templates, and every
// OTHER platform's checksum entry intact (issue #442 migration note: "never
// blank non-current platforms"). Semantics mirror the YAML node-patch approach
// in config_write.go — touch only the keys we own, leave the rest verbatim.
// Layouts a line-based edit cannot make valid (see patchKernelTOML) return an
// error so the caller aborts the write-ahead rather than emit invalid TOML.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// kernelTOMLRel is the workspace-relative path to the pinned-version SOT.
// NOTE: this lives under .cog/conf/ (distinct from the writable kernel.yaml
// under .cog/config/); the two are different files with different owners.
const kernelTOMLRel = ".cog/conf/kernel.toml"

// kernelTOMLSnapshot captures enough state to restore kernel.toml write-ahead
// maintenance if a later apply step fails. Existed==false means there was no
// kernel.toml to maintain (a workspace that never opted into the pinned-version
// SOT); in that case the maintainer is a no-op and rollback is a no-op too.
type kernelTOMLSnapshot struct {
	path      string // absolute path, "" when no workspace root was known
	existed   bool   // whether a kernel.toml was present before the write-ahead
	prior     []byte // verbatim prior bytes (only meaningful when existed)
	priorPerm os.FileMode
}

// kernelTOMLPlatformKey returns the [kernel.checksums] key for assetName. The
// section is keyed by "<goos>-<goarch>" (e.g. "darwin-arm64"), which is the
// release asset name with the leading "cogos-" prefix and any ".exe" suffix
// stripped. Returns ("", false) when assetName is not in the expected shape.
func kernelTOMLPlatformKey(assetName string) (string, bool) {
	key := strings.TrimSuffix(assetName, ".exe")
	key, ok := strings.CutPrefix(key, "cogos-")
	if !ok || key == "" {
		return "", false
	}
	return key, true
}

// writeAheadKernelTOML records version + the current platform's sha256 into
// kernel.toml BEFORE the binary is downloaded, returning a snapshot the caller
// uses to roll the entry back on a later failure.
//
// Contract:
//   - root=="" (no known workspace) → no-op snapshot (existed=false).
//   - kernel.toml absent            → no-op snapshot (existed=false). We do NOT
//     synthesize a fresh file: a partial kernel.toml (current platform only, no
//     URL templates) would break the shell install path that reads it. A
//     workspace that never had kernel.toml stays without one.
//   - kernel.toml present           → surgically set [kernel].version and the
//     current platform's [kernel.checksums.<key>], preserving comments, URL
//     templates, and every other platform's entry; snapshot holds the prior
//     bytes for rollback.
func writeAheadKernelTOML(root, version, assetName, sum string) (kernelTOMLSnapshot, error) {
	if root == "" {
		return kernelTOMLSnapshot{}, nil
	}
	path := filepath.Join(root, kernelTOMLRel)
	snap := kernelTOMLSnapshot{path: path, priorPerm: 0o644}

	prior, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Not maintained here — leave the workspace as-is.
			return snap, nil
		}
		return snap, fmt.Errorf("read %s: %w", path, err)
	}
	if info, serr := os.Stat(path); serr == nil {
		snap.priorPerm = info.Mode().Perm()
	}
	snap.existed = true
	snap.prior = prior

	key, ok := kernelTOMLPlatformKey(assetName)
	if !ok {
		return snap, fmt.Errorf("cannot derive kernel.toml platform key from asset %q", assetName)
	}

	updated, err := patchKernelTOML(prior, version, key, sum)
	if err != nil {
		return snap, fmt.Errorf("patch %s: %w", path, err)
	}
	if err := atomicWriteConfigFile(path, updated); err != nil {
		return snap, fmt.Errorf("write %s: %w", path, err)
	}
	// atomicWriteConfigFile writes via os.CreateTemp (0o600) + rename, so restore
	// the file's prior mode — a config SOT silently narrowing from 0o644 to 0o600
	// can surprise a non-root reader (e.g. the shell install path).
	if err := os.Chmod(path, snap.priorPerm); err != nil {
		return snap, fmt.Errorf("restore mode on %s: %w", path, err)
	}
	return snap, nil
}

// rollbackKernelTOML restores the pre-write-ahead kernel.toml bytes. It is a
// no-op when the maintainer was a no-op (no root / no prior file). Returns any
// restore error so the caller can log it loudly — a failed kernel.toml rollback
// leaves the recorded version ahead of the running binary, the exact drift #442
// is closing.
func rollbackKernelTOML(snap kernelTOMLSnapshot) error {
	if !snap.existed || snap.path == "" {
		return nil
	}
	if err := atomicWriteConfigFile(snap.path, snap.prior); err != nil {
		return fmt.Errorf("restore %s: %w", snap.path, err)
	}
	// Restore the original mode too (atomicWriteConfigFile lands 0o600); rollback
	// must return the file to its exact pre-write-ahead state, mode included.
	if err := os.Chmod(snap.path, snap.priorPerm); err != nil {
		return fmt.Errorf("restore mode on %s: %w", snap.path, err)
	}
	return nil
}

// patchKernelTOML returns src with [kernel].version set to version and the
// [kernel.checksums] entry for platformKey set to sum, preserving everything
// else byte-for-byte where possible. The edit is line-based and TOML-aware only
// to the depth this file needs:
//
//   - The top-level `version = "..."` under [kernel] is replaced in place.
//   - Within [kernel.checksums], an existing `<platformKey> = "..."` line is
//     replaced in place; if absent, a new line is appended to that section.
//   - A [kernel.checksums.<platformKey>] subtable form is also recognised and
//     updated in place, so both the inline-key and subtable layouts round-trip.
//
// A missing [kernel].version or a missing [kernel.checksums] section is created
// conservatively so the write-ahead record is never silently dropped. Two
// non-canonical layouts, however, cannot be patched into valid TOML by a
// line-based edit and so return an error rather than emitting a file a strict
// parser would reject (the caller then aborts the write-ahead, leaving the
// running binary untouched):
//
//   - a bare top-level `version = "..."` defined before any [kernel] header
//     (inserting a fresh [kernel].version would duplicate the key and strand the
//     stale value), and
//   - checksums present only as [kernel.checksums.<other>] subtables with no
//     inline [kernel.checksums] header, when the current platform is absent
//     (appending an inline [kernel.checksums] table after its child subtable is
//     a parent-redefined-after-child error).
func patchKernelTOML(src []byte, version, platformKey, sum string) ([]byte, error) {
	if platformKey == "" {
		return nil, fmt.Errorf("empty platform key")
	}
	// Normalise line endings we emit to "\n"; detect a trailing newline so we
	// can preserve "no trailing newline" files faithfully.
	text := string(src)
	hadTrailingNL := strings.HasSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	// strings.Split on a trailing-NL string yields a final empty element; drop
	// it so appends land before, then re-add the newline at the end.
	if hadTrailingNL && len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	// Track the active TOML table header as we scan.
	currentTable := ""
	versionSet := false
	checksumSet := false
	checksumsSectionLine := -1       // index of the [kernel.checksums] header, if seen
	bareVersionBeforeKernel := false // top-level `version` seen before any [kernel] header
	sawChecksumsSubtable := false    // any [kernel.checksums.<x>] subtable header seen

	quoted := func(s string) string { return "\"" + s + "\"" }

	for i, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			currentTable = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			if currentTable == "kernel.checksums" {
				checksumsSectionLine = i
			}
			if strings.HasPrefix(currentTable, "kernel.checksums.") {
				sawChecksumsSubtable = true
			}
			continue
		}
		// Skip comments / blanks for key matching.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		key, ok := tomlLineKey(trimmed)
		if !ok {
			continue
		}

		switch {
		case currentTable == "" && key == "version":
			// A bare `version` at the document root (before any table header).
			// We cannot safely add a [kernel].version without duplicating the
			// key, so flag it and error out below rather than corrupt the file.
			bareVersionBeforeKernel = true
		case currentTable == "kernel" && key == "version" && !versionSet:
			lines[i] = replaceTOMLValue(raw, "version", quoted(version))
			versionSet = true
		case currentTable == "kernel.checksums" && key == platformKey && !checksumSet:
			lines[i] = replaceTOMLValue(raw, platformKey, quoted(sum))
			checksumSet = true
		case currentTable == "kernel.checksums."+platformKey && key == "sha256" && !checksumSet:
			// Subtable layout: [kernel.checksums.<key>] / sha256 = "..."
			lines[i] = replaceTOMLValue(raw, "sha256", quoted(sum))
			checksumSet = true
		}
	}

	// Refuse to patch layouts a line-based edit cannot make valid. Erroring here
	// aborts the write-ahead in the caller, leaving the running binary untouched,
	// which is strictly safer than emitting TOML a strict parser would reject.
	if !versionSet && bareVersionBeforeKernel {
		return nil, fmt.Errorf("kernel.toml has a top-level `version` before any [kernel] header; refusing to patch (would duplicate the version key)")
	}
	if !checksumSet && checksumsSectionLine < 0 && sawChecksumsSubtable {
		return nil, fmt.Errorf("kernel.toml defines checksums only as [kernel.checksums.<platform>] subtables and the current platform %q is absent; refusing to patch (appending an inline [kernel.checksums] table after its subtable is invalid TOML)", platformKey)
	}

	// If [kernel].version was never present, insert it. Create a [kernel]
	// header at the top when there isn't one.
	if !versionSet {
		lines = ensureKernelVersion(lines, version)
	}

	// If the current platform's checksum line was never present, append it to
	// the [kernel.checksums] section (creating the section when absent).
	if !checksumSet {
		lines = ensureChecksumEntry(lines, checksumsSectionLine, platformKey, quoted(sum))
	}

	out := strings.Join(lines, "\n")
	if hadTrailingNL {
		out += "\n"
	}
	return []byte(out), nil
}

// tomlLineKey returns the bare key of a `key = value` line, normalising a
// quoted key to its unquoted form so it compares equal to the platform key we
// match against. TOML permits quoted bare keys — `"darwin-arm64" = "..."` is a
// valid and common spelling for a hyphenated key — so a naive verbatim compare
// would miss the current-platform line and (wrongly) append a duplicate entry.
// Returns ok=false for non-assignment lines.
func tomlLineKey(trimmed string) (string, bool) {
	eq := strings.IndexByte(trimmed, '=')
	if eq <= 0 {
		return "", false
	}
	key := strings.TrimSpace(trimmed[:eq])
	if key == "" {
		return "", false
	}
	key = unquoteTOMLKey(key)
	if key == "" {
		return "", false
	}
	// Reject inline-table / array bracket noise that isn't a plain key.
	if strings.ContainsAny(key, "[]{}") {
		return "", false
	}
	return key, true
}

// unquoteTOMLKey strips a single matched pair of surrounding double or single
// quotes from a TOML key so `"darwin-arm64"` and `'darwin-arm64'` both normalise
// to `darwin-arm64`. A key without surrounding quotes is returned unchanged.
func unquoteTOMLKey(key string) string {
	if len(key) >= 2 {
		if (key[0] == '"' && key[len(key)-1] == '"') ||
			(key[0] == '\'' && key[len(key)-1] == '\'') {
			return key[1 : len(key)-1]
		}
	}
	return key
}

// replaceTOMLValue rewrites the value of a `<key> = <old>` line in place,
// preserving the key exactly as it was written (including any surrounding
// quotes), the leading indentation, and any trailing inline comment. The passed
// key argument is used only as a fallback; the on-line key text is authoritative
// so a quoted key round-trips as-quoted rather than being rewritten bare (which
// would leave the original quoted line behind and duplicate the entry).
func replaceTOMLValue(raw, key, newValue string) string {
	indent := raw[:len(raw)-len(strings.TrimLeft(raw, " \t"))]
	// Preserve the key text as it appears on the line (up to the '='), so a
	// quoted key stays quoted. Fall back to the bare key argument if the line
	// somehow has no '=' (it always does here, but be defensive).
	keyText := key
	if eq := strings.IndexByte(raw, '='); eq >= 0 {
		keyText = strings.TrimSpace(raw[len(indent):eq])
	}
	// Preserve a trailing inline comment (best-effort: split on the first " #").
	comment := ""
	if idx := strings.Index(raw, " #"); idx >= 0 {
		// Only treat as a comment if it appears after the '=' assignment.
		if eq := strings.IndexByte(raw, '='); eq >= 0 && idx > eq {
			comment = raw[idx:]
		}
	}
	return fmt.Sprintf("%s%s = %s%s", indent, keyText, newValue, comment)
}

// ensureKernelVersion inserts `version = "<v>"` under a [kernel] table. If a
// [kernel] header exists, the line is inserted directly after it; otherwise a
// fresh [kernel] table is prepended.
func ensureKernelVersion(lines []string, version string) []string {
	value := "version = \"" + version + "\""
	for i, raw := range lines {
		if strings.TrimSpace(raw) == "[kernel]" {
			out := make([]string, 0, len(lines)+1)
			out = append(out, lines[:i+1]...)
			out = append(out, value)
			out = append(out, lines[i+1:]...)
			return out
		}
	}
	// No [kernel] header: prepend a fresh table.
	prefix := []string{"[kernel]", value, ""}
	return append(prefix, lines...)
}

// ensureChecksumEntry appends `<key> = <quotedSum>` to the [kernel.checksums]
// section. sectionLine is the index of the section header if it was seen, else
// -1 (in which case a fresh section is appended to the end of the file).
func ensureChecksumEntry(lines []string, sectionLine int, platformKey, quotedSum string) []string {
	entry := platformKey + " = " + quotedSum
	if sectionLine < 0 {
		// Append a fresh section at end of file.
		tail := []string{"", "[kernel.checksums]", entry}
		return append(lines, tail...)
	}
	// Insert the entry at the end of the existing [kernel.checksums] block:
	// scan forward to the next table header (or EOF) and insert before it,
	// after the last non-blank line so we sit tight against the section body.
	insertAt := len(lines)
	for i := sectionLine + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			insertAt = i
			break
		}
	}
	// Back up over trailing blank lines within the section so the new entry
	// sits directly under the existing keys.
	for insertAt > sectionLine+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAt]...)
	out = append(out, entry)
	out = append(out, lines[insertAt:]...)
	return out
}
