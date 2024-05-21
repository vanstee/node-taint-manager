package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"time"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	apiv1pod "k8s.io/kubernetes/pkg/api/v1/pod"
)

const (
	DefaultResyncInterval         = 10 * time.Minute
	DefaultReconciliationInterval = 5 * time.Second

	PodsInformerIndexByNodeName = "ByNodeName"

	TaintNodeDaemonSetNotReady = "node.vanstee.github.io/daemonset-not-ready"

	JSONPatchOperationOpRemove = "remove"
)

type JSONPatchOperation struct {
	Op   string `json:"op"`
	Path string `json:"path"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
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
						Name: t.ObjectMeta.Name,
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
						NodeName: t.Spec.NodeName,
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
	podsInformer.AddIndexers(map[string]cache.IndexFunc{
		PodsInformerIndexByNodeName: func(obj interface{}) ([]string, error) {
			return []string{obj.(*apiv1.Pod).Spec.NodeName}, nil
		},
	})

	log.Printf("starting shared informer cache")
	factory.Start(ctx.Done())
	synced := factory.WaitForCacheSync(ctx.Done())
	for t, ok := range synced {
		if !ok {
			log.Fatalf("failed to sync informer cache for type %v", t)
		}
	}
	log.Printf("shared informer cache fully synced")

	reconciliationInterval := DefaultReconciliationInterval
	ticker := time.NewTicker(reconciliationInterval)
	log.Printf("reconciling node taints with daemonset pods every %d", reconciliationInterval)

	// TODO: consider selecting a channel of events from informer, or use more
	// custom watch implementation to speed things up (and save memory)
	for {
		select {
		case <-ticker.C:
			for _, inode := range nodesInformer.GetIndexer().List() {
				node, ok := inode.(*apiv1.Node)
				if !ok {
					continue
				}

				hasMatchingTaints := false
				for _, taint := range node.Spec.Taints {
					if taint.Key == TaintNodeDaemonSetNotReady {
						hasMatchingTaints = true
						break
					}
				}
				if !hasMatchingTaints {
					continue
				}

				pods, err := podsInformer.GetIndexer().ByIndex(PodsInformerIndexByNodeName, node.ObjectMeta.Name)
				if err != nil {
					continue
				}

				taintsToRemove := []int{}
				for _, ipod := range pods {
					pod, ok := ipod.(*apiv1.Pod)
					if !ok {
						continue
					}
					if !apiv1pod.IsPodReady(pod) {
						continue
					}
					controller := metav1.GetControllerOfNoCopy(pod)
					if controller == nil || controller.Kind != "DaemonSet" {
						continue
					}

					// TODO: build map of taints with original index to avoid inner loop
					value := pod.ObjectMeta.Namespace + "." + controller.Name
					for i, taint := range node.Spec.Taints {
						if taint.Key != TaintNodeDaemonSetNotReady || taint.Value != value {
							continue
						}

						taintsToRemove = append(taintsToRemove, i)
						// continue to remove any additional taints with the same key
					}
				}

				if len(taintsToRemove) == 0 {
					continue
				}

				// use reverse order to avoid indexing problems with multiple jsonpatch
				// remove operations
				sort.Sort(sort.Reverse(sort.IntSlice(taintsToRemove)))

				patch := make([]JSONPatchOperation, len(taintsToRemove))
				for i, t := range taintsToRemove {
					log.Printf("removing taint %s from node %s", node.Spec.Taints[i].ToString(), node.ObjectMeta.Name)
					patch[i] = JSONPatchOperation{
						Op:   JSONPatchOperationOpRemove,
						Path: fmt.Sprintf("/spec/taints/%d", t),
					}
				}

				bytes, err := json.Marshal(patch)
				if err != nil {
					continue
				}

				// TODO: retry on conflict
				if _, err = client.CoreV1().Nodes().Patch(ctx, node.ObjectMeta.Name, types.JSONPatchType, bytes, metav1.PatchOptions{}); err != nil {
					continue
				}
			}
		case <-ctx.Done():
			break
		}
	}
}
