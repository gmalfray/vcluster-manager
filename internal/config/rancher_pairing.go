package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// RancherPairingFailure records the outcome of a failed Rancher pairing
// attempt for a vcluster. PairRancher runs its heavy work (import, manifest
// apply, wait-for-active) in a background goroutine that outlives the
// request; without this, its only trace of failure was a log line in a pod
// nobody was watching, and the vcluster was left showing "en cours" forever.
type RancherPairingFailure struct {
	Name    string `json:"name"`
	Env     string `json:"env"`
	At      string `json:"at"` // RFC3339
	Message string `json:"message"`
}

// SetRancherPairingFailure records name/env's most recent pairing failure,
// replacing whatever was recorded before it.
func (c *Config) SetRancherPairingFailure(name, env, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.loadRancherPairingFailuresLocked()
	var filtered []RancherPairingFailure
	for _, e := range entries {
		if e.Name != name || e.Env != env {
			filtered = append(filtered, e)
		}
	}
	filtered = append(filtered, RancherPairingFailure{
		Name:    name,
		Env:     env,
		At:      time.Now().UTC().Format(time.RFC3339),
		Message: message,
	})
	c.saveRancherPairingFailuresLocked(filtered)
}

// ClearRancherPairingFailure drops name/env's recorded pairing failure, if
// any. Called when a fresh pairing attempt actually starts (so a stale
// message from a previous, unrelated attempt doesn't linger) and when one
// finally succeeds.
func (c *Config) ClearRancherPairingFailure(name, env string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := c.loadRancherPairingFailuresLocked()
	var filtered []RancherPairingFailure
	for _, e := range entries {
		if e.Name != name || e.Env != env {
			filtered = append(filtered, e)
		}
	}
	c.saveRancherPairingFailuresLocked(filtered)
}

// RancherPairingFailureFor returns name/env's most recently recorded pairing
// failure, if any.
func (c *Config) RancherPairingFailureFor(name, env string) (RancherPairingFailure, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, e := range c.loadRancherPairingFailuresLocked() {
		if e.Name == name && e.Env == env {
			return e, true
		}
	}
	return RancherPairingFailure{}, false
}

func (c *Config) rancherPairingFailuresPath() string {
	return filepath.Join(c.dataDir, "rancher_pairing_failures.json")
}

func (c *Config) loadRancherPairingFailuresLocked() []RancherPairingFailure {
	data, err := os.ReadFile(c.rancherPairingFailuresPath())
	if err != nil {
		return nil
	}
	var entries []RancherPairingFailure
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Warn("could not parse rancher pairing failures state", "path", c.rancherPairingFailuresPath(), "err", err)
		return nil
	}
	return entries
}

func (c *Config) saveRancherPairingFailuresLocked(entries []RancherPairingFailure) {
	if err := os.MkdirAll(c.dataDir, 0755); err != nil {
		slog.Warn("could not create data dir", "dir", c.dataDir, "err", err)
		return
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		slog.Warn("could not marshal rancher pairing failures", "err", err)
		return
	}
	if err := os.WriteFile(c.rancherPairingFailuresPath(), data, 0644); err != nil {
		slog.Warn("could not write rancher pairing failures state", "path", c.rancherPairingFailuresPath(), "err", err)
	}
}
