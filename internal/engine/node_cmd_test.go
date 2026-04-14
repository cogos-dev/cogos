package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNodeSync_syncJSON_DetectsDriftAndFixes(t *testing.T) {
	t.Parallel()

	// Create a temp JSON file with a wrong port value.
	dir := t.TempDir()
	jsonFile := filepath.Join(dir, "config.json")

	original := map[string]any{
		"mcpServers": map[string]any{
			"cogos": map[string]any{
				"url": "http://localhost:9999/mcp",
			},
		},
	}
	writeJSON(t, jsonFile, original)

	consumer := ConsumerEntry{
		Path:     jsonFile,
		Type:     "json",
		JSONPath: ".mcpServers.cogos.url",
		Template: "http://localhost:{{port}}/mcp",
	}

	// Dry run should detect drift.
	result := syncJSON(jsonFile, consumer, 6931, false)
	if result.state != syncDrift {
		t.Fatalf("dry run: state = %v; want syncDrift", result.state)
	}

	// Apply should fix drift.
	result = syncJSON(jsonFile, consumer, 6931, true)
	if result.state != syncDrift {
		t.Fatalf("apply: state = %v; want syncDrift (indicating fix applied)", result.state)
	}

	// Re-read and verify the value is corrected.
	data, err := os.ReadFile(jsonFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var updated map[string]any
	if err := json.Unmarshal(data, &updated); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	val, err := jsonGet(updated, []string{"mcpServers", "cogos", "url"})
	if err != nil {
		t.Fatalf("jsonGet: %v", err)
	}
	if val != "http://localhost:6931/mcp" {
		t.Errorf("url = %v; want http://localhost:6931/mcp", val)
	}
}

func TestNodeSync_syncJSON_ReportsOKWhenInSync(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	jsonFile := filepath.Join(dir, "config.json")

	inSync := map[string]any{
		"mcpServers": map[string]any{
			"cogos": map[string]any{
				"url": "http://localhost:6931/mcp",
			},
		},
	}
	writeJSON(t, jsonFile, inSync)

	consumer := ConsumerEntry{
		Path:     jsonFile,
		Type:     "json",
		JSONPath: ".mcpServers.cogos.url",
		Template: "http://localhost:{{port}}/mcp",
	}

	result := syncJSON(jsonFile, consumer, 6931, false)
	if result.state != syncOK {
		t.Errorf("state = %v; want syncOK", result.state)
	}
}

func TestNodeSync_syncSed_ReplacesMatchedPatterns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sedFile := filepath.Join(dir, "launch.sh")

	content := "exec cogos serve --port 9999\nexec other --port 9999\n"
	if err := os.WriteFile(sedFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	consumer := ConsumerEntry{
		Path:    sedFile,
		Type:    "sed",
		Match:   "--port 9999",
		Replace: "--port {{port}}",
	}

	// Dry run detects drift.
	result := syncSed(sedFile, consumer, 6931, false)
	if result.state != syncDrift {
		t.Fatalf("dry run: state = %v; want syncDrift", result.state)
	}

	// Apply replaces.
	result = syncSed(sedFile, consumer, 6931, true)
	if result.state != syncDrift {
		t.Fatalf("apply: state = %v; want syncDrift (indicating replacement done)", result.state)
	}

	data, err := os.ReadFile(sedFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	want := "exec cogos serve --port 6931\nexec other --port 6931\n"
	if got != want {
		t.Errorf("content = %q; want %q", got, want)
	}
}

func TestNodeSync_parseJSONPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  []string
	}{
		{".foo.bar.baz", []string{"foo", "bar", "baz"}},
		{".mcpServers.cogos.url", []string{"mcpServers", "cogos", "url"}},
		{".args[3]", []string{"args", "3"}},
		{".servers[0].url", []string{"servers", "0", "url"}},
		{"single", []string{"single"}},
	}

	for _, tt := range tests {
		got := parseJSONPath(tt.input)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseJSONPath(%q) = %v; want %v", tt.input, got, tt.want)
		}
	}
}

func TestNodeSync_jsonGetSet(t *testing.T) {
	t.Parallel()

	root := map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"value": "old",
			},
		},
		"list": []any{"a", "b", "c"},
	}

	// jsonGet — nested map
	val, err := jsonGet(root, []string{"level1", "level2", "value"})
	if err != nil {
		t.Fatalf("jsonGet nested: %v", err)
	}
	if val != "old" {
		t.Errorf("jsonGet nested = %v; want \"old\"", val)
	}

	// jsonGet — array index
	val, err = jsonGet(root, []string{"list", "1"})
	if err != nil {
		t.Fatalf("jsonGet array: %v", err)
	}
	if val != "b" {
		t.Errorf("jsonGet array = %v; want \"b\"", val)
	}

	// jsonGet — missing key
	_, err = jsonGet(root, []string{"level1", "nonexistent"})
	if err == nil {
		t.Error("jsonGet missing key should return error")
	}

	// jsonGet — array out of range
	_, err = jsonGet(root, []string{"list", "99"})
	if err == nil {
		t.Error("jsonGet out-of-range index should return error")
	}

	// jsonSet — nested map
	updated, err := jsonSet(root, []string{"level1", "level2", "value"}, "new")
	if err != nil {
		t.Fatalf("jsonSet nested: %v", err)
	}
	val, _ = jsonGet(updated, []string{"level1", "level2", "value"})
	if val != "new" {
		t.Errorf("jsonSet nested result = %v; want \"new\"", val)
	}

	// jsonSet — array index
	updated, err = jsonSet(root, []string{"list", "0"}, "replaced")
	if err != nil {
		t.Fatalf("jsonSet array: %v", err)
	}
	val, _ = jsonGet(updated, []string{"list", "0"})
	if val != "replaced" {
		t.Errorf("jsonSet array result = %v; want \"replaced\"", val)
	}

	// jsonSet — empty keys returns value directly
	updated, err = jsonSet("anything", []string{}, "replacement")
	if err != nil {
		t.Fatalf("jsonSet empty keys: %v", err)
	}
	if updated != "replacement" {
		t.Errorf("jsonSet empty keys = %v; want \"replacement\"", updated)
	}
}

// writeJSON marshals v to the given path as indented JSON.
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}
