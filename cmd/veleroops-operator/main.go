// Command veleroops-operator réconcilie la CRD marqueur VClusterVeleroOps :
// sauvegarde et restauration Velero à la demande (internal/controller/
// veleroops_controller.go).
//
// Binaire séparé de cmd/operator, et c'est tout l'intérêt du découpage.
// L'ancien manager unique portait CE reconciler ET VClusterReconciler dans
// le même pod, donc sous le même ServiceAccount — celui qui a `delete
// namespaces` cluster-wide (arbitrage N6) et les identifiants Vault,
// Keycloak et Rancher (wireIntegrations, cmd/operator/integrations.go). Un
// pod qui exécute une restauration Velero — suspend Flux, scale un
// StatefulSet à zéro, supprime un PVC — n'a besoin d'aucun des deux : il ne
// crée ni ne supprime de namespace, il ne parle à aucun système externe.
//
// D'où ce que ce binaire NE câble PAS, volontairement :
//   - wireIntegrations et ses trois clients : ce reconciler ne les appelle
//     jamais (voir veleroops_controller.go, l'interface veleroops.Ops) ;
//   - le générateur GitOps : rien ici n'expand une CRD en objets Flux ;
//   - --vclusters-namespace : cette contrainte n'a de sens que pour la CRD
//     VCluster, dont l'espace de noms est plat par construction
//     (namespace_guard.go). Un marqueur VClusterVeleroOps, lui, est
//     auto-borné à son propre namespace vcluster-<nom> — markerMisplaced
//     s'en charge à chaque Reconcile, pas un flag de démarrage.
//
// Son ClusterRole (deploy/base/veleroops-operator-rbac.yaml) est donc plus
// étroit que celui de cmd/operator sur des points qui comptent en sécurité :
// pas de `delete namespaces`, pas de `create/patch configmaps`, pas de
// `resourcequotas`, pas de secrets Vault/Keycloak/Rancher — mais il porte en
// plus ce que VClusterReconciler n'a jamais eu besoin de faire : scaler des
// workloads, supprimer un PVC, créer des Restore Velero.
package main

import (
	"flag"
	"log/slog"
	"os"
	"sync"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/controller"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/service"
	"github.com/gmalfray/vcluster-manager/internal/version"
)

func main() {
	var metricsAddr, probeAddr, cell string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "adresse d'écoute des métriques")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "adresse d'écoute des sondes")
	// Pas de flag `kubeconfig` ici, pour la même raison que cmd/operator :
	// controller-runtime en enregistre déjà un dans son init(), le redéfinir
	// fait paniquer le binaire au démarrage.
	flag.StringVar(&cell, "cell", "preprod", "nom de la cell (cluster hôte) que ce binaire réconcilie — étiquette l'audit")
	// L'élection de leader importe même à une seule réplique : pendant un
	// rolling update, l'ancien et le nouveau pod se chevauchent quelques
	// secondes, et deux pilotes pour une restauration destructrice est
	// exactement ce qu'on veut éviter.
	flag.BoolVar(&enableLeaderElection, "leader-elect", true, "active l'élection de leader")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	kubeconfig := ""
	if f := flag.Lookup("kubeconfig"); f != nil {
		kubeconfig = f.Value.String()
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")
	log.Info("démarrage de l'opérateur veleroops", "version", version.Version)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Error(err, "scheme client-go")
		os.Exit(1)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		log.Error(err, "scheme vcluster.rebuild-it.fr")
		os.Exit(1)
	}

	// Un seul client, comme cmd/operator — ce binaire tourne dans le cluster
	// qu'il réconcilie, indexé par la cell pour éviter le repli
	// "n'importe quel client" de k8sForEnv.
	k8sClient, err := kubernetes.NewStatusClient(kubeconfig)
	if err != nil {
		log.Error(err, "client Kubernetes")
		os.Exit(1)
	}

	cfg, err := config.LoadOperator()
	if err != nil {
		log.Error(err, "configuration")
		os.Exit(1)
	}

	// Deps volontairement maigre : ni générateur, ni GitLab, ni Keycloak, ni
	// Rancher, ni Vault. Le seul domaine que ce reconciler consomme
	// (internal/veleroops.Ops) est Velero, et Velero ne passe pas par ces
	// clients — voir le commentaire de tête sur ce que ce binaire ne câble
	// pas, et pourquoi.
	svc := service.New(service.Deps{
		Cfg:          cfg,
		K8sClients:   map[string]*kubernetes.StatusClient{cell: k8sClient},
		K8sClientsMu: &sync.RWMutex{},
	})

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		// Distinct de celui de cmd/operator : deux managers avec le MÊME
		// LeaderElectionID se disputeraient un seul lease, et l'un des deux
		// resterait éternellement en attente sans jamais réconcilier quoi
		// que ce soit — le piège n°1 de ce découpage.
		LeaderElectionID: "vcluster-manager-veleroops-operator.rebuild-it.fr",
	})
	if err != nil {
		log.Error(err, "manager")
		os.Exit(1)
	}

	if err := (&controller.VeleroOpsReconciler{
		Client: mgr.GetClient(),
		Ops:    svc,
		Cell:   cell,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "câblage du reconciler VClusterVeleroOps")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "healthz")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "readyz")
		os.Exit(1)
	}

	slog.Info("opérateur veleroops prêt", "cell", cell, "leader-election", enableLeaderElection)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "arrêt du manager")
		os.Exit(1)
	}
}
