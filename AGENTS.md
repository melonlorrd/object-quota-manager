# object-quota-manager

eBPF-powered Kubernetes agent enforcing per-namespace file descriptor and process limits.

## Structure

```
.
├── main.go           # entrypoint (package main)
├── controller.go     # reconciliation loops (ObjectQuota + Pod)
├── syncer.go         # eBPF load + map sync + lease watermark reconciliation
├── lease.go          # sharded in-memory lease manager for multi-node credit allocation
├── manager.go        # eBPF map management (Manager, StubManager, MapKind specs)
├── cgroup.go         # cgroup v2 ID resolution (with caching + fake resolver)
├── metrics.go        # Prometheus exports (FD + process counters, limits, reconciles)
├── types.go          # CRD types (quota.melonlorrd.dev/v1alpha1)
├── lease_test.go     # unit tests: lease grant, exhaustion, release
├── syncer_test.go    # unit tests: sync, cleanup, leaser integration, cgroup caching
├── benchmark_test.go # benchmarks: namespace key hashing, lease manager, syncer
├── load_test.go      # e2e load test for actual eBPF program attachment
├── ebpf/             # generated BPF wrappers + C source (enforcer_kern.c)
├── config/           # Kubernetes manifests (CRD, test pods, test quota)
├── e2e.sh            # end-to-end tests
└── Makefile
```

## Conventions

- All Go source is `package main` at root (single package)
- eBPF programs live in `ebpf/src/` (single C source); Go-side sync in `syncer.go`
- Tests accompany every source file; add e2e scenarios to `e2e.sh`
- eBPF maps keyed by cgroup v2 ID (uint64) or namespace hash (uint64)
- Controller ↔ eBPF communicate only through maps

## Makefile

All interactions go through `make` — never run go commands directly.

| Target | Purpose |
|--------|---------|
| `make build` | Build the binary |
| `make test` | Run all unit tests |
| `make bench` | Run benchmarks |
| `make test-e2e` | Run end-to-end tests (`e2e.sh`) |
| `make generate` | Compile eBPF programs (`bpf2go`, etc.) |
| `make clean` | Clean build artifacts |
