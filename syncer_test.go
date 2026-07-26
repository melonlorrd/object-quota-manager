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

func TestSyncer_SyncFromQuota(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	quota := &ObjectQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns1"},
		Spec: ObjectQuotaSpec{
			FileDescriptors: int64Ptr(100),
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	mgr := NewStubManager()
	res := NewFakeResolver(42)
	logger := log.FromContext(context.Background())

	syncer := NewSyncer(cl, mgr, res, logger)
	if err := syncer.SyncFromQuota(context.Background(), quota); err != nil {
		t.Fatalf("SyncFromQuota: %v", err)
	}
}

func TestSyncer_RemoveNamespace(t *testing.T) {
	cl := fake.NewClientBuilder().Build()
	mgr := NewStubManager()
	res := NewFakeResolver(99)
	logger := log.FromContext(context.Background())

	syncer := NewSyncer(cl, mgr, res, logger)
	if err := syncer.RemoveNamespace(context.Background(), "ns1"); err != nil {
		t.Fatalf("RemoveNamespace: %v", err)
	}
}

func TestSyncer_NoFDInQuota(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	quota := &ObjectQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns1"},
		Spec:       ObjectQuotaSpec{},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	mgr := NewStubManager()
	res := NewFakeResolver(42)
	logger := log.FromContext(context.Background())

	syncer := NewSyncer(cl, mgr, res, logger)
	if err := syncer.SyncFromQuota(context.Background(), quota); err != nil {
		t.Fatalf("SyncFromQuota (no FD): %v", err)
	}
}

func TestSyncer_WithLeaser(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	quota := &ObjectQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "ns-cluster"},
		Spec: ObjectQuotaSpec{
			FileDescriptors: int64Ptr(5000),
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	mgr := NewStubManager()
	res := NewFakeResolver(123)
	logger := log.FromContext(context.Background())

	leaser := newFakeLeaseManager()
	syncer := NewSyncer(cl, mgr, res, logger).WithLeaser(leaser, "node-worker-1")

	if err := syncer.SyncFromQuota(context.Background(), quota); err != nil {
		t.Fatalf("SyncFromQuota with leaser: %v", err)
	}

	status, ok := leaser.GetStatus("ns-cluster")
	if !ok {
		t.Fatalf("expected lease status for ns-cluster")
	}
	if status.AllocatedCredits != 500 {
		t.Errorf("expected 500 leased credits to node-worker-1, got %d", status.AllocatedCredits)
	}

	lease, err := syncer.ReconcileLeaseWatermark(context.Background(), "ns-cluster", 450)
	if err != nil {
		t.Fatalf("ReconcileLeaseWatermark: %v", err)
	}
	if lease != 1000 {
		t.Errorf("expected lease to increase to 1000 on watermark hit, got %d", lease)
	}

	if err := syncer.RemoveNamespace(context.Background(), "ns-cluster"); err != nil {
		t.Fatalf("RemoveNamespace with leaser: %v", err)
	}
	status, _ = leaser.GetStatus("ns-cluster")
	if status.AllocatedCredits != 0 {
		t.Errorf("expected 0 allocated credits after node removal, got %d", status.AllocatedCredits)
	}
}

func TestSyncer_CustomLeaseBatchSize(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	quota := &ObjectQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-batch", Namespace: "ns-custom"},
		Spec: ObjectQuotaSpec{
			FileDescriptors: int64Ptr(10000),
			LeaseBatchSize:  int64Ptr(2500),
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	mgr := NewStubManager()
	res := NewFakeResolver(123)
	logger := log.FromContext(context.Background())

	leaser := newFakeLeaseManager()
	syncer := NewSyncer(cl, mgr, res, logger).WithLeaser(leaser, "node-1")

	if err := syncer.SyncFromQuota(context.Background(), quota); err != nil {
		t.Fatalf("SyncFromQuota with custom lease batch size: %v", err)
	}

	status, ok := leaser.GetStatus("ns-custom")
	if !ok || status.AllocatedCredits != 2500 {
		t.Errorf("expected 2500 leased credits, got %v", status)
	}
}

func TestSyncer_TopLevelFileDescriptors(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	quota := &ObjectQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "top-level-fd", Namespace: "ns-top"},
		Spec: ObjectQuotaSpec{
			FileDescriptors: int64Ptr(3000),
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota).Build()
	mgr := NewStubManager()
	res := NewFakeResolver(77)
	logger := log.FromContext(context.Background())

	leaser := newFakeLeaseManager()
	syncer := NewSyncer(cl, mgr, res, logger).WithLeaser(leaser, "node-1")

	if err := syncer.SyncFromQuota(context.Background(), quota); err != nil {
		t.Fatalf("SyncFromQuota top-level FD: %v", err)
	}

	status, ok := leaser.GetStatus("ns-top")
	if !ok || status.AllocatedCredits != 300 {
		t.Errorf("expected 300 allocated credits, got %v", status)
	}
}

func TestSyncer_GetTopConsumerCgroup(t *testing.T) {
	cl := fake.NewClientBuilder().Build()
	mgr := NewStubManager()
	res := NewFakeResolver(123)
	logger := log.FromContext(context.Background())

	syncer := NewSyncer(cl, mgr, res, logger)
	_, _, err := syncer.GetTopConsumerCgroup("empty-ns")
	if err == nil {
		t.Error("expected error for unregistered namespace")
	}
}

func TestCachedPodResolver(t *testing.T) {
	fakeRes := NewFakeResolver(888)
	cached := NewCachedPodResolver(fakeRes)

	infos1, err := cached.Resolve("pod-123")
	if err != nil || len(infos1) == 0 || infos1[0].CgroupID != 888 {
		t.Fatalf("unexpected resolve output: %v, %v", infos1, err)
	}

	fakeRes.ID = 999
	infos2, err := cached.Resolve("pod-123")
	if err != nil || len(infos2) == 0 || infos2[0].CgroupID != 888 {
		t.Fatalf("expected cached value 888, got: %v", infos2)
	}

	cached.Invalidate("pod-123")
	infos3, err := cached.Resolve("pod-123")
	if err != nil || len(infos3) == 0 || infos3[0].CgroupID != 999 {
		t.Fatalf("expected fresh value 999 after invalidation, got: %v", infos3)
	}
}

func int64Ptr(v int64) *int64 { return &v }

// GetTopConsumerCgroup returns the cgroup ID with the highest FD count for a namespace.
// Test-only implementation — not wired in production.
func (s *Syncer) GetTopConsumerCgroup(ns string) (uint64, uint64, error) {
	s.mu.RLock()
	cgroups, ok := s.nsCgroups[ns]
	s.mu.RUnlock()
	if !ok || len(cgroups) == 0 {
		return 0, 0, fmt.Errorf("no cgroups registered for namespace %s", ns)
	}

	if s.bpfObjs == nil {
		return 0, 0, nil
	}

	var topCgid uint64
	var maxCount uint64

	for cgid := range cgroups {
		var count uint64
		if err := s.bpfObjs.FdCgroupCounter.Lookup(cgid, &count); err == nil {
			if count >= maxCount {
				maxCount = count
				topCgid = cgid
			}
		}
	}

	return topCgid, maxCount, nil
}

// ReconcileLeaseWatermark checks current usage against the leased limit and requests
// additional credits if usage exceeds 80% of the current lease.
// Test-only implementation — not wired in production.
func (s *Syncer) ReconcileLeaseWatermark(ctx context.Context, ns string, currentUsage int64) (int64, error) {
	if s.leaser == nil {
		return 0, nil
	}

	status, ok := s.leaser.GetStatus(ns)
	if !ok {
		return 0, nil
	}

	currentLease := status.NodeLeases[s.nodeID]
	if currentLease == 0 {
		return 0, nil
	}

	if float64(currentUsage) >= 0.8*float64(currentLease) {
		granted, err := s.leaser.RequestLease(ns, s.nodeID, 0)
		if err != nil {
			return currentLease, err
		}
		nsKey := NamespaceKey(ns)
		if err := s.updateNsLimit(nsKey, granted); err != nil {
			return currentLease, fmt.Errorf("update leased limit for %s: %w", ns, err)
		}
		FDLimit.WithLabelValues(ns).Set(float64(granted))
		return granted, nil
	}

	return currentLease, nil
}
