package server

import (
	"errors"
	"os"
	"path/filepath"
)

// ReadFile reads the entire content of a file
func ReadFile(path string) (string, error) {
	// Clean the path to prevent directory traversal
	cleanPath := filepath.Clean(path)

	// Check if file exists
	if _, err := os.Stat(cleanPath); os.IsNotExist(err) {
		return "", errors.New("file does not exist")
	}

	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

// WriteFile writes content to a file
func WriteFile(path, content string) error {
	// Clean the path
	cleanPath := filepath.Clean(path)

	// Create directory if it doesn't exist
	dir := filepath.Dir(cleanPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return os.WriteFile(cleanPath, []byte(content), 0644)
}

// ApplyDelta applies a text delta to content at a specific position
func ApplyDelta(content string, fromPos, toPos int, insert string) (string, error) {
	if fromPos < 0 || toPos > len(content) || fromPos > toPos {
		return "", errors.New("invalid delta positions")
	}

	// Apply the delta: remove [fromPos:toPos] and insert new text
	newContent := content[:fromPos] + insert + content[toPos:]
	return newContent, nil
}
