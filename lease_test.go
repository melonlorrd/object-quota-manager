package main

import (
	"fmt"
	"maps"
	"sync"
	"testing"
)

// fakeLeaseManager implements QuotaLeaser in-memory for unit tests.
// It mirrors the interface contract without needing a Kubernetes API server.
type fakeLeaseManager struct {
	mu    sync.Mutex
	pools map[string]*fakePool
}

type fakePool struct {
	totalLimit       int64
	allocatedCredits int64
	nodeLeases       map[string]int64
}

func newFakeLeaseManager() *fakeLeaseManager {
	return &fakeLeaseManager{
		pools: make(map[string]*fakePool),
	}
}

func (f *fakeLeaseManager) SetGlobalPool(ns string, totalLimit int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pools[ns]
	if !ok {
		f.pools[ns] = &fakePool{
			totalLimit: totalLimit,
			nodeLeases: make(map[string]int64),
		}
		return
	}
	p.totalLimit = totalLimit
}

func (f *fakeLeaseManager) RequestLease(ns string, nodeID string, requested int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p, ok := f.pools[ns]
	if !ok {
		return 0, fmt.Errorf("no global quota pool for namespace %s", ns)
	}

	if requested <= 0 {
		requested = p.totalLimit / 10
		if p.totalLimit <= 100 {
			requested = p.totalLimit
		}
		requested = max(requested, 10)
		requested = min(requested, int64(1000))
		requested = min(requested, p.totalLimit)
	}

	remaining := p.totalLimit - p.allocatedCredits
	if remaining <= 0 {
		return p.nodeLeases[nodeID], nil
	}

	grant := min(requested, remaining)
	p.allocatedCredits += grant
	p.nodeLeases[nodeID] += grant

	return p.nodeLeases[nodeID], nil
}

func (f *fakeLeaseManager) ReleaseLease(ns string, nodeID string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	p, ok := f.pools[ns]
	if !ok {
		return 0
	}

	leased, ok := p.nodeLeases[nodeID]
	if !ok {
		return 0
	}

	delete(p.nodeLeases, nodeID)
	p.allocatedCredits = max(0, p.allocatedCredits-leased)
	return leased
}

func (f *fakeLeaseManager) GetStatus(ns string) (LeaseStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p, ok := f.pools[ns]
	if !ok {
		return LeaseStatus{}, false
	}

	copyLeases := make(map[string]int64, len(p.nodeLeases))
	maps.Copy(copyLeases, p.nodeLeases)

	return LeaseStatus{
		TotalLimit:       p.totalLimit,
		AllocatedCredits: p.allocatedCredits,
		NodeLeases:       copyLeases,
	}, true
}

func (f *fakeLeaseManager) RemoveNamespace(ns string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pools, ns)
}

func TestLeaserInterface_BasicLeasing(t *testing.T) {
	mgr := newFakeLeaseManager()
	ns := "prod"

	mgr.SetGlobalPool(ns, 2000)

	granted1, err := mgr.RequestLease(ns, "node-1", 500)
	if err != nil {
		t.Fatalf("RequestLease node-1: %v", err)
	}
	if granted1 != 500 {
		t.Errorf("expected 500 granted to node-1, got %d", granted1)
	}

	granted2, err := mgr.RequestLease(ns, "node-2", 1000)
	if err != nil {
		t.Fatalf("RequestLease node-2: %v", err)
	}
	if granted2 != 1000 {
		t.Errorf("expected 1000 granted to node-2, got %d", granted2)
	}

	status, ok := mgr.GetStatus(ns)
	if !ok {
		t.Fatalf("expected status for %s", ns)
	}
	if status.AllocatedCredits != 1500 {
		t.Errorf("expected 1500 allocated credits, got %d", status.AllocatedCredits)
	}

	granted3, err := mgr.RequestLease(ns, "node-3", 1000)
	if err != nil {
		t.Fatalf("RequestLease node-3: %v", err)
	}
	if granted3 != 500 {
		t.Errorf("expected 500 remaining granted to node-3, got %d", granted3)
	}

	status, _ = mgr.GetStatus(ns)
	if status.AllocatedCredits != 2000 {
		t.Errorf("expected 2000 allocated credits (fully leased), got %d", status.AllocatedCredits)
	}

	mgr.ReleaseLease(ns, "node-1")
	status, _ = mgr.GetStatus(ns)
	if status.AllocatedCredits != 1500 {
		t.Errorf("expected 1500 allocated credits after releasing node-1, got %d", status.AllocatedCredits)
	}
}

func TestLeaserInterface_NoPoolError(t *testing.T) {
	mgr := newFakeLeaseManager()
	_, err := mgr.RequestLease("non-existent", "node-1", 100)
	if err == nil {
		t.Error("expected error requesting lease for non-existent pool")
	}
}
