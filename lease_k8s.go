package main

import (
	"context"
	"fmt"
	"maps"
	"time"

	coorv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const leaseDurationSeconds = 60
const renewInterval = 30 * time.Second

// K8sLeaseManager implements QuotaLeaser using ObjectQuota.status for credit
// allocations and coordination.k8s.io/v1 Lease objects for per-node heartbeats.
//
// Credit state lives in the ObjectQuota CRD status:
//
//	status:
//	  fdAllocations:
//	    node-a: 30
//	    node-b: 30
//	    available: 40
//	  procAllocations:
//	    node-a: 15
//	    node-b: 10
//	    available: 25
//	  version: 5
//
// Each node maintains a Lease object (object-quota-<ns>-<nodeID>) as a heartbeat.
// When a Lease expires (>leaseDurationSeconds since last renew), other nodes can
// reclaim that node's credits.
type K8sLeaseManager struct {
	client       client.Client
	nodeID       string
	renewPeriod  time.Duration
	defaultBatch int64
}


func NewK8sLeaseManager(c client.Client, nodeID string, defaultBatch int64) *K8sLeaseManager {
	if defaultBatch <= 0 {
		defaultBatch = 1000
	}
	return &K8sLeaseManager{
		client:       c,
		nodeID:       nodeID,
		renewPeriod:  renewInterval,
		defaultBatch: defaultBatch,
	}
}

// isProcSuffix returns true if the namespace key has the "-proc" suffix used by the
// syncer to distinguish process allocations from FD allocations within the same
// ObjectQuota CRD.
func isProcSuffix(ns string) (string, bool) {
	if len(ns) > 5 && ns[len(ns)-5:] == "-proc" {
		return ns[:len(ns)-5], true
	}
	return ns, false
}

// leaseName returns the Lease object name for a namespace.
// Each node gets its own Lease per namespace for independent heartbeats.
func (m *K8sLeaseManager) leaseName(ns string) string {
	return fmt.Sprintf("object-quota-%s-%s", ns, m.nodeID)
}

// SetGlobalPool is a no-op: pool limits are read directly from ObjectQuota.Spec
// by RequestLease, so no separate initialization is needed.
func (m *K8sLeaseManager) SetGlobalPool(ns string, totalLimit int64) {
	_ = totalLimit
}

// RequestLease reads the current ObjectQuota status, computes a credit allocation
// for this node, and updates the status. If ns ends with "-proc", it operates on
// the process allocation fields; otherwise on the FD allocation fields.
func (m *K8sLeaseManager) RequestLease(ns string, nodeID string, requested int64) (int64, error) {
	ctx := context.Background()
	baseNS, isProc := isProcSuffix(ns)

	if nodeID != m.nodeID {
		return 0, fmt.Errorf("this manager is for node %s, not %s", m.nodeID, nodeID)
	}

	var quota ObjectQuota
	if err := m.client.Get(ctx, types.NamespacedName{Namespace: baseNS, Name: baseNS}, &quota); err != nil {
		return 0, fmt.Errorf("get ObjectQuota %s/%s: %w", baseNS, baseNS, err)
	}

	if isProc {
		return m.requestProc(ctx, &quota, baseNS, nodeID, requested)
	}
	return m.requestFD(ctx, &quota, baseNS, nodeID, requested)
}

func (m *K8sLeaseManager) requestFD(ctx context.Context, quota *ObjectQuota, ns, nodeID string, requested int64) (int64, error) {
	if quota.Status.FDAllocations == nil {
		quota.Status.FDAllocations = make(map[string]int64)
	}

	if existing, ok := quota.Status.FDAllocations[nodeID]; ok && existing > 0 {
		m.ensureLease(ctx, ns)
		return existing, nil
	}

	allocated := int64(0)
	for _, v := range quota.Status.FDAllocations {
		allocated += v
	}

	totalLimit := int64(0)
	if quota.Spec.FileDescriptors != nil {
		totalLimit = *quota.Spec.FileDescriptors
	}

	if requested <= 0 {
		requested = totalLimit / 10
		if totalLimit <= 100 {
			requested = totalLimit
		}
		requested = max(requested, 10)
		requested = min(requested, m.defaultBatch)
		requested = min(requested, totalLimit)
	}

	available := totalLimit - allocated
	if available <= 0 {
		m.ensureLease(ctx, ns)
		return quota.Status.FDAllocations[nodeID], nil
	}

	grant := min(requested, available)
	quota.Status.FDAllocations[nodeID] += grant
	quota.Status.AvailableFD = totalLimit - allocated - grant
	v := incrementVersion(quota.Status.Version)
	quota.Status.Version = &v

	if err := m.client.Status().Update(ctx, quota); err != nil {
		return 0, fmt.Errorf("update ObjectQuota status %s/%s: %w", ns, ns, err)
	}

	m.ensureLease(ctx, ns)
	return quota.Status.FDAllocations[nodeID], nil
}

func (m *K8sLeaseManager) requestProc(ctx context.Context, quota *ObjectQuota, ns, nodeID string, requested int64) (int64, error) {
	if quota.Status.ProcAllocations == nil {
		quota.Status.ProcAllocations = make(map[string]int64)
	}

	if existing, ok := quota.Status.ProcAllocations[nodeID]; ok && existing > 0 {
		return existing, nil
	}

	allocated := int64(0)
	for _, v := range quota.Status.ProcAllocations {
		allocated += v
	}

	totalLimit := int64(0)
	if quota.Spec.Processes != nil {
		totalLimit = *quota.Spec.Processes
	}

	if requested <= 0 {
		requested = totalLimit / 10
		if totalLimit <= 100 {
			requested = totalLimit
		}
		requested = max(requested, 10)
		requested = min(requested, m.defaultBatch)
		requested = min(requested, totalLimit)
	}

	available := totalLimit - allocated
	if available <= 0 {
		return quota.Status.ProcAllocations[nodeID], nil
	}

	grant := min(requested, available)
	quota.Status.ProcAllocations[nodeID] += grant
	v := incrementVersion(quota.Status.Version)
	quota.Status.Version = &v

	if err := m.client.Status().Update(ctx, quota); err != nil {
		return 0, fmt.Errorf("update ObjectQuota status %s/%s: %w", ns, ns, err)
	}

	return quota.Status.ProcAllocations[nodeID], nil
}

func (m *K8sLeaseManager) ReleaseLease(ns string, nodeID string) int64 {
	ctx := context.Background()
	baseNS, isProc := isProcSuffix(ns)

	var quota ObjectQuota
	if err := m.client.Get(ctx, types.NamespacedName{Namespace: baseNS, Name: baseNS}, &quota); err != nil {
		return 0
	}

	if isProc {
		return m.releaseProc(ctx, &quota, baseNS, nodeID)
	}
	return m.releaseFD(ctx, &quota, baseNS, nodeID)
}

func (m *K8sLeaseManager) releaseFD(ctx context.Context, quota *ObjectQuota, ns, nodeID string) int64 {
	if quota.Status.FDAllocations == nil {
		return 0
	}

	leased, ok := quota.Status.FDAllocations[nodeID]
	if !ok {
		return 0
	}

	delete(quota.Status.FDAllocations, nodeID)

	totalUsed := int64(0)
	for _, v := range quota.Status.FDAllocations {
		totalUsed += v
	}
	totalLimit := int64(0)
	if quota.Spec.FileDescriptors != nil {
		totalLimit = *quota.Spec.FileDescriptors
	}
	quota.Status.AvailableFD = totalLimit - totalUsed

	v := incrementVersion(quota.Status.Version)
	quota.Status.Version = &v

	if err := m.client.Status().Update(ctx, quota); err != nil {
		return 0
	}

	m.deleteLease(ctx, ns)
	return leased
}

func (m *K8sLeaseManager) releaseProc(ctx context.Context, quota *ObjectQuota, ns, nodeID string) int64 {
	if quota.Status.ProcAllocations == nil {
		return 0
	}

	leased, ok := quota.Status.ProcAllocations[nodeID]
	if !ok {
		return 0
	}

	delete(quota.Status.ProcAllocations, nodeID)
	v := incrementVersion(quota.Status.Version)
	quota.Status.Version = &v

	if err := m.client.Status().Update(ctx, quota); err != nil {
		return 0
	}

	return leased
}

func incrementVersion(v *int64) int64 {
	if v == nil {
		return 1
	}
	return *v + 1
}

// GetStatus reads the current allocation state from the ObjectQuota CRD.
func (m *K8sLeaseManager) GetStatus(ns string) (LeaseStatus, bool) {
	ctx := context.Background()

	var quota ObjectQuota
	if err := m.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ns}, &quota); err != nil {
		return LeaseStatus{}, false
	}

	allocated := int64(0)
	nodeLeases := make(map[string]int64)
	for k, v := range quota.Status.FDAllocations {
		nodeLeases[k] = v
		allocated += v
	}

	totalLimit := int64(0)
	if quota.Spec.FileDescriptors != nil {
		totalLimit = *quota.Spec.FileDescriptors
	}

	return LeaseStatus{
		TotalLimit:       totalLimit,
		AllocatedCredits: allocated,
		NodeLeases:       nodeLeases,
	}, true
}

func (m *K8sLeaseManager) RemoveNamespace(ns string) {
	m.deleteLease(context.Background(), ns)
}

// ensureLease creates or updates the heartbeat Lease for this node in the namespace.
func (m *K8sLeaseManager) ensureLease(ctx context.Context, ns string) {
	name := m.leaseName(ns)
	now := metav1.NowMicro()
	duration := int32(leaseDurationSeconds)

	lease := &coorv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: coorv1.LeaseSpec{
			HolderIdentity:       &m.nodeID,
			LeaseDurationSeconds: &duration,
			RenewTime:            &now,
			AcquireTime:          &now,
		},
	}

	existing := &coorv1.Lease{}
	if err := m.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, existing); err != nil {
		// Create
		if createErr := m.client.Create(ctx, lease); createErr != nil {
			return
		}
		return
	}

	// Update renew time
	existing.Spec.RenewTime = &now
	_ = m.client.Update(ctx, existing)
}

func (m *K8sLeaseManager) deleteLease(ctx context.Context, ns string) {
	name := m.leaseName(ns)
	lease := &coorv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
	}
	if err := m.client.Delete(ctx, lease); err != nil {
		return
	}
}

// ReclaimExpiredLeases checks all leases in the namespace and reclaims credits from
// any node whose lease has expired. Should be called periodically by all nodes.
func (m *K8sLeaseManager) ReclaimExpiredLeases(ctx context.Context, ns string) error {
	logger := log.FromContext(ctx)

	var quota ObjectQuota
	if err := m.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: ns}, &quota); err != nil {
		return nil
	}

	leaseList := &coorv1.LeaseList{}
	if err := m.client.List(ctx, leaseList, client.InNamespace(ns)); err != nil {
		return nil
	}

	// Build map of active nodes from leases
	activeNodes := make(map[string]bool)
	now := time.Now()
	for _, lease := range leaseList.Items {
		if lease.Spec.RenewTime != nil {
			age := now.Sub(lease.Spec.RenewTime.Time)
			if age < time.Duration(leaseDurationSeconds)*time.Second {
				if lease.Spec.HolderIdentity != nil {
				activeNodes[*lease.Spec.HolderIdentity] = true
			}
			}
		}
	}

	// Remove allocations for nodes without active leases
	changed := false
	for nodeID := range quota.Status.FDAllocations {
		if nodeID == m.nodeID {
			continue // never reclaim our own
		}
		if !activeNodes[nodeID] {
			delete(quota.Status.FDAllocations, nodeID)
			changed = true
			logger.V(1).Info("reclaimed expired FD lease", "namespace", ns, "node", nodeID)
		}
	}
	for nodeID := range quota.Status.ProcAllocations {
		if nodeID == m.nodeID {
			continue
		}
		if !activeNodes[nodeID] {
			delete(quota.Status.ProcAllocations, nodeID)
			changed = true
			logger.V(1).Info("reclaimed expired proc lease", "namespace", ns, "node", nodeID)
		}
	}

	if changed {
		// Recompute available
		totalFD := int64(0)
		for _, v := range quota.Status.FDAllocations {
			totalFD += v
		}
		fdLimit := int64(0)
		if quota.Spec.FileDescriptors != nil {
			fdLimit = *quota.Spec.FileDescriptors
		}
		quota.Status.AvailableFD = fdLimit - totalFD

		v := int64(0)
		if quota.Status.Version != nil {
			v = *quota.Status.Version
		}
		v++
		quota.Status.Version = &v

		// Copy maps before update
		if quota.Status.FDAllocations != nil {
			quota.Status.FDAllocations = maps.Clone(quota.Status.FDAllocations)
		}
		if quota.Status.ProcAllocations != nil {
			quota.Status.ProcAllocations = maps.Clone(quota.Status.ProcAllocations)
		}

		if err := m.client.Status().Update(ctx, &quota); err != nil {
			return fmt.Errorf("update reclaimed status: %w", err)
		}
	}

	return nil
}

// StartLeaseRenewal runs a background loop that periodically renews the heartbeat
// Lease for each namespace this node has credits in. It also calls ReclaimExpiredLeases
// on each tick to detect dead nodes.
func (m *K8sLeaseManager) StartLeaseRenewal(ctx context.Context) {
	logger := log.FromContext(ctx)
	ticker := time.NewTicker(m.renewPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// List all ObjectQuota CRDs in all namespaces
			var list ObjectQuotaList
			if err := m.client.List(ctx, &list); err != nil {
				logger.V(1).Info("lease renewal: list quotas failed", "error", err)
				continue
			}

			for _, quota := range list.Items {
				ns := quota.Namespace

				// Renew this node's heartbeat lease
				m.ensureLease(ctx, ns)

				// Reclaim expired leases from dead nodes
				if err := m.ReclaimExpiredLeases(ctx, ns); err != nil {
					logger.V(1).Info("lease renewal: reclaim failed", "namespace", ns, "error", err)
				}
			}
		}
	}
}
