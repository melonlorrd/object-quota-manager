package main

import (
	"context"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	events "k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const sigKillExitCode int32 = 137

type ObjectQuotaReconciler struct {
	client.Client
	FDSyncer *Syncer
}

func (r *ObjectQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var quota ObjectQuota
	if err := r.Get(ctx, req.NamespacedName, &quota); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("ObjectQuota deleted, cleaning up", "namespace", req.Namespace)
			if err := r.FDSyncer.RemoveNamespace(ctx, req.Namespace); err != nil {
				logger.Error(err, "cleanup after quota deletion failed")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling ObjectQuota",
		"name", quota.Name,
		"namespace", quota.Namespace,
	)

	QuotaReconciles.WithLabelValues(quota.Namespace).Inc()

	if err := r.FDSyncer.SyncFromQuota(ctx, &quota); err != nil {
		logger.Error(err, "fd sync failed")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ObjectQuotaReconciler) SetupWithManager(mgr ctrl.Manager, logger logr.Logger) error {
	logger.Info("setting up ObjectQuota controller")
	return ctrl.NewControllerManagedBy(mgr).
		For(&ObjectQuota{}).
		Complete(r)
}

type PodReconciler struct {
	client.Client
	FDSyncer *Syncer
	Recorder events.EventRecorder
}

func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if (pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending) && r.FDSyncer != nil {
		if cached, ok := r.FDSyncer.cgroupRes.(*CachedPodResolver); ok {
			cached.Invalidate(string(pod.UID))
		}
		if err := r.FDSyncer.SyncNamespaceFromQuota(ctx, pod.Namespace); err != nil {
			logger.Error(err, "fd sync on pod event", "pod", pod.Name)
		}
	}

	if pod.Status.Phase == corev1.PodFailed && r.Recorder != nil {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.ExitCode == sigKillExitCode {
				r.Recorder.Eventf(
					&pod, nil, corev1.EventTypeWarning, "QuotaExceeded", "EnforceQuota",
					"Pod exceeded namespace quota (FD or process limit) and was terminated by eBPF enforcement",
				)
				break
			}
		}
	}

	logger.V(1).Info("pod event",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"phase", pod.Status.Phase,
	)

	return ctrl.Result{}, nil
}

func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}
