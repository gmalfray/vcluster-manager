package service

import (
	"context"
	"errors"
	"testing"
)

// The ordering is the point, not the mapping: Unknown has to be checked before
// everything below it, otherwise a failed lookup that happens to arrive with a
// stale Cleaning or Paired flag gets dressed up as a real state.
func TestRancherStateOf(t *testing.T) {
	cases := []struct {
		name string
		in   RancherStatus
		want string
	}{
		{"pas activé sur la cell", RancherStatus{Enabled: false}, RancherStateOff},
		{"lookup en échec", RancherStatus{Enabled: true, Unknown: true}, RancherStateUnknown},
		{"lookup en échec pendant un cleaning", RancherStatus{Enabled: true, Unknown: true, Cleaning: true}, RancherStateUnknown},
		{"cleanup en cours", RancherStatus{Enabled: true, Cleaning: true}, RancherStateCleaning},
		{"appairé à la main", RancherStatus{Enabled: true, ManuallyPaired: true}, RancherStateManuallyPaired},
		{"appairé", RancherStatus{Enabled: true, Paired: true}, RancherStatePaired},
		{"import en cours", RancherStatus{Enabled: true, Pairing: true}, RancherStatePairing},
		{"activé mais absent de Rancher", RancherStatus{Enabled: true}, RancherStateOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rancherStateOf(tc.in); got != tc.want {
				t.Fatalf("état = %q, attendu %q", got, tc.want)
			}
		})
	}
}

// Sans client pour la cell, l'observation ne raconte rien plutôt que de rendre
// des zéros qui se liraient comme des faits.
func TestObserveVCluster_NoClient(t *testing.T) {
	obs := newTestService().ObserveVCluster(context.Background(), "demo", "")

	if !errors.Is(obs.Err, ErrK8sUnavailable) {
		t.Fatalf("Err = %v, attendu ErrK8sUnavailable", obs.Err)
	}
	if obs.Env != "preprod" {
		t.Fatalf("env vide non ramené à preprod : %q", obs.Env)
	}
	if obs.RancherKnown || obs.ProtectionKnown || obs.BackupsKnown {
		t.Fatal("une source est annoncée comme lue alors qu'aucune n'a été interrogée")
	}
	if obs.RancherState != "" {
		t.Fatalf("état Rancher = %q : rien n'a été demandé à Rancher, « Off » serait une affirmation", obs.RancherState)
	}
}
