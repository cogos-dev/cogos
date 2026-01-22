// Package fs provides filesystem primitives for the SDK.
//
// This package implements atomic file operations and other filesystem
// utilities needed by the kernel.
package fs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// WriteAtomic writes data to a file atomically using temp file + rename.
// This prevents partial writes from corrupting state files.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	// Ensure directory exists
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir failed: %w", err)
	}

	// Write to temp file
	tmpPath := path + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	if err := os.WriteFile(tmpPath, data, perm); err != nil {
		return fmt.Errorf("write temp failed: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // Clean up temp file
		return fmt.Errorf("rename failed: %w", err)
	}

	return nil
}

// ReadJSON reads and unmarshals a JSON file.
func ReadJSON[T any](path string) (T, error) {
	var result T
	data, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, fmt.Errorf("json unmarshal failed: %w", err)
	}
	return result, nil
}

// WriteJSON marshals and writes data as JSON atomically.
func WriteJSON(path string, data any, perm os.FileMode) error {
	content, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("json marshal failed: %w", err)
	}
	return WriteAtomic(path, content, perm)
}

// ReadYAML reads and unmarshals a YAML file.
func ReadYAML[T any](path string) (T, error) {
	var result T
	data, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	if err := yaml.Unmarshal(data, &result); err != nil {
		return result, fmt.Errorf("yaml unmarshal failed: %w", err)
	}
	return result, nil
}

// WriteYAML marshals and writes data as YAML atomically.
func WriteYAML(path string, data any, perm os.FileMode) error {
	content, err := yaml.Marshal(data)
	if err != nil {
		return fmt.Errorf("yaml marshal failed: %w", err)
	}
	return WriteAtomic(path, content, perm)
}

// Exists checks if a file or directory exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// IsDir checks if path is a directory.
func IsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// IsFile checks if path is a regular file.
func IsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// EnsureDir creates directory and all parents if they don't exist.
func EnsureDir(path string) error {
	return os.MkdirAll(path, 0755)
}

// AppendLine appends a line to a file (useful for JSONL).
// Creates the file if it doesn't exist.
func AppendLine(path string, line []byte) error {
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	// Ensure line ends with newline
	if len(line) == 0 || line[len(line)-1] != '\n' {
		line = append(line, '\n')
	}

	_, err = f.Write(line)
	return err
}
