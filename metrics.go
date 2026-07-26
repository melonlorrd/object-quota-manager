package main

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	FDCurrent = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "quota_fd_current",
			Help: "Current file descriptor count per namespace",
		},
		[]string{"namespace"},
	)

	FDRejected = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "quota_fd_rejected_total",
			Help: "Total rejected file descriptor allocations per namespace",
		},
		[]string{"namespace"},
	)

	FDLimit = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "quota_fd_limit",
			Help: "Configured file descriptor limit per namespace",
		},
		[]string{"namespace"},
	)

	QuotaReconciles = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "quota_reconciles_total",
			Help: "Total number of ObjectQuota reconciles",
		},
		[]string{"namespace"},
	)

	ProcCurrent = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "quota_proc_current",
			Help: "Current active process count per namespace",
		},
		[]string{"namespace"},
	)

	ProcRejected = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "quota_proc_rejected_total",
			Help: "Total rejected process creations per namespace",
		},
		[]string{"namespace"},
	)

	ProcLimit = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "quota_proc_limit",
			Help: "Configured process limit per namespace",
		},
		[]string{"namespace"},
	)
)

func RegisterMetrics() {
	ctrlmetrics.Registry.MustRegister(FDCurrent, FDRejected, FDLimit, QuotaReconciles, ProcCurrent, ProcRejected, ProcLimit)
}
