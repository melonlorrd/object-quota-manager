package main

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func BenchmarkNamespaceKey(b *testing.B) {
	ns := "kube-system-payments-processing-service"
	for b.Loop() {
		_ = NamespaceKey(ns)
	}
}

func BenchmarkLeaseManager_SingleNamespace(b *testing.B) {
	mgr := newFakeLeaseManager()
	mgr.SetGlobalPool("test-ns", 100000)

	for b.Loop() {
		_, _ = mgr.RequestLease("test-ns", "node-1", 100)
	}
}

func BenchmarkLeaseManager_Concurrent(b *testing.B) {
	mgr := newFakeLeaseManager()
	for i := 0; i < 100; i++ {
		mgr.SetGlobalPool(fmt.Sprintf("ns-%d", i), 100000)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			ns := fmt.Sprintf("ns-%d", i%100)
			node := fmt.Sprintf("node-%d", i%10)
			_, _ = mgr.RequestLease(ns, node, 50)
			i++
		}
	})
}

func BenchmarkSyncer_SyncFromQuota(b *testing.B) {
	scheme := runtime.NewScheme()
	_ = AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	quota := &ObjectQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "bench", Namespace: "bench-ns"},
		Spec: ObjectQuotaSpec{
			FileDescriptors: int64Ptr(10000),
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	mgr := NewStubManager()
	res := NewFakeResolver(42)
	logger := log.FromContext(context.Background())

	syncer := NewSyncer(cl, mgr, res, logger)
	ctx := context.Background()

	for b.Loop() {
		_ = syncer.SyncFromQuota(ctx, quota)
	}
}
