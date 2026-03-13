package config

import (
	"os"
	"path/filepath"
)

// fileBackend stores state in local JSON files under dataDir.
// This is the default backend, equivalent to the original behaviour.
type fileBackend struct {
	dataDir string
}

func (b *fileBackend) readSettings() ([]byte, error) {
	return os.ReadFile(filepath.Join(b.dataDir, "settings.json"))
}

func (b *fileBackend) writeSettings(data []byte) error {
	if err := os.MkdirAll(b.dataDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.dataDir, "settings.json"), data, 0644)
}

func (b *fileBackend) readDeleting() ([]byte, error) {
	return os.ReadFile(filepath.Join(b.dataDir, "deleting.json"))
}

func (b *fileBackend) writeDeleting(data []byte) error {
	if err := os.MkdirAll(b.dataDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.dataDir, "deleting.json"), data, 0644)
}
