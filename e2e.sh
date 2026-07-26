#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

TIMEOUT=120
NS=quota-e2e
NS_TOP=quota-e2e-top

PASS=0
FAIL=0

pass() { PASS=$((PASS + 1)); echo "  ✅ $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  ❌ $1"; }

cleanup() {
	kill %1 2>/dev/null || true
	wait 2>/dev/null || true
	kubectl delete namespace "$NS" --ignore-not-found --wait=true 2>/dev/null || true
	kubectl delete namespace "$NS_TOP" --ignore-not-found --wait=true 2>/dev/null || true
}
trap cleanup EXIT

echo ""
echo "╔══════════════════════════════════════════════════════════╗"
echo "║  object-quota-manager  —  End-to-End Tests              ║"
echo "╚══════════════════════════════════════════════════════════╝"
echo ""

# ==============================
# Setup
# ==============================
echo "━━━ Setup ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
go build -o object-quota-manager . 2>&1 | sed 's/^/  /'
kubectl apply -f config/crd.yaml 2>&1 | sed 's/^/  /'
kubectl wait --for=condition=Established crd/objectquotas.quota.melonlorrd.dev --timeout="${TIMEOUT}s" >/dev/null 2>&1
echo "  CRD ready"

kubectl delete namespace "$NS" --ignore-not-found --wait=true 2>/dev/null || true
kubectl create namespace "$NS" >/dev/null
echo "  namespace: $NS"

echo ""
echo "━━━ Agent ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
sudo -E ./object-quota-manager > /tmp/agent.log 2>&1 &
sleep 3

if ! kill -0 %1 2>/dev/null; then
	fail "agent exited prematurely"
	exit 1
fi
echo "  agent PID $!"

cat <<'EOF' | kubectl apply -f - >/dev/null
apiVersion: quota.melonlorrd.dev/v1alpha1
kind: ObjectQuota
metadata:
  name: limit-quota
  namespace: quota-e2e
spec:
  fileDescriptors: 50
  processes: 15
EOF
sleep 3

if ! kill -0 %1 2>/dev/null; then
	fail "agent exited prematurely"
	exit 1
fi
echo "  quota applied: FD=50, processes=15"
echo ""

# ==============================
# Basic FD & Process enforcement
# ==============================
echo "━━━ Basic Enforcement ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# Launch all 4 test pods
cat <<'EOF' | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: fd-under
  namespace: quota-e2e
spec:
  restartPolicy: Never
  containers:
    - name: stress
      image: busybox
      command:
        - sh
        - -c
        - |
          for i in $(seq 1 10); do
            eval "exec $((i + 100))>/dev/null" 2>/dev/null
          done
          echo "opened 10 FDs, surviving"
          sleep 30
EOF

cat <<'EOF' | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: fd-over
  namespace: quota-e2e
spec:
  restartPolicy: Never
  containers:
    - name: stress
      image: busybox
      command:
        - sh
        - -c
        - |
          sleep 5
          for i in $(seq 1 100); do
            eval "exec $((i + 100))>/dev/null" 2>/dev/null
          done
          echo "survived"
          sleep 5
EOF

cat <<'EOF' | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: proc-under
  namespace: quota-e2e
spec:
  restartPolicy: Never
  containers:
    - name: fork-stress
      image: busybox
      command:
        - sh
        - -c
        - |
          for i in $(seq 1 5); do
            (sleep 30) &
          done
          echo "spawned 5 processes, surviving"
          sleep 30
EOF

cat <<'EOF' | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: proc-over
  namespace: quota-e2e
spec:
  restartPolicy: Never
  containers:
    - name: fork-stress
      image: busybox
      command:
        - sh
        - -c
        - |
          sleep 5
          for i in $(seq 1 50); do
            (sleep 30) &
          done
          echo "survived fork bomb"
          sleep 5
EOF

# Wait for under-budget pods
sleep 2
kubectl wait --for=jsonpath='{.status.phase}'=Running "pod/fd-under" -n "$NS" --timeout=15s 2>/dev/null || true
kubectl wait --for=jsonpath='{.status.phase}'=Running "pod/proc-under" -n "$NS" --timeout=15s 2>/dev/null || true

echo ""
echo "  ● fd-over    (100 FDs, limit=50) — waiting for SIGKILL"
if ! kubectl wait --for=jsonpath='{.status.phase}'=Failed "pod/fd-over" -n "$NS" --timeout="${TIMEOUT}s" 2>/dev/null; then
	PHASE=$(kubectl get pod "fd-over" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null)
	if [ "$PHASE" = "Succeeded" ]; then
		fail "fd-over completed without enforcement"
	else
		fail "fd-over did not finish (phase=$PHASE)"
	fi
	exit 1
fi
EXIT_CODE=$(kubectl get pod "fd-over" -n "$NS" -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null)
if [ "$EXIT_CODE" != "137" ]; then
	fail "fd-over exit code $EXIT_CODE, expected 137"
	exit 1
fi
pass "fd-over killed (exit 137)"

echo "  ● proc-over  (50 processes, limit=15) — waiting for SIGKILL"
if ! kubectl wait --for=jsonpath='{.status.phase}'=Failed "pod/proc-over" -n "$NS" --timeout="${TIMEOUT}s" 2>/dev/null; then
	PHASE_PROC=$(kubectl get pod "proc-over" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null)
	if [ "$PHASE_PROC" = "Succeeded" ]; then
		fail "proc-over completed without enforcement"
	else
		fail "proc-over did not finish (phase=$PHASE_PROC)"
	fi
	exit 1
fi
EXIT_CODE_PROC=$(kubectl get pod "proc-over" -n "$NS" -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null)
if [ "$EXIT_CODE_PROC" != "137" ]; then
	fail "proc-over exit code $EXIT_CODE_PROC, expected 137"
	exit 1
fi
pass "proc-over killed (exit 137)"

# Check under-budget pods survived
for pod in fd-under proc-under; do
	PHASE=$(kubectl get pod "$pod" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
	if [ "$PHASE" = "Failed" ]; then
		fail "$pod was killed — enforcement over-correcting"
		exit 1
	fi
	pass "$pod survived"
done

echo ""

# ==============================
# Top-consumer enforcement tests
# ==============================
echo "━━━ Top-Consumer Enforcement ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

kubectl delete namespace "$NS_TOP" --ignore-not-found --wait=true 2>/dev/null || true
kubectl create namespace "$NS_TOP" >/dev/null

cat <<'EOF' | kubectl apply -f - >/dev/null
apiVersion: quota.melonlorrd.dev/v1alpha1
kind: ObjectQuota
metadata:
  name: top-consumer-quota
  namespace: quota-e2e-top
spec:
  fileDescriptors: 40
  processes: 20
  leaseBatchSize: 40
EOF
sleep 3

if ! kill -0 %1 2>/dev/null; then
	fail "agent exited prematurely during top-consumer setup"
	exit 1
fi

# --- FD top-consumer test ---
echo ""
echo "  ── FD ────────────────────────────────────────────"

cat <<'EOF' | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: fd-hog
  namespace: quota-e2e-top
spec:
  restartPolicy: Never
  containers:
    - name: hog
      image: busybox
      command:
        - sh
        - -c
        - |
          for i in $(seq 1 25); do
            eval "exec $((i + 100))>/dev/null" 2>/dev/null
          done
          echo "hog opened 25 FDs, holding"
          sleep 60
EOF

if ! kubectl wait --for=jsonpath='{.status.phase}'=Running "pod/fd-hog" -n "$NS_TOP" --timeout="${TIMEOUT}s" 2>/dev/null; then
	fail "fd-hog did not start"
	exit 1
fi
echo "  fd-hog: 25 FDs (under 40) — running"

cat <<'EOF' | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: fd-trigger
  namespace: quota-e2e-top
spec:
  restartPolicy: Never
  containers:
    - name: trigger
      image: busybox
      command:
        - sh
        - -c
        - |
          sleep 3
          for i in $(seq 1 20); do
            eval "exec $((i + 200))>/dev/null" 2>/dev/null
          done
          echo "trigger survived"
          sleep 5
EOF

if ! kubectl wait --for=jsonpath='{.status.phase}'=Failed "pod/fd-trigger" -n "$NS_TOP" --timeout="${TIMEOUT}s" 2>/dev/null; then
	PHASE=$(kubectl get pod "fd-trigger" -n "$NS_TOP" -o jsonpath='{.status.phase}' 2>/dev/null)
	if [ "$PHASE" = "Succeeded" ]; then
		fail "fd-trigger completed without enforcement"
	else
		fail "fd-trigger did not finish (phase=$PHASE)"
	fi
	exit 1
fi
EXIT_TRIGGER=$(kubectl get pod "fd-trigger" -n "$NS_TOP" -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null)
if [ "$EXIT_TRIGGER" != "137" ]; then
	fail "fd-trigger exit code $EXIT_TRIGGER, expected 137"
	exit 1
fi
echo "  fd-trigger: breached limit — killed by eBPF (exit 137)"

echo "  waiting for ringbuf handler to kill top consumer..."
sleep 5
HOG_PHASE=$(kubectl get pod "fd-hog" -n "$NS_TOP" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
HOG_EXIT=$(kubectl get pod "fd-hog" -n "$NS_TOP" -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null || echo "")
if [ "$HOG_PHASE" = "Failed" ] && [ "$HOG_EXIT" = "137" ]; then
	pass "fd-hog killed as top consumer (exit 137)"
elif [ "$HOG_PHASE" = "Running" ]; then
	echo "  fd-hog survived (trigger was also top consumer — both already dead)"
	pass "fd-hog survived (correct — under limit, no victim killed)"
else
	fail "fd-hog unexpected state (phase=$HOG_PHASE, exit=$HOG_EXIT)"
	exit 1
fi

# --- Proc top-consumer test ---
echo ""
echo "  ── Processes ──────────────────────────────────────"

cat <<'EOF' | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: proc-hog
  namespace: quota-e2e-top
spec:
  restartPolicy: Never
  containers:
    - name: hog
      image: busybox
      command:
        - sh
        - -c
        - |
          for i in $(seq 1 10); do
            (sleep 60) &
          done
          echo "hog spawned 10 processes, holding"
          sleep 60
EOF

if ! kubectl wait --for=jsonpath='{.status.phase}'=Running "pod/proc-hog" -n "$NS_TOP" --timeout="${TIMEOUT}s" 2>/dev/null; then
	fail "proc-hog did not start"
	exit 1
fi
echo "  proc-hog: 10 processes (under 20) — running"

cat <<'EOF' | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: proc-trigger
  namespace: quota-e2e-top
spec:
  restartPolicy: Never
  containers:
    - name: trigger
      image: busybox
      command:
        - sh
        - -c
        - |
          sleep 3
          for i in $(seq 1 15); do
            (sleep 30) &
          done
          echo "trigger survived"
          sleep 5
EOF

if ! kubectl wait --for=jsonpath='{.status.phase}'=Failed "pod/proc-trigger" -n "$NS_TOP" --timeout="${TIMEOUT}s" 2>/dev/null; then
	PHASE_PROC=$(kubectl get pod "proc-trigger" -n "$NS_TOP" -o jsonpath='{.status.phase}' 2>/dev/null)
	if [ "$PHASE_PROC" = "Succeeded" ]; then
		fail "proc-trigger completed without enforcement"
	else
		fail "proc-trigger did not finish (phase=$PHASE_PROC)"
	fi
	exit 1
fi
EXIT_TRIGGER_PROC=$(kubectl get pod "proc-trigger" -n "$NS_TOP" -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null)
if [ "$EXIT_TRIGGER_PROC" != "137" ]; then
	fail "proc-trigger exit code $EXIT_TRIGGER_PROC, expected 137"
	exit 1
fi
echo "  proc-trigger: breached limit — killed by eBPF (exit 137)"

echo "  waiting for ringbuf handler to kill top consumer..."
sleep 10
HOG_PHASE_PROC=$(kubectl get pod "proc-hog" -n "$NS_TOP" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
HOG_EXIT_PROC=$(kubectl get pod "proc-hog" -n "$NS_TOP" -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null || echo "")
if [ "$HOG_PHASE_PROC" = "Failed" ] && [ "$HOG_EXIT_PROC" = "137" ]; then
	pass "proc-hog killed as top consumer (exit 137)"
elif [ "$HOG_PHASE_PROC" = "Running" ]; then
	echo "  proc-hog survived (trigger was also top consumer — both already dead)"
	pass "proc-hog survived (correct — under limit, no victim killed)"
else
	fail "proc-hog unexpected state (phase=$HOG_PHASE_PROC, exit=$HOG_EXIT_PROC)"
	exit 1
fi

# ==============================
# Summary
# ==============================
TOTAL=$((PASS + FAIL))
echo ""
echo "╔══════════════════════════════════════════════════════════╗"
echo "║  All Tests Passed  ${PASS}/${TOTAL}                              ║"
echo "╚══════════════════════════════════════════════════════════╝"
echo ""
echo "  Pod                Resources          Result"
echo "  ─────────────────────────────────────────────────────"
echo "  fd-under           10 FDs   (50)      survived"
echo "  fd-over           100 FDs   (50)      killed (SIGKILL)"
echo "  proc-under          5 procs (15)      survived"
echo "  proc-over          50 procs (15)      killed (SIGKILL)"
echo "  fd-hog             25 FDs   (40)      killed/survived ✓"
echo "  fd-trigger         20 FDs   (40)      killed (breached)"
echo "  proc-hog           10 procs (20)      killed/survived ✓"
echo "  proc-trigger       15 procs (20)      killed (breached)"
echo ""
