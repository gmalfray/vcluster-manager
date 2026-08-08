// Command operator runs the vcluster-manager Kubernetes operator: it reconciles
// VClusterVeleroOps markers, turning backup/restore trigger annotations into
// Velero work by calling internal/service — the same business logic the web UI
// calls, consumed by a third adapter (design §7).
//
// It is a separate binary from cmd/server on purpose. The server serves humans
// and holds the integrations (Keycloak, GitLab, Rancher, Vault); the operator
// runs in the cluster it reconciles and needs none of that — only a Kubernetes
// client and the Velero half of the service.
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
		K8sClients:   map[string]*kubernetes.StatusClient{cell: k8sClient},
		K8sClientsMu: &sync.RWMutex{},
	})
	// Everything the Velero domain of the service needs is above. The other Deps
	// (GitLab, Keycloak, Rancher, Vault…) belong to vcluster lifecycle methods
	// this operator does not call — leaving them nil is deliberate, and a call
	// that needed one would panic loudly rather than misbehave quietly.

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

	if err := (&controller.VeleroOpsReconciler{
		Client: mgr.GetClient(),
		Ops:    svc,
		Cell:   cell,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "câblage du reconciler VClusterVeleroOps")
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
