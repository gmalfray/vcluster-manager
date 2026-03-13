package config

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/metrics"
)

// DeletingEntry tracks a vcluster that is being deleted (waiting for K8s reconciliation).
type DeletingEntry struct {
	Name      string `json:"name"`
	Env       string `json:"env"`
	MRURL     string `json:"mr_url,omitempty"`
	DeletedAt string `json:"deleted_at"`
}

// AddDeleting registers a vcluster as being deleted.
func (c *Config) AddDeleting(name, env, mrURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.loadDeletingLocked()

	// Avoid duplicates
	for _, e := range entries {
		if e.Name == name && e.Env == env {
			return
		}
	}

	entries = append(entries, DeletingEntry{
		Name:      name,
		Env:       env,
		MRURL:     mrURL,
		DeletedAt: time.Now().UTC().Format(time.RFC3339),
	})

	c.saveDeletingLocked(entries)
	metrics.ActiveDeletions.Inc()
}

// RemoveDeleting removes a vcluster from the deleting list.
func (c *Config) RemoveDeleting(name, env string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.loadDeletingLocked()
	var filtered []DeletingEntry
	for _, e := range entries {
		if !(e.Name == name && e.Env == env) {
			filtered = append(filtered, e)
		}
	}

	if len(filtered) < len(entries) {
		metrics.ActiveDeletions.Dec()
	}
	c.saveDeletingLocked(filtered)
}

// IsDeleting checks if a vcluster is in the deleting list.
func (c *Config) IsDeleting(name, env string) (bool, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entries := c.loadDeletingLocked()
	for _, e := range entries {
		if e.Name == name && e.Env == env {
			return true, e.MRURL
		}
	}
	return false, ""
}

// ListDeleting returns all vclusters currently being deleted, with auto-cleanup of entries > 24h.
func (c *Config) ListDeleting() []DeletingEntry {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.loadDeletingLocked()
	cutoff := time.Now().UTC().Add(-24 * time.Hour)

	var active []DeletingEntry
	changed := false
	for _, e := range entries {
		t, err := time.Parse(time.RFC3339, e.DeletedAt)
		if err != nil || t.Before(cutoff) {
			changed = true
			continue
		}
		active = append(active, e)
	}

	if changed {
		c.saveDeletingLocked(active)
	}

	return active
}

// loadDeletingLocked reads the deleting state. Caller must hold c.mu (read or write).
func (c *Config) loadDeletingLocked() []DeletingEntry {
	data, err := c.backend.readDeleting()
	if err != nil {
		return nil
	}
	var entries []DeletingEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("Warning: could not parse deleting state: %v", err)
		return nil
	}
	return entries
}

// saveDeletingLocked writes the deleting state. Caller must hold c.mu for write.
func (c *Config) saveDeletingLocked(entries []DeletingEntry) {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		log.Printf("Warning: could not marshal deleting entries: %v", err)
		return
	}
	if err := c.backend.writeDeleting(data); err != nil {
		log.Printf("Warning: could not write deleting state: %v", err)
	}
}
