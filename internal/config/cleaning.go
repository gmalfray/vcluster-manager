package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// CleaningEntry tracks a vcluster where rancher-cleanup is running.
// Deletion flags are persisted so the operation can be resumed after a restart.
type CleaningEntry struct {
	Name           string `json:"name"`
	Env            string `json:"env"`
	StartedAt      string `json:"started_at"`
	DeletePreprod  bool   `json:"delete_preprod"`
	DeleteProd     bool   `json:"delete_prod"`
	DeleteGitlab   bool   `json:"delete_gitlab"`
	DeleteKeycloak bool   `json:"delete_keycloak"`
}

// AddCleaning registers a vcluster as having a rancher-cleanup in progress.
func (c *Config) AddCleaning(name, env string, deletePreprod, deleteProd, deleteGitlab, deleteKeycloak bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.loadCleaningLocked()

	for _, e := range entries {
		if e.Name == name && e.Env == env {
			return
		}
	}

	entries = append(entries, CleaningEntry{
		Name:           name,
		Env:            env,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		DeletePreprod:  deletePreprod,
		DeleteProd:     deleteProd,
		DeleteGitlab:   deleteGitlab,
		DeleteKeycloak: deleteKeycloak,
	})

	c.saveCleaningLocked(entries)
}

// RemoveCleaning removes a vcluster from the cleaning list.
func (c *Config) RemoveCleaning(name, env string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.loadCleaningLocked()
	var filtered []CleaningEntry
	for _, e := range entries {
		if !(e.Name == name && e.Env == env) {
			filtered = append(filtered, e)
		}
	}

	c.saveCleaningLocked(filtered)
}

// IsCleaning checks if a vcluster has a rancher-cleanup in progress.
// Auto-expires entries older than 1 hour.
func (c *Config) IsCleaning(name, env string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.loadCleaningLocked()
	cutoff := time.Now().UTC().Add(-1 * time.Hour)

	var active []CleaningEntry
	changed := false
	for _, e := range entries {
		t, err := time.Parse(time.RFC3339, e.StartedAt)
		if err != nil || t.Before(cutoff) {
			changed = true
			continue
		}
		active = append(active, e)
	}

	if changed {
		c.saveCleaningLocked(active)
	}

	for _, e := range active {
		if e.Name == name && e.Env == env {
			return true
		}
	}
	return false
}

// ListCleaning returns all active (non-expired) cleaning entries.
// Entries older than 1 hour are purged automatically, same as IsCleaning.
func (c *Config) ListCleaning() []CleaningEntry {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.loadCleaningLocked()
	cutoff := time.Now().UTC().Add(-1 * time.Hour)

	var active []CleaningEntry
	changed := false
	for _, e := range entries {
		t, err := time.Parse(time.RFC3339, e.StartedAt)
		if err != nil || t.Before(cutoff) {
			changed = true
			continue
		}
		active = append(active, e)
	}
	if changed {
		c.saveCleaningLocked(active)
	}
	return active
}

func (c *Config) cleaningPath() string {
	return filepath.Join(c.dataDir, "cleaning.json")
}

func (c *Config) loadCleaningLocked() []CleaningEntry {
	data, err := os.ReadFile(c.cleaningPath())
	if err != nil {
		return nil
	}
	var entries []CleaningEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Warn("could not parse cleaning state", "path", c.cleaningPath(), "err", err)
		return nil
	}
	return entries
}

func (c *Config) saveCleaningLocked(entries []CleaningEntry) {
	if err := os.MkdirAll(c.dataDir, 0755); err != nil {
		slog.Warn("could not create data dir", "dir", c.dataDir, "err", err)
		return
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		slog.Warn("could not marshal cleaning entries", "err", err)
		return
	}
	if err := os.WriteFile(c.cleaningPath(), data, 0644); err != nil {
		slog.Warn("could not write cleaning state", "path", c.cleaningPath(), "err", err)
	}
}
