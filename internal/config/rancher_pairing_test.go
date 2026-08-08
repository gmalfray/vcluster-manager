package config

import "testing"

func newRancherPairingTestConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{dataDir: t.TempDir()}
}

func TestRancherPairingFailure_SetThenGet(t *testing.T) {
	c := newRancherPairingTestConfig(t)

	if _, recorded := c.RancherPairingFailureFor("demo", "preprod"); recorded {
		t.Fatal("expected no failure recorded before any Set")
	}

	c.SetRancherPairingFailure("demo", "preprod", "le cluster n'est pas devenu actif")

	got, recorded := c.RancherPairingFailureFor("demo", "preprod")
	if !recorded {
		t.Fatal("expected the failure to be recorded")
	}
	if got.Message != "le cluster n'est pas devenu actif" {
		t.Fatalf("expected the recorded message, got %q", got.Message)
	}
	if got.At == "" {
		t.Fatal("expected a non-empty timestamp")
	}
}

// TestRancherPairingFailure_SetReplacesRatherThanAccumulates: only the most
// recent attempt matters. If Set appended instead of replacing, a vcluster
// that failed to pair three times over its history would carry three stale
// entries forever instead of one current one.
func TestRancherPairingFailure_SetReplacesRatherThanAccumulates(t *testing.T) {
	c := newRancherPairingTestConfig(t)

	c.SetRancherPairingFailure("demo", "preprod", "premier échec")
	c.SetRancherPairingFailure("demo", "preprod", "second échec")

	got, recorded := c.RancherPairingFailureFor("demo", "preprod")
	if !recorded {
		t.Fatal("expected the failure to be recorded")
	}
	if got.Message != "second échec" {
		t.Fatalf("expected the second Set to replace the first, got %q", got.Message)
	}
}

// TestRancherPairingFailure_ScopedByNameAndEnv: two vclusters (or the same
// name in two envs) must not step on each other's recorded failure.
func TestRancherPairingFailure_ScopedByNameAndEnv(t *testing.T) {
	c := newRancherPairingTestConfig(t)

	c.SetRancherPairingFailure("demo", "preprod", "échec preprod")
	c.SetRancherPairingFailure("demo", "prod", "échec prod")
	c.SetRancherPairingFailure("autre", "preprod", "échec autre")

	preprod, _ := c.RancherPairingFailureFor("demo", "preprod")
	prod, _ := c.RancherPairingFailureFor("demo", "prod")
	autre, _ := c.RancherPairingFailureFor("autre", "preprod")

	if preprod.Message != "échec preprod" {
		t.Fatalf("preprod entry got clobbered: %q", preprod.Message)
	}
	if prod.Message != "échec prod" {
		t.Fatalf("prod entry got clobbered: %q", prod.Message)
	}
	if autre.Message != "échec autre" {
		t.Fatalf("autre entry got clobbered: %q", autre.Message)
	}
}

func TestRancherPairingFailure_Clear(t *testing.T) {
	c := newRancherPairingTestConfig(t)

	c.SetRancherPairingFailure("demo", "preprod", "échec")
	c.SetRancherPairingFailure("autre", "preprod", "échec autre")

	c.ClearRancherPairingFailure("demo", "preprod")

	if _, recorded := c.RancherPairingFailureFor("demo", "preprod"); recorded {
		t.Fatal("expected the cleared entry to be gone")
	}
	if _, recorded := c.RancherPairingFailureFor("autre", "preprod"); !recorded {
		t.Fatal("expected the unrelated entry to survive the clear")
	}
}

// TestRancherPairingFailure_ClearOfUnknownEntryIsANoop: unpairing a vcluster
// that never failed to pair must not panic or corrupt the store.
func TestRancherPairingFailure_ClearOfUnknownEntryIsANoop(t *testing.T) {
	c := newRancherPairingTestConfig(t)
	c.ClearRancherPairingFailure("demo", "preprod")
	if _, recorded := c.RancherPairingFailureFor("demo", "preprod"); recorded {
		t.Fatal("expected no failure recorded")
	}
}

// TestRancherPairingFailure_SurvivesReload: the whole point is to outlive the
// process that recorded the failure (the pod restarts, the goroutine that
// discovered the problem is long gone) — a fresh *Config pointed at the same
// dataDir must still see it.
func TestRancherPairingFailure_SurvivesReload(t *testing.T) {
	dir := t.TempDir()
	c1 := &Config{dataDir: dir}
	c1.SetRancherPairingFailure("demo", "preprod", "échec avant redémarrage")

	c2 := &Config{dataDir: dir}
	got, recorded := c2.RancherPairingFailureFor("demo", "preprod")
	if !recorded {
		t.Fatal("expected the failure to survive across a reload of the store")
	}
	if got.Message != "échec avant redémarrage" {
		t.Fatalf("expected the message to survive, got %q", got.Message)
	}
}
