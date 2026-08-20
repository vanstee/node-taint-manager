package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/avast/retry-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/homedir"
	apiv1pod "k8s.io/kubernetes/pkg/api/v1/pod"
)

const (
	DefaultResyncInterval         = 10 * time.Minute
	DefaultReconciliationInterval = 5 * time.Second

	DefaultLeaderElectionLeaseDuration = 15 * time.Second
	DefaultLeaderElectionRenewDeadline = 10 * time.Second
	DefaultLeaderElectionRetryPeriod   = 2 * time.Second

	DefaultLeaderElectionName      = "node-taint-manager"
	DefaultLeaderElectionNamespace = "node-taint-manager"
	PodNamespaceEnvironment        = "POD_NAMESPACE"

	PodsInformerIndexByNodeName = "ByNodeName"

	TaintNodeDaemonSetNotReady = "node.vanstee.github.io/daemonset-not-ready"

	JSONPatchOperationOpRemove = "remove"
)

var (
	totalNodeCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "node_taint_manager_nodes_monitored",
		Help: "The total number of nodes node-taint-manager is tracking.",
	})

	nodesUntainted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "node_taint_manager_nodes_untainted",
		Help: "The number of nodes node-taint-manager has determined ready and removed taint from.",
	})
	timeToStartup = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "node_taint_manager_time_to_ready",
		Help:    "Time in seconds taken for the all the daemonsets on the nodes to be ready",
		Buckets: []float64{0.1, 1, 2, 3, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55, 60, 70, 80, 90, 100, 110, 120},
	}, []string{})
	leaderStatus = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "node_taint_manager_leader",
		Help: "Whether this process currently leads node-taint-manager reconciliation.",
	})
)

type JSONPatchOperation struct {
	Op   string `json:"op"`
	Path string `json:"path"`
}

type options struct {
	leaderElect             bool
	leaderElectionName      string
	leaderElectionNamespace string
}

func main() {
	opts := parseOptions()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("failed to retrieve in-cluster config, using current context from local kube config")

		home := homedir.HomeDir()
		if home == "" {
			log.Fatalf("failed to retrieve home directory: %v", err)
		}

		kubeconfig := filepath.Join(home, ".kube", "config")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.Fatalf("failed to build kube config: %v", err)
		}
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to create client: %v", err)
	}

	transformOption := informers.WithTransform(
		func(obj interface{}) (interface{}, error) {
			switch t := obj.(type) {
			case *apiv1.Node:
				return &apiv1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:              t.ObjectMeta.Name,
						CreationTimestamp: t.ObjectMeta.CreationTimestamp,
					},
					Spec: apiv1.NodeSpec{
						Taints: t.Spec.Taints,
					},
				}, nil
			case *apiv1.Pod:
				return &apiv1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:            t.ObjectMeta.Name,
						Namespace:       t.ObjectMeta.Namespace,
						OwnerReferences: t.ObjectMeta.OwnerReferences,
					},
					Spec: apiv1.PodSpec{
						NodeName:    t.Spec.NodeName,
						Tolerations: t.Spec.Tolerations,
					},
					Status: apiv1.PodStatus{
						Conditions: t.Status.Conditions,
					},
				}, nil
			default:
				return obj, nil
			}
		},
	)

	factory := informers.NewSharedInformerFactoryWithOptions(client, DefaultResyncInterval, transformOption)

	nodesInformer := factory.Core().V1().Nodes().Informer()

	podsInformer := factory.Core().V1().Pods().Informer()
	if err := podsInformer.AddIndexers(map[string]cache.IndexFunc{
		PodsInformerIndexByNodeName: func(obj interface{}) ([]string, error) {
			return []string{obj.(*apiv1.Pod).Spec.NodeName}, nil
		},
	}); err != nil {
		log.Fatalf("failed to add pod informer index: %v", err)
	}

	log.Printf("starting shared informer cache")
	factory.Start(ctx.Done())
	synced := factory.WaitForCacheSync(ctx.Done())
	for t, ok := range synced {
		if !ok {
			log.Fatalf("failed to sync informer cache for type %v", t)
		}
	}
	log.Printf("shared informer cache fully synced")

	if err := prometheus.Register(timeToStartup); err != nil {
		log.Fatalf("failed to register time-to-ready metric: %v", err)
	}
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Println("serving metrics on :9090/metrics")
		if err := http.ListenAndServe(":9090", nil); err != http.ErrServerClosed {
			log.Fatalf("metrics server failed %v", err)
		}
	}()

	identity, err := os.Hostname()
	if err != nil {
		log.Fatalf("failed to determine leader election identity: %v", err)
	}
	if identity == "" {
		log.Fatal("leader election identity is empty")
	}

	leaderMetric := leaderStatus
	leaderMetric.Set(0)
	run := func(runCtx context.Context) {
		runReconciliation(
			runCtx,
			client,
			nodesInformer.GetIndexer(),
			podsInformer.GetIndexer(),
			DefaultReconciliationInterval,
		)
	}

	if !opts.leaderElect {
		log.Printf("leader election disabled")
		leaderMetric.Set(1)
		defer leaderMetric.Set(0)
		run(ctx)
		return
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      opts.leaderElectionName,
			Namespace: opts.leaderElectionNamespace,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   DefaultLeaderElectionLeaseDuration,
		RenewDeadline:   DefaultLeaderElectionRenewDeadline,
		RetryPeriod:     DefaultLeaderElectionRetryPeriod,
		ReleaseOnCancel: false,
		Name:            opts.leaderElectionName,
		Callbacks:       newLeaderCallbacks(identity, leaderMetric, run),
	})
}

func parseOptions() options {
	namespace := os.Getenv(PodNamespaceEnvironment)
	if namespace == "" {
		namespace = DefaultLeaderElectionNamespace
	}

	opts := options{}
	flag.BoolVar(&opts.leaderElect, "leader-elect", false, "Enable Kubernetes leader election before reconciling nodes.")
	flag.StringVar(&opts.leaderElectionName, "leader-election-name", DefaultLeaderElectionName, "Name of the Kubernetes Lease used for leader election.")
	flag.StringVar(&opts.leaderElectionNamespace, "leader-election-namespace", namespace, "Namespace of the Kubernetes Lease used for leader election.")
	flag.Parse()
	return opts
}

func newLeaderCallbacks(
	identity string,
	leaderMetric prometheus.Gauge,
	run func(context.Context),
) leaderelection.LeaderCallbacks {
	return leaderelection.LeaderCallbacks{
		OnStartedLeading: func(ctx context.Context) {
			log.Printf("became leader as %s", identity)
			leaderMetric.Set(1)
			defer leaderMetric.Set(0)

			run(ctx)
		},
		OnStoppedLeading: func() {
			leaderMetric.Set(0)
			log.Printf("stopped leading as %s", identity)
		},
		OnNewLeader: func(currentIdentity string) {
			if currentIdentity == identity {
				return
			}
			log.Printf("new leader elected: %s", currentIdentity)
		},
	}
}

func runReconciliation(
	ctx context.Context,
	client kubernetes.Interface,
	nodesIndexer cache.Indexer,
	podsIndexer cache.Indexer,
	reconciliationInterval time.Duration,
) {
	ticker := time.NewTicker(reconciliationInterval)
	defer ticker.Stop()

	log.Printf("reconciling node taints with daemonset pods every %s", reconciliationInterval)
	for {
		select {
		case <-ticker.C:
			reconcile(ctx, client, nodesIndexer, podsIndexer)
		case <-ctx.Done():
			return
		}
	}
}

func reconcile(
	ctx context.Context,
	client kubernetes.Interface,
	nodesIndexer cache.Indexer,
	podsIndexer cache.Indexer,
) {
	if ctx.Err() != nil {
		return
	}

	nodes := nodesIndexer.List()
	totalNodeCount.Set(float64(len(nodes)))
	for _, inode := range nodes {
		if ctx.Err() != nil {
			return
		}

		node, ok := inode.(*apiv1.Node)
		if !ok {
			continue
		}

		taintIndex := -1
		for i, taint := range node.Spec.Taints {
			if taint.Key == TaintNodeDaemonSetNotReady {
				taintIndex = i
				break
			}
		}
		if taintIndex == -1 {
			continue
		}

		pods, err := podsIndexer.ByIndex(PodsInformerIndexByNodeName, node.ObjectMeta.Name)
		if err != nil {
			continue
		}

		// only proceed if all the tolerated daemonset pods on the node are ready
		allPodsReady := true
		for _, ipod := range pods {
			pod, ok := ipod.(*apiv1.Pod)
			if !ok {
				continue
			}
			controller := metav1.GetControllerOfNoCopy(pod)
			if controller == nil || controller.Kind != "DaemonSet" {
				continue
			}
			toleratedPod := false
			for _, toleration := range pod.Spec.Tolerations {
				if toleration.Key == TaintNodeDaemonSetNotReady {
					toleratedPod = true
					break
				}
			}
			if !toleratedPod {
				continue
			}
			if !apiv1pod.IsPodReady(pod) {
				allPodsReady = false
				break
			}
		}
		if !allPodsReady {
			continue
		}

		// calculate the time here so potential slow node patching time doesn't get reflected in metrics
		nodeTimeToReady := time.Since(time.Time(node.ObjectMeta.CreationTimestamp.Time)).Seconds()

		patch := []JSONPatchOperation{
			{
				Op:   JSONPatchOperationOpRemove,
				Path: fmt.Sprintf("/spec/taints/%d", taintIndex),
			},
		}

		bytes, err := json.Marshal(patch)
		if err != nil {
			continue
		}

		err = retry.Do(
			func() error {
				_, err := client.CoreV1().Nodes().Patch(ctx, node.ObjectMeta.Name, types.JSONPatchType, bytes, metav1.PatchOptions{})
				return err
			},
			retry.Attempts(3),
			retry.Context(ctx),
		)

		if err != nil {
			continue
		}
		log.Printf("untainted node %s", node.ObjectMeta.Name)

		timeToStartup.WithLabelValues().Observe(nodeTimeToReady)
		nodesUntainted.Inc()
	}
}
