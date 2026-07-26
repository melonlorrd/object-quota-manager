package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	ciliumebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/melonlorrd/object-quota-manager/ebpf"
)

type Syncer struct {
	client    client.Client
	ebpfMgr   syncManager
	cgroupRes CgroupResolver
	logger    logr.Logger
	bpfObjs   *ebpf.BpfObjects
	allocLink link.Link
	closeLink link.Link
	cloneLink link.Link
	exitLink  link.Link
	mu            sync.RWMutex
	nsCgroups     map[string]map[uint64]bool
	cgroupPaths   map[uint64]string
	nsNameByKey   map[uint64]string
	ringbufReader *ringbuf.Reader
	breachCtx     context.Context
	breachCancel  context.CancelFunc
	leaser        QuotaLeaser
	nodeID        string
}

type syncManager interface {
	UpdateElement(kind MapKind, key, value any) error
	DeleteElement(kind MapKind, key any) error
}

func NewSyncer(cl client.Client, mgr syncManager, res CgroupResolver, log logr.Logger) *Syncer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Syncer{
		client:       cl,
		ebpfMgr:      mgr,
		cgroupRes:    res,
		logger:       log,
		nsCgroups:    make(map[string]map[uint64]bool),
		cgroupPaths:  make(map[uint64]string),
		nsNameByKey:  make(map[uint64]string),
		breachCtx:    ctx,
		breachCancel: cancel,
		nodeID:       "node-local",
	}
}

func (s *Syncer) WithLeaser(leaser QuotaLeaser, nodeID string) *Syncer {
	s.leaser = leaser
	if nodeID != "" {
		s.nodeID = nodeID
	}
	return s
}

func (s *Syncer) SyncNamespace(ctx context.Context, ns string, limit int64, batchSize ...int64) error {
	nsKey := NamespaceKey(ns)

	effectiveLimit := limit
	if s.leaser != nil {
		s.leaser.SetGlobalPool(ns, limit)
		var req int64
		if len(batchSize) > 0 {
			req = batchSize[0]
		}
		granted, err := s.leaser.RequestLease(ns, s.nodeID, req)
		if err == nil && granted > 0 {
			effectiveLimit = granted
		}
	}

	if err := s.updateNsLimit(nsKey, effectiveLimit); err != nil {
		return fmt.Errorf("update namespace limit for %s: %w", ns, err)
	}

	if err := s.resetNsCounter(nsKey, effectiveLimit); err != nil {
		return fmt.Errorf("reset namespace counter for %s: %w", ns, err)
	}

	known := make(map[uint64]bool)

	var pods corev1.PodList
	if err := s.client.List(ctx, &pods, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("list pods in %s: %w", ns, err)
	}

	s.mu.Lock()
	s.nsNameByKey[nsKey] = ns
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		cInfos, err := s.cgroupRes.Resolve(string(pod.UID))
		if err != nil {
			s.logger.V(1).Info("cgroup resolve skipped", "pod", pod.Name, "err", err)
			continue
		}

		for _, cInfo := range cInfos {
			if err := s.updateCgroupMapping(cInfo.CgroupID, nsKey); err != nil {
				s.mu.Unlock()
				return fmt.Errorf("update cgroup mapping for %d: %w", cInfo.CgroupID, err)
			}
			known[cInfo.CgroupID] = true
			s.cgroupPaths[cInfo.CgroupID] = cInfo.CgroupPath
		}
	}

	s.nsCgroups[ns] = known
	s.mu.Unlock()
	FDLimit.WithLabelValues(ns).Set(float64(effectiveLimit))

	if s.bpfObjs != nil {
		var lease ebpf.BpfCpuLease
		if err := s.bpfObjs.FdNsCounter.Lookup(nsKey, &lease); err == nil {
			FDCurrent.WithLabelValues(ns).Set(float64(lease.Count))
		}
	}

	return nil
}

func (s *Syncer) SyncProcessNamespace(ctx context.Context, ns string, limit int64, batchSize ...int64) error {
	nsKey := NamespaceKey(ns)

	effectiveLimit := limit
	if s.leaser != nil {
		s.leaser.SetGlobalPool(ns+"-proc", limit)
		var req int64
		if len(batchSize) > 0 {
			req = batchSize[0]
		}
		granted, err := s.leaser.RequestLease(ns+"-proc", s.nodeID, req)
		if err == nil && granted > 0 {
			effectiveLimit = granted
		}
	}

	if err := s.updateProcNsLimit(nsKey, effectiveLimit); err != nil {
		return fmt.Errorf("update process namespace limit for %s: %w", ns, err)
	}

	if err := s.resetProcNsCounter(nsKey, effectiveLimit); err != nil {
		return fmt.Errorf("reset process namespace counter for %s: %w", ns, err)
	}

	known := make(map[uint64]bool)

	var pods corev1.PodList
	if err := s.client.List(ctx, &pods, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("list pods in %s: %w", ns, err)
	}

	s.mu.Lock()
	s.nsNameByKey[nsKey] = ns
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		cInfos, err := s.cgroupRes.Resolve(string(pod.UID))
		if err != nil {
			s.logger.V(1).Info("cgroup resolve skipped", "pod", pod.Name, "err", err)
			continue
		}

		for _, cInfo := range cInfos {
			if err := s.updateProcCgroupMapping(cInfo.CgroupID, nsKey); err != nil {
				s.mu.Unlock()
				return fmt.Errorf("update proc cgroup mapping for %d: %w", cInfo.CgroupID, err)
			}
			known[cInfo.CgroupID] = true
			s.cgroupPaths[cInfo.CgroupID] = cInfo.CgroupPath
		}
	}

	s.nsCgroups[ns] = known
	s.mu.Unlock()
	ProcLimit.WithLabelValues(ns).Set(float64(effectiveLimit))

	if s.bpfObjs != nil {
		var lease ebpf.BpfCpuLease
		if err := s.bpfObjs.ProcNsCounter.Lookup(nsKey, &lease); err == nil {
			ProcCurrent.WithLabelValues(ns).Set(float64(lease.Count))
		}
	}

	return nil
}

func (s *Syncer) RemoveNamespace(ctx context.Context, ns string) error {
	nsKey := NamespaceKey(ns)

	if s.leaser != nil {
		s.leaser.ReleaseLease(ns, s.nodeID)
		s.leaser.ReleaseLease(ns+"-proc", s.nodeID)
	}

	if err := s.deleteNsLimit(nsKey); err != nil {
		s.logger.V(1).Info("delete namespace limit", "ns", ns, "err", err)
	}
	if err := s.deleteNsCounter(nsKey); err != nil {
		s.logger.V(1).Info("delete namespace counter", "ns", ns, "err", err)
	}

	if err := s.deleteProcNsLimit(nsKey); err != nil {
		s.logger.V(1).Info("delete proc namespace limit", "ns", ns, "err", err)
	}
	if err := s.deleteProcNsCounter(nsKey); err != nil {
		s.logger.V(1).Info("delete proc namespace counter", "ns", ns, "err", err)
	}

	s.mu.Lock()
	if cgroups, ok := s.nsCgroups[ns]; ok {
		for cgid := range cgroups {
			if err := s.deleteCgroupMapping(cgid); err != nil {
				s.logger.V(1).Info("delete cgroup mapping", "cgid", cgid, "err", err)
			}
			if err := s.deleteProcCgroupMapping(cgid); err != nil {
				s.logger.V(1).Info("delete proc cgroup mapping", "cgid", cgid, "err", err)
			}
			delete(s.cgroupPaths, cgid)
		}
		delete(s.nsCgroups, ns)
	}
	delete(s.nsNameByKey, nsKey)
	s.mu.Unlock()

	FDLimit.DeleteLabelValues(ns)
	ProcLimit.DeleteLabelValues(ns)
	return nil
}

func (s *Syncer) updateNsLimit(nsKey uint64, limit int64) error {
	if s.bpfObjs != nil {
		return s.bpfObjs.FdNsLimit.Update(nsKey, limit, ciliumebpf.UpdateAny)
	}
	return s.ebpfMgr.UpdateElement(MapFDNsLimit, nsKey, limit)
}

func (s *Syncer) deleteNsLimit(nsKey uint64) error {
	if s.bpfObjs != nil {
		return s.bpfObjs.FdNsLimit.Delete(nsKey)
	}
	return s.ebpfMgr.DeleteElement(MapFDNsLimit, nsKey)
}

func (s *Syncer) resetNsCounter(nsKey uint64, limit int64) error {
	microSlice := uint64(limit)
	if limit > 100 {
		microSlice = uint64(limit / 10)
	}

	if s.bpfObjs != nil {
		var existing ebpf.BpfCpuLease
		err := s.bpfObjs.FdNsCounter.Lookup(nsKey, &existing)
		if err == nil {
			existing.LeaseBudget = microSlice
			return s.bpfObjs.FdNsCounter.Update(nsKey, existing, ciliumebpf.UpdateAny)
		}
		return s.bpfObjs.FdNsCounter.Update(nsKey, ebpf.BpfCpuLease{
			Count:       0,
			LeaseBudget: microSlice,
		}, ciliumebpf.UpdateAny)
	}
	return s.ebpfMgr.UpdateElement(MapFDNsCounter, nsKey, uint64(0))
}

func (s *Syncer) deleteNsCounter(nsKey uint64) error {
	if s.bpfObjs != nil {
		return s.bpfObjs.FdNsCounter.Delete(nsKey)
	}
	return s.ebpfMgr.DeleteElement(MapFDNsCounter, nsKey)
}

func (s *Syncer) updateCgroupMapping(cgroupID uint64, nsKey uint64) error {
	if s.bpfObjs != nil {
		return s.bpfObjs.FdCgroup.Update(cgroupID, nsKey, ciliumebpf.UpdateAny)
	}
	return s.ebpfMgr.UpdateElement(MapFDCgroup, cgroupID, nsKey)
}

func (s *Syncer) deleteCgroupMapping(cgroupID uint64) error {
	if s.bpfObjs != nil {
		_ = s.bpfObjs.FdCgroupCounter.Delete(cgroupID)
		return s.bpfObjs.FdCgroup.Delete(cgroupID)
	}
	return s.ebpfMgr.DeleteElement(MapFDCgroup, cgroupID)
}

func (s *Syncer) updateProcNsLimit(nsKey uint64, limit int64) error {
	if s.bpfObjs != nil {
		return s.bpfObjs.ProcNsLimit.Update(nsKey, limit, ciliumebpf.UpdateAny)
	}
	return s.ebpfMgr.UpdateElement(MapProcNsLimit, nsKey, limit)
}

func (s *Syncer) deleteProcNsLimit(nsKey uint64) error {
	if s.bpfObjs != nil {
		return s.bpfObjs.ProcNsLimit.Delete(nsKey)
	}
	return s.ebpfMgr.DeleteElement(MapProcNsLimit, nsKey)
}

func (s *Syncer) resetProcNsCounter(nsKey uint64, limit int64) error {
	microSlice := uint64(limit)
	if limit > 100 {
		microSlice = uint64(limit / 10)
	}

	if s.bpfObjs != nil {
		var existing ebpf.BpfCpuLease
		err := s.bpfObjs.ProcNsCounter.Lookup(nsKey, &existing)
		if err == nil {
			existing.LeaseBudget = microSlice
			return s.bpfObjs.ProcNsCounter.Update(nsKey, existing, ciliumebpf.UpdateAny)
		}
		return s.bpfObjs.ProcNsCounter.Update(nsKey, ebpf.BpfCpuLease{
			Count:       0,
			LeaseBudget: microSlice,
		}, ciliumebpf.UpdateAny)
	}
	return s.ebpfMgr.UpdateElement(MapProcNsCounter, nsKey, uint64(0))
}

func (s *Syncer) deleteProcNsCounter(nsKey uint64) error {
	if s.bpfObjs != nil {
		return s.bpfObjs.ProcNsCounter.Delete(nsKey)
	}
	return s.ebpfMgr.DeleteElement(MapProcNsCounter, nsKey)
}

func (s *Syncer) updateProcCgroupMapping(cgroupID uint64, nsKey uint64) error {
	if s.bpfObjs != nil {
		return s.bpfObjs.ProcCgroup.Update(cgroupID, nsKey, ciliumebpf.UpdateAny)
	}
	return s.ebpfMgr.UpdateElement(MapProcCgroup, cgroupID, nsKey)
}

func (s *Syncer) deleteProcCgroupMapping(cgroupID uint64) error {
	if s.bpfObjs != nil {
		_ = s.bpfObjs.ProcCgroupCounter.Delete(cgroupID)
		return s.bpfObjs.ProcCgroup.Delete(cgroupID)
	}
	return s.ebpfMgr.DeleteElement(MapProcCgroup, cgroupID)
}

func (s *Syncer) LoadBPF() error {
	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("remove memlock rlimit: %w", err)
	}

	objs := ebpf.BpfObjects{}
	if err := ebpf.LoadBpfObjects(&objs, nil); err != nil {
		return fmt.Errorf("load bpf: %w", err)
	}

	installLink, err := link.AttachTracing(link.TracingOptions{
		Program:    objs.TrackInstallFd,
		AttachType: ciliumebpf.AttachTraceFEntry,
	})
	if err != nil {
		objs.Close()
		return fmt.Errorf("attach fentry fd_install: %w", err)
	}

	closeLink, err := link.AttachTracing(link.TracingOptions{
		Program:    objs.TrackCloseFd,
		AttachType: ciliumebpf.AttachTraceFEntry,
	})
	if err != nil {
		installLink.Close()
		objs.Close()
		return fmt.Errorf("attach fentry close_fd: %w", err)
	}

	cloneLink, _ := link.AttachTracing(link.TracingOptions{
		Program:    objs.TrackClone,
		AttachType: ciliumebpf.AttachTraceFEntry,
	})

	exitLink, _ := link.AttachTracing(link.TracingOptions{
		Program:    objs.TrackExit,
		AttachType: ciliumebpf.AttachTraceFEntry,
	})

	s.bpfObjs = &objs
	s.allocLink = installLink
	s.closeLink = closeLink
	s.cloneLink = cloneLink
	s.exitLink = exitLink
	return nil
}

func (s *Syncer) Close() {
	s.breachCancel()

	if s.ringbufReader != nil {
		s.ringbufReader.Close()
	}

	if s.allocLink != nil {
		s.allocLink.Close()
	}
	if s.closeLink != nil {
		s.closeLink.Close()
	}
	if s.cloneLink != nil {
		s.cloneLink.Close()
	}
	if s.exitLink != nil {
		s.exitLink.Close()
	}
	if s.bpfObjs != nil {
		s.bpfObjs.Close()
	}
}

func (s *Syncer) SyncNamespaceFromQuota(ctx context.Context, ns string) error {
	var list ObjectQuotaList
	if err := s.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("list quotas in %s: %w", ns, err)
	}
	for _, q := range list.Items {
		_ = s.SyncFromQuota(ctx, &q)
	}
	return nil
}

func (s *Syncer) SyncFromQuota(ctx context.Context, quota *ObjectQuota) error {
	var batchSize int64
	if quota.Spec.LeaseBatchSize != nil {
		batchSize = *quota.Spec.LeaseBatchSize
	}

	var hasLimit bool

	if quota.Spec.FileDescriptors != nil {
		fdLimit := *quota.Spec.FileDescriptors
		hasLimit = true
		s.logger.Info("syncing FD limit", "ns", quota.Namespace, "limit", fdLimit, "leaseBatchSize", batchSize)
		if err := s.SyncNamespace(ctx, quota.Namespace, fdLimit, batchSize); err != nil {
			return err
		}
	}

	if quota.Spec.Processes != nil {
		procLimit := *quota.Spec.Processes
		hasLimit = true
		s.logger.Info("syncing Process limit", "ns", quota.Namespace, "limit", procLimit, "leaseBatchSize", batchSize)
		if err := s.SyncProcessNamespace(ctx, quota.Namespace, procLimit, batchSize); err != nil {
			return err
		}
	}

	if !hasLimit {
		s.logger.V(1).Info("no FD or Process limit in quota, cleaning up", "ns", quota.Namespace)
		return s.RemoveNamespace(ctx, quota.Namespace)
	}

	return nil
}

func (s *Syncer) handleBreachEvent(evt ebpf.BpfBreachEvent) {
	s.logger.Info("breach event received",
		"ns_key", evt.NsKey, "cgid", evt.Cgid,
		"pid", evt.Pid, "reason", evt.Reason)

	s.mu.RLock()
	ns, ok := s.nsNameByKey[evt.NsKey]
	s.mu.RUnlock()
	if !ok {
		s.logger.Info("breach: ns not found for key", "ns_key", evt.NsKey)
		return
	}

	if evt.Reason == 0 {
		FDRejected.WithLabelValues(ns).Inc()
	} else {
		ProcRejected.WithLabelValues(ns).Inc()
	}

	if s.bpfObjs == nil {
		return
	}

	topCgid := evt.TopCgid

	if topCgid == 0 || topCgid == evt.Cgid {
		if topCgid != 0 {
			s.logger.Info("breach: top consumer was the trigger, already killed by eBPF",
				"ns", ns, "cgid", topCgid, "reason", evt.Reason)
		} else {
			s.logger.Info("breach: no top consumer tracked",
				"ns", ns, "trigger_cgid", evt.Cgid)
		}
		return
	}

	s.mu.RLock()
	path, ok := s.cgroupPaths[topCgid]
	s.mu.RUnlock()
	if !ok {
		s.logger.Info("breach: path not found for top consumer",
			"ns", ns, "top_cgid", topCgid)
		return
	}

	s.logger.Info("killing top consumer cgroup",
		"ns", ns, "cgid", topCgid, "path", path,
		"reason", evt.Reason)

	if err := KillCgroupProcs(path); err != nil {
		s.logger.Error(err, "kill cgroup procs", "cgid", topCgid, "path", path)
	}
}

func (s *Syncer) StartBreachWatcher() error {
	if s.bpfObjs == nil {
		return nil
	}

	rd, err := ringbuf.NewReader(s.bpfObjs.Events)
	if err != nil {
		return fmt.Errorf("ringbuf reader: %w", err)
	}
	s.ringbufReader = rd

	go func() {
		s.logger.Info("breach watcher started")
		for {
			record, err := rd.Read()
			if err != nil {
				if s.breachCtx.Err() != nil {
					return
				}
				s.logger.V(1).Info("ringbuf read error", "error", err)
				continue
			}

			if len(record.RawSample) < 32 {
				continue
			}

			var evt ebpf.BpfBreachEvent
			if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &evt); err != nil {
				continue
			}

			s.logger.V(1).Info("breach event",
				"ns_key", evt.NsKey, "cgid", evt.Cgid,
				"pid", evt.Pid, "reason", evt.Reason)

			s.handleBreachEvent(evt)
		}
	}()

	return nil
}

func (s *Syncer) StartCgroupScanner(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.scanAndMapCgroups(ctx)
			}
		}
	}()
}

func (s *Syncer) scanAndMapCgroups(ctx context.Context) {
	if s.bpfObjs == nil {
		return
	}

	s.mu.RLock()
	tracked := make(map[string][]uint64)
	for ns, cgroups := range s.nsCgroups {
		ids := make([]uint64, 0, len(cgroups))
		for cgid := range cgroups {
			ids = append(ids, cgid)
		}
		tracked[ns] = ids
	}
	s.mu.RUnlock()

	if len(tracked) == 0 {
		return
	}

	root := filepath.Join(CgroupFsRoot, "kubepods.slice")
	podDirs := findPodCgroupDirs(root)
	if len(podDirs) == 0 {
		return
	}

	for ns, existingIDs := range tracked {
		existing := make(map[uint64]bool, len(existingIDs))
		for _, id := range existingIDs {
			existing[id] = true
		}

		nsKey := NamespaceKey(ns)

		for _, podDir := range podDirs {
			podCgInfo, _ := readCgroupID(podDir)
			if podCgInfo == nil {
				continue
			}
			if existing[podCgInfo.CgroupID] {
				continue
			}

			uid, ok := PodIDFromCgroupPath(podDir)
			if !ok {
				continue
			}

			matched := false
			var pods corev1.PodList
			if err := s.client.List(ctx, &pods, client.InNamespace(ns)); err != nil {
				continue
			}
			for _, p := range pods.Items {
				podUID := strings.ReplaceAll(uid, "_", "-")
				if string(p.UID) != podUID {
					continue
				}
				matched = true
				break
			}
			if !matched {
				continue
			}

			cInfos := []*CgroupInfo{podCgInfo}
			children, _ := os.ReadDir(podDir)
			for _, child := range children {
				if !child.IsDir() {
					continue
				}
				childInfo, _ := readCgroupID(filepath.Join(podDir, child.Name()))
				if childInfo != nil {
					cInfos = append(cInfos, childInfo)
				}
			}

			s.mu.Lock()
			for _, cInfo := range cInfos {
				if existing[cInfo.CgroupID] {
					continue
				}
				if err := s.updateCgroupMapping(cInfo.CgroupID, nsKey); err != nil {
					s.logger.V(1).Info("scanner: map cgroup", "cgid", cInfo.CgroupID, "err", err)
					continue
				}
				if err := s.updateProcCgroupMapping(cInfo.CgroupID, nsKey); err != nil {
					s.logger.V(1).Info("scanner: map proc cgroup", "cgid", cInfo.CgroupID, "err", err)
					continue
				}
				s.nsCgroups[ns][cInfo.CgroupID] = true
				s.cgroupPaths[cInfo.CgroupID] = cInfo.CgroupPath
				existing[cInfo.CgroupID] = true
			}
			s.mu.Unlock()
		}
	}
}

func findPodCgroupDirs(root string) []string {
	var result []string
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name())
		if _, ok := PodIDFromCgroupPath(e.Name()); ok {
			result = append(result, path)
		} else {
			result = append(result, findPodCgroupDirs(path)...)
		}
	}
	return result
}
