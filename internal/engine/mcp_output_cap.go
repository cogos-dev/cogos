// mcp_output_cap.go — byte-level output capping for MCP tool responses.
//
// Problem (2026-06-04 diagnosis): cog_* MCP tool responses are capped only
// by item/line counts, not by byte size. A single 500-line read of a minified
// JS bundle or a large JSONL ledger can return multiple megabytes, bloating
// agent context windows beyond recovery.
//
// Fix: capToolOutput trims any string to maxBytes at a UTF-8 rune boundary
// and appends an explicit truncation marker so the agent knows it received
// partial output and how to obtain the rest.
//
// Cap defaults by tool class:
//
//	Read-file / grep / cogdoc class  — DefaultReadToolOutputBytes  (32 KiB)
//	List / search / event class      — DefaultListToolOutputBytes  (32 KiB)
//
// Both classes share the same DefaultMaxToolOutputBytes default; a single
// config knob (max_tool_output_bytes in kernel.yaml) adjusts both.
// The floor is MinToolOutputBytes (4 KiB) — absurdly low values produce
// truncation so aggressive the tool becomes useless.
//
// Usage:
//
//	s, truncated := capToolOutput(s, m.cfg.EffectiveMaxToolOutputBytes())
package engine

import (
	"bufio"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// DefaultMaxToolOutputBytes is the default byte cap applied to all
	// cog_* MCP tool text outputs. 32 KiB is large enough to carry any
	// reasonable single-tool response while preventing megabyte blowout.
	DefaultMaxToolOutputBytes = 32 * 1024 // 32 KiB

	// MinToolOutputBytes is the hard floor for max_tool_output_bytes.
	// Below this the cap becomes so aggressive that tools are useless.
	MinToolOutputBytes = 4 * 1024 // 4 KiB

	// grepLineKeep is the per-line retention budget for cog_grep_files.
	// Matches the old bufio.Scanner default token size so match coverage is
	// unchanged for lines that previously worked, while lines beyond it are
	// now truncated-and-drained instead of silently aborting the scan.
	grepLineKeep = 64 * 1024 // 64 KiB
)

// capToolOutput truncates s to at most maxBytes, cutting at the last valid
// UTF-8 rune boundary at or before the limit. When truncation occurs it
// appends a marker that states:
//   - the total byte length of the original string
//   - how many bytes were returned
//   - guidance on how to narrow the call
//
// The marker itself is never counted against maxBytes, so the caller always
// receives exactly min(len(s), maxBytes) bytes of payload plus the marker.
//
// Returns (possibly truncated string, true) when truncation occurred; returns
// (s, false) when s fits within maxBytes.
func capToolOutput(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxToolOutputBytes
	}
	if len(s) <= maxBytes {
		return s, false
	}

	// Walk back from maxBytes until we're on a valid UTF-8 boundary.
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	marker := fmt.Sprintf(
		"\n[output truncated: returned %d of %d bytes; narrow with offset/limit/filters, or dereference cog:conversations/... where applicable]",
		cut, len(s),
	)
	return s[:cut] + marker, true
}

// EffectiveMaxToolOutputBytes returns the configured byte cap, applying the
// hard floor so it can never be set absurdly low.
func (c *Config) EffectiveMaxToolOutputBytes() int {
	if c == nil {
		return DefaultMaxToolOutputBytes
	}
	v := c.MaxToolOutputBytes
	if v <= 0 {
		return DefaultMaxToolOutputBytes
	}
	if v < MinToolOutputBytes {
		return MinToolOutputBytes
	}
	return v
}

// capMarshalResult marshals data to JSON, then caps the resulting string at
// maxBytes. It is the drop-in replacement for marshalResult in handlers where
// we want byte-level output control.
func capMarshalResult(data any, maxBytes int) (*mcp.CallToolResult, any, error) {
	r, metadata, err := marshalResult(data)
	if err != nil || r == nil {
		return r, metadata, err
	}
	if len(r.Content) == 0 {
		return r, metadata, nil
	}
	tc, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		return r, metadata, nil
	}
	capped, _ := capToolOutput(tc.Text, maxBytes)
	tc.Text = capped
	return r, metadata, nil
}

// capResult applies the configured byte cap to the already-built CallToolResult.
// Returns r unmodified when r is nil or has no text content.
func capResult(r *mcp.CallToolResult, metadata any, err error, maxBytes int) (*mcp.CallToolResult, any, error) {
	if err != nil || r == nil || len(r.Content) == 0 {
		return r, metadata, err
	}
	tc, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		return r, metadata, err
	}
	capped, _ := capToolOutput(tc.Text, maxBytes)
	tc.Text = capped
	return r, metadata, nil
}

// cappedMarshal is a convenience method on MCPServer that calls marshalResult
// and applies the configured byte cap. Use this in all handlers that produce
// potentially large textual output.
func (m *MCPServer) cappedMarshal(data any) (*mcp.CallToolResult, any, error) {
	maxBytes := DefaultMaxToolOutputBytes
	if m.cfg != nil {
		maxBytes = m.cfg.EffectiveMaxToolOutputBytes()
	}
	return capMarshalResult(data, maxBytes)
}

// MaxToolOutputBytes returns the effective byte cap for MCP tool outputs.
// Exposed so extension-registration callsites (e.g. z_conversations_mcp.go)
// can forward the cap to external RegisterXxx helpers.
func (m *MCPServer) MaxToolOutputBytes() int {
	if m.cfg == nil {
		return DefaultMaxToolOutputBytes
	}
	return m.cfg.EffectiveMaxToolOutputBytes()
}

// readLineCapped reads one line from r (terminated by '\n' or EOF), retaining
// at most keep bytes of line content (excluding the trailing newline). Unlike
// bufio.Scanner, it can NEVER fail because a line is longer than a buffer —
// over-long lines are truncated, not errored. This is the fix for the
// "bufio.Scanner: token too long" cliff on minified one-line blobs.
//
// When drain is true the remainder of an over-long line is consumed so the
// reader is positioned at the start of the next line (needed when the caller
// wants subsequent lines, e.g. grep output parsing). When drain is false the
// function returns as soon as keep bytes have been retained, leaving the rest
// of the line unread — cog_read_file uses this so it never reads more than
// ~maxBytes+ε from disk regardless of file size.
//
// Returns:
//
//	line     — up to keep bytes of line content, no trailing '\n'
//	overflow — true when the line content was longer than keep
//	err      — io.EOF only when no bytes remained at the start of the call;
//	           any other error is a real read failure.
func readLineCapped(r *bufio.Reader, keep int, drain bool) (line []byte, overflow bool, err error) {
	first := true
	for {
		chunk, rerr := r.ReadSlice('\n')
		if len(chunk) == 0 && rerr == io.EOF {
			if first {
				return nil, false, io.EOF
			}
			// EOF after accumulating a final unterminated line.
			return line, overflow, nil
		}
		first = false

		content := chunk
		complete := rerr == nil // found the '\n'
		if complete {
			content = content[:len(content)-1] // strip '\n'
		}

		if len(line) < keep {
			take := content
			if len(line)+len(take) > keep {
				take = take[:keep-len(line)]
				overflow = true
			}
			// append copies — chunk is only valid until the next ReadSlice.
			line = append(line, take...)
		} else if len(content) > 0 {
			overflow = true
		}

		switch {
		case complete:
			return line, overflow, nil
		case rerr == bufio.ErrBufferFull:
			if overflow && !drain {
				// Caller doesn't need the rest of this line; stop reading.
				return line, overflow, nil
			}
			continue
		case rerr == io.EOF:
			return line, overflow, nil
		default:
			return line, overflow, rerr
		}
	}
}
