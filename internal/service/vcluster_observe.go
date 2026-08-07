package service

import (
	"context"
	"sync"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

// Read budgets. Each source gets its own, because they fail independently and
// sharing one would make a slow port-forward look like an unreachable Rancher.
//
// The host + intra-vcluster read is the expensive one: withVClusterPortForward
// waits up to 10s for the SPDY tunnel then 5s for the discovery call, so 20s is
// its natural ceiling plus a little slack. The other three are plain HTTP calls
// against services that are either up or not.
const (
	ObserveBudgetCluster    = 20 * time.Second
	ObserveBudgetRancher    = 10 * time.Second
	ObserveBudgetProtection = 5 * time.Second
	ObserveBudgetBackups    = 10 * time.Second
)

// Rancher pairing states, the same six words rancher_status.html renders.
// Unknown is one of them on purpose: it is what the operator writes when the
// lookup failed, so nothing downstream has to guess whether "not paired" means
// "we checked" or "we could not check".
const (
	RancherStateOff            = "Off"
	RancherStatePaired         = "Paired"
	RancherStatePairing        = "Pairing"
	RancherStateManuallyPaired = "ManuallyPaired"
	RancherStateCleaning       = "Cleaning"
	RancherStateUnknown        = "Unknown"
)

// VClusterObservation is everything the operator managed to read about one
// vcluster in one pass — and, just as importantly, what it failed to read.
//
// Each block carries its own "did this actually come back". That is not
// defensive decoration: a source that stays silent and a source that answers
// "no" mean opposite things, and collapsing them is how an unreachable Rancher
// ends up looking like a deliberately unpaired vcluster. Same reasoning as
// VeleroRestoreStatusView, which keeps ResumePending and ResumeFailed apart.
//
// Nothing here is recomputed: the values come from the readers the dashboard
// and the status badge already use.
type VClusterObservation struct {
	Name string
	Env  string

	// Err is set when there was nothing to read from: no Kubernetes client for
	// the cell. Every block below stays at its zero value, which reads as
	// "unknown" everywhere.
	Err error

	// ClusterTimedOut says the budget ran out on the host + intra-vcluster read.
	// The distinction matters in the message: "the read did not finish" is not
	// "the vcluster is broken".
	ClusterTimedOut bool

	// HelmRelease and Kustomization carry the three words StatusInfo produces:
	// "Ready", "False"/"Unknown"... — copied through, not interpreted. "Unknown"
	// means the object could not be read, NOT that it is unhealthy.
	HelmRelease   string
	Kustomization string

	ChartVersion string

	// K8sVersion is read from inside the vcluster, through the port-forward.
	// Empty means the read did not get through — a vcluster always has a
	// version.
	K8sVersion string

	// Quota usage, already formatted as "used/limit". Empty = no quota object,
	// or the namespace could not be listed; the two are not worth telling apart
	// here since neither is a claim about health.
	CPUUsage     string
	MemoryUsage  string
	StorageUsage string

	// RancherEnabled is false when Rancher is not configured for the cell at
	// all. In that case RancherKnown is true and the state is Off: "there is
	// nothing to pair with" is a fact, not an unknown.
	//
	// L'exception est le processus qui n'a pas de client alors que la cell
	// annonce Rancher actif : « rien à appairer » n'y est plus un fait, c'est une
	// mauvaise configuration. RancherStatus.NotConfigured porte ce cas et le
	// ramène ici en Unknown.
	RancherEnabled bool
	// RancherKnown is false when the lookup failed, or when this process has no
	// Rancher client to ask with.
	RancherKnown  bool
	RancherState  string
	RancherPaired bool

	// ProtectionKnown is false when no client could answer whether the
	// protect-deletion annotation is on the namespace.
	ProtectionKnown bool
	Protected       bool

	// BackupsKnown tells "no backup exists" (known, LastBackup nil) from "we
	// could not list them" (unknown, LastBackup nil too).
	BackupsKnown bool
	LastBackup   *models.VeleroBackupInfo
}

// ObserveVCluster reads the live state of a vcluster for the operator's status
// subresource. Read-only, no privilege required.
//
// It never returns an error: a source that does not answer is reported as
// unknown, not as a failure. The caller is a reconcile loop, and a reconcile
// that fails because Rancher is down would retry the whole lifecycle over a
// piece of information nobody is blocked on.
//
// The four sources are read in parallel, each under its own budget. Sequentially
// the worst case would add up to 45s — longer than the reconcile's own requeue
// interval, which would leave the operator permanently one pass behind whenever
// several integrations are down at once.
func (s *Service) ObserveVCluster(ctx context.Context, name, env string) VClusterObservation {
	env = envOrDefault(env)
	obs := VClusterObservation{Name: name, Env: env}

	k8s := s.k8sForEnv(env)
	if k8s == nil {
		obs.Err = ErrK8sUnavailable
		return obs
	}

	// Each goroutine owns its own variable and everything is merged after Wait,
	// rather than four writers poking at one struct: distinct fields would be
	// safe, but "safe if you know the memory model" is not the same as obvious.
	var (
		wg         sync.WaitGroup
		info       *models.StatusInfo
		clusterOut bool
		rancher    RancherStatus
		protection ProtectionState
		backups    VeleroBackupsView
		backupsErr error
	)
	wg.Add(4)

	go func() {
		defer wg.Done()
		readCtx, cancel := context.WithTimeout(ctx, ObserveBudgetCluster)
		defer cancel()

		// GetVClusterStatus swallows its own sub-failures and reports them as
		// "Unknown" / empty strings, which is exactly the vocabulary this type
		// wants. A cancelled context therefore comes back as an all-unknown
		// reading rather than an error — the graceful degradation is already
		// there, it just needs to be labelled.
		info, _ = k8s.GetVClusterStatus(readCtx, name)
		clusterOut = ctx.Err() == nil && readCtx.Err() != nil
	}()

	go func() {
		defer wg.Done()
		readCtx, cancel := context.WithTimeout(ctx, ObserveBudgetRancher)
		defer cancel()
		rancher = s.GetRancherStatus(readCtx, name, env)
	}()

	go func() {
		defer wg.Done()
		readCtx, cancel := context.WithTimeout(ctx, ObserveBudgetProtection)
		defer cancel()
		protection = s.GetProtection(readCtx, name, env)
	}()

	go func() {
		defer wg.Done()
		readCtx, cancel := context.WithTimeout(ctx, ObserveBudgetBackups)
		defer cancel()
		backups, backupsErr = s.GetVeleroBackups(readCtx, name, env)
	}()

	wg.Wait()

	obs.ClusterTimedOut = clusterOut
	if info != nil {
		obs.HelmRelease = info.HelmRelease
		obs.Kustomization = info.FluxKustomization
		obs.ChartVersion = info.ChartVersion
		obs.K8sVersion = info.K8sVersion
		obs.CPUUsage = info.CPUUsage
		obs.MemoryUsage = info.MemoryUsage
		obs.StorageUsage = info.StorageUsage
	}

	obs.RancherEnabled = rancher.Enabled
	obs.RancherPaired = rancher.Paired
	obs.RancherKnown = !rancher.Unknown && !rancher.NotConfigured
	obs.RancherState = rancherStateOf(rancher)

	obs.ProtectionKnown = protection.Available
	obs.Protected = protection.Protected

	if backupsErr == nil {
		obs.BackupsKnown = true
		if len(backups.Backups) > 0 {
			// GetVeleroBackups sorts newest first.
			b := backups.Backups[0]
			obs.LastBackup = &b
		}
	}
	return obs
}

// rancherStateOf collapses the six booleans of RancherStatus into the one word
// the CR status carries. Order matters: Unknown first, because a failed lookup
// must not be dressed up as any of the states below it.
func rancherStateOf(rs RancherStatus) string {
	switch {
	// Avant !Enabled : un processus sans client rend Enabled=false, et le
	// rapporter Off dirait « rien à appairer » là où on ne sait simplement pas.
	case rs.NotConfigured:
		return RancherStateUnknown
	case !rs.Enabled:
		return RancherStateOff
	case rs.Unknown:
		return RancherStateUnknown
	case rs.Cleaning:
		return RancherStateCleaning
	case rs.ManuallyPaired:
		return RancherStateManuallyPaired
	case rs.Paired:
		return RancherStatePaired
	case rs.Pairing:
		return RancherStatePairing
	default:
		return RancherStateOff
	}
}
