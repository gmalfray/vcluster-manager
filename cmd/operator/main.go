// Command operator réconcilie la CRD VCluster en appelant internal/service —
// la même logique métier que l'UI web, consommée par un troisième adaptateur
// (design §7).
//
// C'est un binaire séparé de cmd/server, mais plus totalement étranger à ses
// intégrations : il porte ses propres clients Vault, Keycloak et Rancher
// (voir wireIntegrations, integrations.go), parce que reconcileIntegrations
// (internal/controller/vcluster_integrations.go) en a besoin pour faire
// autre chose que rendre Unknown/NotConfigured. GitLab reste absent : aucune
// étape du reconcile n'appelle de client GitLab aujourd'hui — voir
// integrations.go pour la raison précise.
//
// Le reconciler VClusterVeleroOps (sauvegarde/restauration Velero) est un
// AUTRE binaire, cmd/veleroops-operator : les deux tournaient dans le même
// manager, donc dans le même pod, donc avec le même ClusterRole — celui
// qui porte `delete namespaces` cluster-wide ET les trois secrets Vault/
// Keycloak/Rancher. Un pod qui exécute des restaurations Velero n'a besoin
// d'aucun des deux. Les séparer resserre chaque ClusterRole à ce que son
// propre reconciler touche — voir deploy/base/operator-rbac.yaml et
// deploy/base/veleroops-operator-rbac.yaml, et
// docs/etat-brique-operateur.md pour l'historique de la bascule.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/gmalfray/vcluster-manager/api/v1alpha1"
	"github.com/gmalfray/vcluster-manager/internal/config"
	"github.com/gmalfray/vcluster-manager/internal/controller"
	"github.com/gmalfray/vcluster-manager/internal/gitops"
	"github.com/gmalfray/vcluster-manager/internal/kubernetes"
	"github.com/gmalfray/vcluster-manager/internal/service"
	"github.com/gmalfray/vcluster-manager/internal/version"
)

func main() {
	var metricsAddr, probeAddr, cell, vclustersNamespace string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metric endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the probe endpoint binds to")
	// Pas de flag `kubeconfig` ici : controller-runtime en enregistre déjà un sur
	// flag.CommandLine dans son init(), et le redéfinir fait paniquer le binaire
	// au démarrage (« flag redefined: kubeconfig »). On lit le sien après Parse.
	// Which host cluster this operator reconciles — a "cell" in the sense of
	// ADR-002. Not used to choose a client (there is only one), but it labels the
	// audit trail and the Prometheus series: an operator reporting another cell's
	// name would be actively misleading, so the overlay must set this.
	flag.StringVar(&vclustersNamespace, "vclusters-namespace", controller.DefaultVClustersNamespace,
		"seul namespace d'où des VCluster sont acceptés — un CR déposé ailleurs est ignoré")
	flag.StringVar(&cell, "cell", "preprod", "nom de la cell (cluster hôte) que cet opérateur réconcilie — étiquette l'audit et les métriques")
	// Leader election matters even for a single replica: it stops a rolling
	// update's old and new pods from both driving a destructive restore sequence
	// for the few seconds they overlap.
	flag.BoolVar(&enableLeaderElection, "leader-elect", true, "enable leader election")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Le flag --kubeconfig appartient à controller-runtime ; vide = in-cluster.
	kubeconfig := ""
	if f := flag.Lookup("kubeconfig"); f != nil {
		kubeconfig = f.Value.String()
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")
	log.Info("démarrage de l'opérateur", "version", version.Version)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Error(err, "scheme client-go")
		os.Exit(1)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		log.Error(err, "scheme vcluster.rebuild-it.fr")
		os.Exit(1)
	}

	// The operator runs *in* the environment it reconciles, so there is a single
	// client and no SSH tunnel — crd-vcluster.md §2.1: one operator per host
	// cluster. Keying the map by the cell name (rather than by "") means
	// k8sForEnv finds it by name instead of landing on its "return any client"
	// fallback, which the operator must not depend on. The service still calls
	// this dimension `env` — see ADR-002 for why the two names coexist for now.
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

	// Vault, Keycloak, Rancher : configuration absente = Unknown/NotConfigured
	// plus loin dans le reconcile, c'est un état légitime. Configuration
	// incohérente (une URL sans les identifiants qui vont avec) = échec ici,
	// tout de suite — voir integrations.go pour le détail de la règle et la
	// divergence assumée avec cmd/server sur ce point précis.
	wireCtx, cancelWire := context.WithTimeout(context.Background(), 30*time.Second)
	integrations, err := wireIntegrations(wireCtx, cfg)
	cancelWire()
	if err != nil {
		log.Error(err, "configuration des intégrations Vault/Keycloak/Rancher")
		os.Exit(1)
	}
	logIntegrationOutcome(log, "Vault", integrations.vault != nil, cfg.VaultAddr)
	logIntegrationOutcome(log, "Keycloak", integrations.keycloak != nil, cfg.KeycloakURL)
	logIntegrationOutcome(log, "Rancher", integrations.rancher != nil, cfg.RancherURL)

	svc := service.New(service.Deps{
		Cfg: cfg,
		// Le générateur ne parle à personne : il dérive des valeurs depuis le nom,
		// la cell et la configuration. C'est ce qui rend le ConfigMap de
		// substitutions que le reconcile applique.
		Generator: gitops.NewGenerator(gitops.GeneratorConfig{
			BaseDomainPreprod:   cfg.BaseDomainPreprod,
			BaseDomainProd:      cfg.BaseDomainProd,
			TLSSecretPreprod:    cfg.TLSSecretPreprod,
			TLSSecretProd:       cfg.TLSSecretProd,
			OIDCIssuer:          cfg.KeycloakURL + "/auth/realms/" + cfg.KeycloakRealm,
			GitLabSSHURL:        cfg.GitLabSSHURL,
			GitLabArgoCDPath:    cfg.GitLabArgoCDPath,
			DefaultCPU:          cfg.DefaultCPU,
			DefaultMemory:       cfg.DefaultMemory,
			DefaultStorage:      cfg.DefaultStorage,
			VeleroTimezone:      cfg.VeleroTimezone,
			VeleroDefaultTTL:    cfg.VeleroDefaultTTL,
			VClusterPodSecurity: cfg.VClusterPodSecurity,
			ArgoCDDefaultPolicy: cfg.ArgoCDDefaultPolicy,
		}),
		Keycloak:     integrations.keycloak,
		Rancher:      integrations.rancher,
		Vault:        integrations.vault,
		K8sClients:   map[string]*kubernetes.StatusClient{cell: k8sClient},
		K8sClientsMu: &sync.RWMutex{},
	})
	// GitLab reste à nil : aucune étape du reconcile n'en appelle un
	// aujourd'hui (integrations.go détaille pourquoi), donc il n'y a rien à
	// câbler — pas un oubli.

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "vcluster-manager-operator.rebuild-it.fr",
	})
	if err != nil {
		log.Error(err, "manager")
		os.Exit(1)
	}

	if err := (&controller.VClusterReconciler{
		Client:    mgr.GetClient(),
		Ops:       svc,
		BudgetOps: svc,
		Budget: controller.BudgetLimits{
			CPU:     cfg.ResourceBudgetCPU,
			Memory:  cfg.ResourceBudgetMemory,
			Storage: cfg.ResourceBudgetStorage,
		},
		Cell:      cell,
		Namespace: vclustersNamespace,
		// Le seul consommateur aujourd'hui est la conclusion de la séquence de
		// suppression : elle écrit sa phrase dans un status dont l'objet
		// disparaît deux appels plus loin, et l'Event est ce qui en garde une
		// trace consultable après coup (voir vcluster_finalizer.go).
		Recorder: mgr.GetEventRecorder("vcluster-controller"),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "câblage du reconciler VCluster")
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

	slog.Info("opérateur prêt", "cell", cell, "leader-election", enableLeaderElection)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "arrêt du manager")
		os.Exit(1)
	}
}
