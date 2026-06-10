// mcp_output_cap_test.go — unit tests for the MCP tool output byte cap.
//
// Coverage:
//   - capToolOutput: truncation at the cap boundary, UTF-8 rune boundary
//     safety, no-op when within cap, marker format, full-size reported.
//   - Config.EffectiveMaxToolOutputBytes: floor enforcement, default, custom.
//   - toolReadFile: byte cap applied when file content exceeds the cap,
//     minified-one-line-blob case, truncated flag set correctly.
//   - toolGrepFiles: byte cap applied to match result JSON.
//   - cappedMarshal: cap applied to marshalResult output.
//   - Config knob respected: setting max_tool_output_bytes changes the cap.
package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── capToolOutput tests ─────────────────────────────────────────────────────

func TestCapToolOutput_NoTruncation(t *testing.T) {
	t.Parallel()
	s := "hello world"
	got, truncated := capToolOutput(s, 100)
	if truncated {
		t.Error("expected no truncation for small string")
	}
	if got != s {
		t.Errorf("got %q; want %q", got, s)
	}
}

func TestCapToolOutput_TruncatesAtCap(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("a", 1000)
	got, truncated := capToolOutput(s, 500)
	if !truncated {
		t.Error("expected truncation")
	}
	// Payload part should be exactly 500 bytes (all ASCII, so rune boundary = byte boundary).
	payload := strings.SplitN(got, "\n[output truncated:", 2)[0]
	if len(payload) != 500 {
		t.Errorf("payload len = %d; want 500", len(payload))
	}
}

func TestCapToolOutput_MarkerFormat(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("x", 1000)
	got, _ := capToolOutput(s, 100)
	if !strings.Contains(got, "[output truncated: returned 100 of 1000 bytes") {
		t.Errorf("marker missing or wrong format: %q", got)
	}
	if !strings.Contains(got, "narrow with offset/limit/filters") {
		t.Errorf("marker missing narrowing guidance: %q", got)
	}
	if !strings.Contains(got, "cog:conversations/") {
		t.Errorf("marker missing deref hint: %q", got)
	}
}

func TestCapToolOutput_UTF8Boundary(t *testing.T) {
	t.Parallel()
	// Build a string with multi-byte runes. Use the 3-byte rune U+4E16 (世).
	rune3 := "世" // 3 bytes: 0xE4, 0xB8, 0x96
	// Repeat to fill >10 bytes, then cap at a point mid-rune.
	s := strings.Repeat(rune3, 5) // 15 bytes
	for cutPoint := 1; cutPoint < len(s); cutPoint++ {
		got, _ := capToolOutput(s, cutPoint)
		// The payload (before the marker) must be valid UTF-8.
		payload := strings.SplitN(got, "\n[output truncated:", 2)[0]
		if !utf8.ValidString(payload) {
			t.Errorf("capToolOutput(s, %d): payload is not valid UTF-8", cutPoint)
		}
	}
}

func TestCapToolOutput_ZeroMaxUsesDefault(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("a", DefaultMaxToolOutputBytes+1)
	got, truncated := capToolOutput(s, 0)
	if !truncated {
		t.Error("expected truncation when maxBytes=0 and string exceeds default")
	}
	payload := strings.SplitN(got, "\n[output truncated:", 2)[0]
	if len(payload) != DefaultMaxToolOutputBytes {
		t.Errorf("payload len = %d; want %d", len(payload), DefaultMaxToolOutputBytes)
	}
}

func TestCapToolOutput_ExactlyAtCap(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("b", 100)
	got, truncated := capToolOutput(s, 100)
	if truncated {
		t.Error("expected no truncation when string is exactly at cap")
	}
	if got != s {
		t.Error("string modified when it should be unchanged")
	}
}

// ─── Config.EffectiveMaxToolOutputBytes tests ────────────────────────────────

func TestEffectiveMaxToolOutputBytes_Default(t *testing.T) {
	t.Parallel()
	cfg := &Config{}
	got := cfg.EffectiveMaxToolOutputBytes()
	if got != DefaultMaxToolOutputBytes {
		t.Errorf("got %d; want %d (default)", got, DefaultMaxToolOutputBytes)
	}
}

func TestEffectiveMaxToolOutputBytes_CustomValue(t *testing.T) {
	t.Parallel()
	cfg := &Config{MaxToolOutputBytes: 65536}
	got := cfg.EffectiveMaxToolOutputBytes()
	if got != 65536 {
		t.Errorf("got %d; want 65536", got)
	}
}

func TestEffectiveMaxToolOutputBytes_FloorEnforced(t *testing.T) {
	t.Parallel()
	cfg := &Config{MaxToolOutputBytes: 100} // below MinToolOutputBytes
	got := cfg.EffectiveMaxToolOutputBytes()
	if got != MinToolOutputBytes {
		t.Errorf("got %d; want %d (floor)", got, MinToolOutputBytes)
	}
}

func TestEffectiveMaxToolOutputBytes_NilConfig(t *testing.T) {
	t.Parallel()
	var cfg *Config
	got := cfg.EffectiveMaxToolOutputBytes()
	if got != DefaultMaxToolOutputBytes {
		t.Errorf("got %d; want %d (nil config default)", got, DefaultMaxToolOutputBytes)
	}
}

// ─── toolReadFile byte-cap tests ─────────────────────────────────────────────

func TestToolReadFile_ByteCapApplied(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	// Set a very small cap so we can verify truncation with a small test file.
	cfg.MaxToolOutputBytes = MinToolOutputBytes // 4 KiB
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	// Write a file large enough to exceed the 4 KiB cap.
	// Each line is "   N\t<80 chars>\n" ≈ 90 bytes. 50 lines ≈ 4.5 KiB > 4 KiB.
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, strings.Repeat("x", 80))
	}
	path := filepath.Join(root, "big.txt")
	writeTestFile(t, path, strings.Join(lines, "\n")+"\n")

	result, _, err := server.toolReadFile(context.Background(), nil, readFileInput{Path: path})
	if err != nil {
		t.Fatalf("toolReadFile: %v", err)
	}

	var decoded map[string]any
	decodeMCPJSON(t, result, &decoded)

	// The output should be truncated.
	content, _ := decoded["content"].(string)
	if !strings.Contains(content, "[output truncated:") {
		peek := content
		if len(peek) > 200 {
			peek = peek[:200]
		}
		t.Errorf("expected truncation marker in content; got %q", peek)
	}
	truncated, _ := decoded["truncated"].(bool)
	if !truncated {
		t.Error("expected truncated=true in result")
	}
}

func TestToolReadFile_MinifiedOneLineBlobCapped(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	// 32 KiB cap. Single minified line of 5,000,009 bytes — strictly larger
	// than ANY internal read buffer (mirrors the /tmp/caps-sandbox/blob.json
	// e2e fixture). This is the exact case that previously returned
	// "bufio.Scanner: token too long" instead of capped content.
	cfg.MaxToolOutputBytes = DefaultMaxToolOutputBytes
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	bigLine := strings.Repeat("A", 5_000_009)
	path := filepath.Join(root, "minified.js")
	writeTestFile(t, path, bigLine)

	result, _, err := server.toolReadFile(context.Background(), nil, readFileInput{Path: path})
	if err != nil {
		t.Fatalf("toolReadFile on minified blob: %v", err)
	}

	text := result.Content[0].(*mcp.TextContent).Text
	// MUST be capped content, never a scanner error.
	if strings.Contains(text, "token too long") {
		t.Fatalf("scanner cliff still present: %q", text)
	}
	// The full JSON response must not exceed a reasonable multiple of the cap.
	if len(text) > 2*DefaultMaxToolOutputBytes {
		t.Errorf("response too large: %d bytes (cap=%d); minified-blob not capped",
			len(text), DefaultMaxToolOutputBytes)
	}
	var decoded map[string]any
	decodeMCPJSON(t, result, &decoded)
	truncated, _ := decoded["truncated"].(bool)
	if !truncated {
		t.Error("expected truncated=true for 5 MB minified blob with 32 KiB cap")
	}
	content, _ := decoded["content"].(string)
	if !strings.Contains(content, "[output truncated:") {
		t.Error("expected explicit truncation marker in content")
	}
	// file_size_bytes reports the true source size (the marker's Y counts
	// the rendered buffer, which stops at cap+ε by design).
	if size, ok := decoded["file_size_bytes"].(float64); !ok || int64(size) != 5_000_009 {
		t.Errorf("file_size_bytes = %v; want 5000009", decoded["file_size_bytes"])
	}
	// The mid-line cut must be flagged so the agent knows line numbering
	// beyond the truncation point was not scanned.
	if note, _ := decoded["note"].(string); !strings.Contains(note, "cut mid-line") {
		t.Errorf("note = %q; want mid-line cut explanation", note)
	}
}

func TestToolReadFile_OffsetSkipsOverLongLines(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	// Line 1 is a 300 KiB blob (over the 64 KiB read buffer); line 2 is
	// short. offset=1 must skip the blob without erroring and return line 2
	// with the correct line number.
	contents := strings.Repeat("z", 300*1024) + "\n" + "short line two\n"
	path := filepath.Join(root, "mixed.txt")
	writeTestFile(t, path, contents)

	result, _, err := server.toolReadFile(context.Background(), nil, readFileInput{Path: path, Offset: 1})
	if err != nil {
		t.Fatalf("toolReadFile with offset over long line: %v", err)
	}
	var decoded map[string]any
	decodeMCPJSON(t, result, &decoded)
	content, _ := decoded["content"].(string)
	if !strings.Contains(content, "   2\tshort line two") {
		t.Errorf("expected line 2 with correct numbering; got %q", content)
	}
}

func TestToolReadFile_RelativePathResolvesAgainstWorkspaceRoot(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	writeTestFile(t, filepath.Join(root, "rel.txt"), "relative content\n")

	result, _, err := server.toolReadFile(context.Background(), nil, readFileInput{Path: "rel.txt"})
	if err != nil {
		t.Fatalf("toolReadFile relative path: %v", err)
	}
	var decoded map[string]any
	decodeMCPJSON(t, result, &decoded)
	content, _ := decoded["content"].(string)
	if !strings.Contains(content, "relative content") {
		t.Errorf("relative path did not resolve against workspace root; got %q", content)
	}
}

// ─── readLineCapped tests ────────────────────────────────────────────────────

func TestReadLineCapped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		input    string
		keep     int
		wantLine string
		wantOver bool
	}{
		{name: "short line", input: "hello\n", keep: 100, wantLine: "hello"},
		{name: "exact keep", input: "abcde\n", keep: 5, wantLine: "abcde"},
		{name: "over keep", input: "abcdef\n", keep: 5, wantLine: "abcde", wantOver: true},
		{name: "no trailing newline", input: "tail", keep: 100, wantLine: "tail"},
		{name: "empty line", input: "\n", keep: 100, wantLine: ""},
		{name: "keep zero skip", input: "anything here\n", keep: 0, wantLine: "", wantOver: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := bufio.NewReaderSize(strings.NewReader(tc.input), 16)
			line, over, err := readLineCapped(r, tc.keep, true)
			if err != nil {
				t.Fatalf("readLineCapped: %v", err)
			}
			if string(line) != tc.wantLine {
				t.Errorf("line = %q; want %q", line, tc.wantLine)
			}
			if over != tc.wantOver {
				t.Errorf("overflow = %v; want %v", over, tc.wantOver)
			}
		})
	}
}

func TestReadLineCapped_LongLineSmallBuffer(t *testing.T) {
	t.Parallel()
	// A 1 MB line through a 64-byte buffer: must never error, retain keep
	// bytes, and (drain=true) leave the reader at the start of the next line.
	long := strings.Repeat("x", 1024*1024)
	r := bufio.NewReaderSize(strings.NewReader(long+"\nnext\n"), 64)

	line, over, err := readLineCapped(r, 100, true)
	if err != nil {
		t.Fatalf("readLineCapped on 1MB line: %v", err)
	}
	if len(line) != 100 || !over {
		t.Errorf("len=%d over=%v; want 100/true", len(line), over)
	}

	next, over2, err := readLineCapped(r, 100, true)
	if err != nil {
		t.Fatalf("second line: %v", err)
	}
	if string(next) != "next" || over2 {
		t.Errorf("next = %q over=%v; want \"next\"/false", next, over2)
	}

	if _, _, err := readLineCapped(r, 100, true); err != io.EOF {
		t.Errorf("expected io.EOF at end, got %v", err)
	}
}

func TestReadLineCapped_NoDrainStopsEarly(t *testing.T) {
	t.Parallel()
	// drain=false: after retaining keep bytes the rest of the line stays
	// unread — cog_read_file's guarantee that at most cap+ε is read from disk.
	long := strings.Repeat("y", 1024*1024)
	sr := strings.NewReader(long)
	r := bufio.NewReaderSize(sr, 64)

	line, over, err := readLineCapped(r, 100, false)
	if err != nil {
		t.Fatalf("readLineCapped: %v", err)
	}
	if len(line) != 100 || !over {
		t.Errorf("len=%d over=%v; want 100/true", len(line), over)
	}
	// Almost all of the 1 MB must remain unread in the source.
	if sr.Len() < 1024*1024-1024 {
		t.Errorf("reader consumed too much: %d bytes remain (want ~1MB)", sr.Len())
	}
}

func TestToolGrepFiles_LongLineDoesNotAbortScan(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	// One file: 200 KiB first line (over the old 64 KiB scanner limit),
	// then a matching short line AFTER it. The old scanner silently stopped
	// at the long line and missed the later match.
	path := filepath.Join(root, "longline.txt")
	writeTestFile(t, path, strings.Repeat("f", 200*1024)+"\nNEEDLE here\n")

	result, _, err := server.toolGrepFiles(context.Background(), nil, grepFilesInput{
		Pattern: "NEEDLE",
		Path:    root,
	})
	if err != nil {
		t.Fatalf("toolGrepFiles: %v", err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "NEEDLE here") {
		t.Errorf("match after long line was missed: %s", text)
	}
}

func TestToolReadFile_SmallFileNotCapped(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	path := filepath.Join(root, "small.go")
	writeTestFile(t, path, "package engine\n\nfunc Foo() {}\n")

	result, _, err := server.toolReadFile(context.Background(), nil, readFileInput{Path: path})
	if err != nil {
		t.Fatalf("toolReadFile: %v", err)
	}

	var decoded map[string]any
	decodeMCPJSON(t, result, &decoded)
	content, _ := decoded["content"].(string)
	if strings.Contains(content, "[output truncated:") {
		t.Error("small file should not have truncation marker")
	}
}

// ─── toolGrepFiles byte-cap test ─────────────────────────────────────────────

func TestToolGrepFiles_ByteCapApplied(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.MaxToolOutputBytes = MinToolOutputBytes // 4 KiB cap
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	// Write many files each with a matching long line so JSON output bloats.
	for i := 0; i < 30; i++ {
		path := filepath.Join(root, fmt.Sprintf("file%02d.txt", i))
		writeTestFile(t, path, "MATCH: "+strings.Repeat("y", 300)+"\n")
	}

	result, _, err := server.toolGrepFiles(context.Background(), nil, grepFilesInput{
		Pattern:    "MATCH",
		Path:       root,
		MaxResults: 100,
	})
	if err != nil {
		t.Fatalf("toolGrepFiles: %v", err)
	}

	text := result.Content[0].(*mcp.TextContent).Text
	if len(text) > 2*MinToolOutputBytes+500 { // allow some marker overhead
		t.Errorf("grep output too large: %d bytes (cap=%d)", len(text), MinToolOutputBytes)
	}
	if !strings.Contains(text, "[output truncated:") {
		t.Errorf("expected truncation marker in grep output")
	}
}

// ─── Config knob integration test ────────────────────────────────────────────

func TestConfigKnob_MaxToolOutputBytes(t *testing.T) {
	t.Parallel()
	// Verify that a config with max_tool_output_bytes set changes the effective cap.
	cfg := &Config{MaxToolOutputBytes: 8192}
	if got := cfg.EffectiveMaxToolOutputBytes(); got != 8192 {
		t.Errorf("EffectiveMaxToolOutputBytes = %d; want 8192", got)
	}
	// Verify floor: value below MinToolOutputBytes is clamped.
	cfg2 := &Config{MaxToolOutputBytes: 1024}
	if got := cfg2.EffectiveMaxToolOutputBytes(); got != MinToolOutputBytes {
		t.Errorf("EffectiveMaxToolOutputBytes = %d; want %d (floor)", got, MinToolOutputBytes)
	}
}

// ─── cappedMarshal tests ──────────────────────────────────────────────────────

func TestCappedMarshal_AppliesCap(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	// Use MinToolOutputBytes (4 KiB) so the floor doesn't kick in;
	// our test payload of ~6 KiB JSON exceeds this cap.
	cfg.MaxToolOutputBytes = MinToolOutputBytes
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	// Build data whose JSON exceeds MinToolOutputBytes (4 KiB).
	// JSON: {"content":"zzz...5000z"} ≈ 5014 bytes > 4096.
	data := map[string]any{
		"content": strings.Repeat("z", 5000),
	}
	result, _, err := server.cappedMarshal(data)
	if err != nil {
		t.Fatalf("cappedMarshal: %v", err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "[output truncated:") {
		peek := text
		if len(peek) > 200 {
			peek = peek[:200]
		}
		t.Errorf("expected truncation marker, got: %q", peek)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// Ensure imports are used.
var _ = os.Stat
var _ = mcp.NewServer

func TestCapToolOutputWithTotal_TrueDenominator(t *testing.T) {
	// A bounded reader hands us only ~cap bytes of a much larger source; the
	// marker must report the true source size, not the bytes read.
	s := strings.Repeat("A", 40_000)
	out, truncated := capToolOutputWithTotal(s, 32768, 5_000_009)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if !strings.Contains(out, "of 5000009 bytes") {
		t.Fatalf("marker denominator should be true total; got marker tail: %q", out[len(out)-160:])
	}
}
