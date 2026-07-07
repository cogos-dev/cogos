// parser_test.go — unit tests for the LM Studio conversation.json parser.
//
// The fixture JSON below is synthesized to exercise the real shapes observed
// in LM Studio's on-disk format (singleStep, multiStep with interleaved
// text/toolCallRequest/toolCallResult content, skip-worthy toolStatus and
// debugInfoBlock steps, and multi-version messages with a non-zero
// currentlySelected index) using only generic placeholder text — it is not
// derived from any real conversation content.
package lmstudio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureConversationJSON is a small synthetic LM Studio conversation with
// generic placeholder text only.
const fixtureConversationJSON = `{
  "name": "Test conversation title",
  "createdAt": 1700000000000,
  "systemPrompt": "",
  "lastUsedModel": {
    "identifier": "test-model-7b",
    "indexedModelIdentifier": "test-org/test-model-7b-GGUF/test-model-7b-Q4.gguf"
  },
  "messages": [
    {
      "versions": [
        {
          "type": "singleStep",
          "role": "user",
          "content": [
            { "type": "text", "text": "Hello, this is a test question." }
          ]
        }
      ],
      "currentlySelected": 0
    },
    {
      "versions": [
        {
          "type": "multiStep",
          "role": "assistant",
          "senderInfo": { "senderName": "test-model-7b" },
          "steps": [
            {
              "type": "contentBlock",
              "content": [
                { "type": "text", "text": "Let me look that up." },
                { "type": "toolCallRequest", "name": "example_tool", "parameters": {"query": "placeholder"} }
              ]
            },
            {
              "type": "toolStatus"
            },
            {
              "type": "contentBlock",
              "content": [
                { "type": "toolCallResult" }
              ]
            },
            {
              "type": "contentBlock",
              "content": [
                { "type": "text", "text": "Here is the answer based on the tool result." }
              ]
            },
            {
              "type": "debugInfoBlock"
            }
          ]
        }
      ],
      "currentlySelected": 0
    },
    {
      "versions": [
        {
          "type": "singleStep",
          "role": "user",
          "content": [
            { "type": "text", "text": "First draft of a follow-up question." }
          ]
        },
        {
          "type": "singleStep",
          "role": "user",
          "content": [
            { "type": "text", "text": "Edited follow-up question (this version is selected)." }
          ]
        }
      ],
      "currentlySelected": 1
    },
    {
      "versions": [
        {
          "type": "multiStep",
          "role": "assistant",
          "steps": [
            {
              "type": "contentBlock",
              "content": [
                { "type": "toolCallRequest", "name": "another_tool" }
              ]
            },
            {
              "type": "contentBlock",
              "content": [
                { "type": "toolCallResult" }
              ]
            },
            {
              "type": "status"
            }
          ]
        }
      ],
      "currentlySelected": 0
    }
  ]
}`

func writeFixture(t *testing.T, dir, folder, filename string) string {
	t.Helper()
	folderPath := filepath.Join(dir, folder)
	if err := os.MkdirAll(folderPath, 0o755); err != nil {
		t.Fatalf("mkdir fixture folder: %v", err)
	}
	path := filepath.Join(folderPath, filename)
	if err := os.WriteFile(path, []byte(fixtureConversationJSON), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestParseFile_ExtractsSingleStepAndMultiStepText(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "Test.", "1700000000000.conversation.json")

	sessionID, records, err := ParseFile(path, 0)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	wantSessionID := "Test.-1700000000000"
	if sessionID != wantSessionID {
		t.Errorf("sessionID = %q, want %q", sessionID, wantSessionID)
	}

	// 4 messages total, but message index 3 (final multiStep) is pure tool
	// calls with no prose -> dropped. Expect 3 emitted records.
	if len(records) != 3 {
		t.Fatalf("len(records) = %d, want 3; records=%+v", len(records), records)
	}

	// Record 0: singleStep user turn.
	if records[0].Role != "user" {
		t.Errorf("records[0].Role = %q, want user", records[0].Role)
	}
	if records[0].Text != "Hello, this is a test question." {
		t.Errorf("records[0].Text = %q", records[0].Text)
	}
	if records[0].TurnIndex != 0 {
		t.Errorf("records[0].TurnIndex = %d, want 0", records[0].TurnIndex)
	}

	// Record 1: multiStep assistant turn — text from two contentBlock steps
	// joined, tool call / tool result / toolStatus / debugInfoBlock skipped.
	if records[1].Role != "assistant" {
		t.Errorf("records[1].Role = %q, want assistant", records[1].Role)
	}
	wantText := "Let me look that up.\nHere is the answer based on the tool result."
	if records[1].Text != wantText {
		t.Errorf("records[1].Text = %q, want %q", records[1].Text, wantText)
	}
	if records[1].TurnIndex != 1 {
		t.Errorf("records[1].TurnIndex = %d, want 1", records[1].TurnIndex)
	}

	// Record 2: singleStep user turn — must reflect the SELECTED version
	// (index 1), not version 0 (the discarded draft).
	if records[2].Text != "Edited follow-up question (this version is selected)." {
		t.Errorf("records[2].Text = %q — version selection not honored", records[2].Text)
	}
	if records[2].TurnIndex != 2 {
		t.Errorf("records[2].TurnIndex = %d, want 2 (skipped message must not consume an index)", records[2].TurnIndex)
	}

	// Every record must carry the required ingest schema fields.
	for i, r := range records {
		if r.Schema != IngestSchema {
			t.Errorf("records[%d].Schema = %q, want %q", i, r.Schema, IngestSchema)
		}
		if r.Source != Source {
			t.Errorf("records[%d].Source = %q, want %q", i, r.Source, Source)
		}
		if r.SessionID != wantSessionID {
			t.Errorf("records[%d].SessionID = %q, want %q", i, r.SessionID, wantSessionID)
		}
		if r.Timestamp == "" {
			t.Errorf("records[%d].Timestamp is empty", i)
		}
		if _, err := time.Parse(time.RFC3339, r.Timestamp); err != nil {
			t.Errorf("records[%d].Timestamp %q not RFC3339: %v", i, r.Timestamp, err)
		}
		if r.SessionTitle != "Test conversation title" {
			t.Errorf("records[%d].SessionTitle = %q", i, r.SessionTitle)
		}
		if r.Refs == nil || r.Refs.StableID == "" {
			t.Errorf("records[%d].Refs.StableID is empty", i)
		}
	}
}

func TestParseFile_StableIDIsIdempotentAcrossReparse(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "Test.", "1700000000000.conversation.json")

	_, records1, err := ParseFile(path, 0)
	if err != nil {
		t.Fatalf("first ParseFile: %v", err)
	}
	_, records2, err := ParseFile(path, 0)
	if err != nil {
		t.Fatalf("second ParseFile: %v", err)
	}

	if len(records1) != len(records2) {
		t.Fatalf("record count changed across re-parse: %d vs %d", len(records1), len(records2))
	}
	for i := range records1 {
		if records1[i].Refs.StableID != records2[i].Refs.StableID {
			t.Errorf("record %d stable_id changed across re-parse: %q vs %q",
				i, records1[i].Refs.StableID, records2[i].Refs.StableID)
		}
	}
}

func TestParseFile_EmitsValidJSONLLines(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "Test.", "1700000000000.conversation.json")

	_, records, err := ParseFile(path, 0)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	outPath := filepath.Join(dir, "out.jsonl")
	if err := WriteJSONL(outPath, records); err != nil {
		t.Fatalf("WriteJSONL: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read emitted jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != len(records) {
		t.Fatalf("emitted %d lines, want %d", len(lines), len(records))
	}
	for i, line := range lines {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("line %d not valid JSON: %v (line=%q)", i, err, line)
		}
		for _, field := range []string{"schema", "source", "session_id", "role", "timestamp", "text"} {
			if _, ok := decoded[field]; !ok {
				t.Errorf("line %d missing required ingest field %q", i, field)
			}
		}
		if decoded["schema"] != IngestSchema {
			t.Errorf("line %d schema = %v, want %q", i, decoded["schema"], IngestSchema)
		}
	}
}

func TestSessionIDFromPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/home/u/.lmstudio/conversations/Cog./1783432270992.conversation.json", "Cog.-1783432270992"},
		{"/home/u/.lmstudio/conversations/On a plane/123.conversation.json", "On a plane-123"},
	}
	for _, c := range cases {
		if got := SessionIDFromPath(c.path); got != c.want {
			t.Errorf("SessionIDFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestExtractRecords_SkipsMessageWithNoSelectableVersion(t *testing.T) {
	conv := Conversation{
		CreatedAt: 1700000000000,
		Messages: []LSMessage{
			{
				Versions:          []LSVersion{{Type: "singleStep", Role: "user", Content: []LSContentBlock{{Type: "text", Text: "hi"}}}},
				CurrentlySelected: 5, // out of range — must be skipped, not panic
			},
			{
				Versions:          []LSVersion{{Type: "singleStep", Role: "user", Content: []LSContentBlock{{Type: "text", Text: "second message"}}}},
				CurrentlySelected: 0,
			},
		},
	}
	records := ExtractRecords(conv, "sess-x", "", 0)
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	if records[0].Text != "second message" {
		t.Errorf("records[0].Text = %q", records[0].Text)
	}
	if records[0].TurnIndex != 0 {
		t.Errorf("records[0].TurnIndex = %d, want 0", records[0].TurnIndex)
	}
}

func TestExtractRecords_UnknownRoleSkipped(t *testing.T) {
	conv := Conversation{
		CreatedAt: 1700000000000,
		Messages: []LSMessage{
			{
				Versions:          []LSVersion{{Type: "singleStep", Role: "weirdrole", Content: []LSContentBlock{{Type: "text", Text: "hi"}}}},
				CurrentlySelected: 0,
			},
		},
	}
	records := ExtractRecords(conv, "sess-x", "", 0)
	if len(records) != 0 {
		t.Fatalf("len(records) = %d, want 0 for unknown role", len(records))
	}
}

func TestExtractRecords_TruncatesLongText(t *testing.T) {
	longText := strings.Repeat("a", 100)
	conv := Conversation{
		CreatedAt: 1700000000000,
		Messages: []LSMessage{
			{
				Versions:          []LSVersion{{Type: "singleStep", Role: "user", Content: []LSContentBlock{{Type: "text", Text: longText}}}},
				CurrentlySelected: 0,
			},
		},
	}
	records := ExtractRecords(conv, "sess-x", "", 10)
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	if !strings.HasSuffix(records[0].Text, "[truncated]") {
		t.Errorf("records[0].Text = %q, want truncation marker", records[0].Text)
	}
}

func TestEpochMsToRFC3339(t *testing.T) {
	got := epochMsToRFC3339(1700000000000)
	want := time.Unix(1700000000, 0).UTC().Format(time.RFC3339)
	if got != want {
		t.Errorf("epochMsToRFC3339(1700000000000) = %q, want %q", got, want)
	}
	// Non-positive input must not produce an empty string (ingest consumer
	// rejects empty timestamps).
	if epochMsToRFC3339(0) == "" {
		t.Error("epochMsToRFC3339(0) returned empty string")
	}
}
