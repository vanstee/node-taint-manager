package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

func TestLeaderMetricHasNoLabels(t *testing.T) {
	leaderStatus.Set(0)

	err := testutil.CollectAndCompare(
		leaderStatus,
		strings.NewReader(`# HELP node_taint_manager_leader Whether this process currently leads node-taint-manager reconciliation.
# TYPE node_taint_manager_leader gauge
node_taint_manager_leader 0
`),
		"node_taint_manager_leader",
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLeaderCallbackBlocksUntilLeadershipContextEnds(t *testing.T) {
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_leader"})
	entered := make(chan struct{})
	returned := make(chan struct{})

	callbacks := newLeaderCallbacks("pod-a", gauge, func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		callbacks.OnStartedLeading(ctx)
		close(returned)
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("leader callback did not start reconciliation")
	}

	if got := testutil.ToFloat64(gauge); got != 1 {
		t.Fatalf("leader gauge = %v, want 1 while callback is active", got)
	}
	select {
	case <-returned:
		t.Fatal("leader callback returned before its context ended")
	default:
	}

	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("leader callback did not return after its context ended")
	}

	if got := testutil.ToFloat64(gauge); got != 0 {
		t.Fatalf("leader gauge = %v, want 0 after callback exits", got)
	}
}

func TestStoppedLeadingClearsLeaderGauge(t *testing.T) {
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_stopped_leader"})
	gauge.Set(1)

	callbacks := newLeaderCallbacks("pod-a", gauge, func(context.Context) {})
	callbacks.OnStoppedLeading()

	if got := testutil.ToFloat64(gauge); got != 0 {
		t.Fatalf("leader gauge = %v, want 0 after leadership stops", got)
	}
}

func TestReconciliationLoopStopsOnContextCancellation(t *testing.T) {
	nodesIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	podsIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		PodsInformerIndexByNodeName: func(obj interface{}) ([]string, error) {
			return nil, nil
		},
	})
	client := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	returned := make(chan struct{})
	go func() {
		runReconciliation(ctx, client, nodesIndexer, podsIndexer, time.Hour)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("reconciliation loop did not return after context cancellation")
	}

	if actions := client.Actions(); len(actions) != 0 {
		t.Fatalf("reconciliation issued %d Kubernetes actions after cancellation, want 0", len(actions))
	}
}
