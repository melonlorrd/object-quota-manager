package main

type LeaseStatus struct {
	TotalLimit       int64
	AllocatedCredits int64
	NodeLeases       map[string]int64
}

type QuotaLeaser interface {
	SetGlobalPool(ns string, totalLimit int64)
	RequestLease(ns string, nodeID string, requested int64) (int64, error)
	ReleaseLease(ns string, nodeID string) int64
	GetStatus(ns string) (LeaseStatus, bool)
	RemoveNamespace(ns string)
}
