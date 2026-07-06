# WTO Test Plan

Two tiers: MVP tests to guard against regressions now, and a production roadmap for when WTO graduates from POC.

**Last updated:** 2026-07-06

---

## Tier 1: MVP — Regression Guards

Enough tests to know we aren't breaking what we've built. Covers the critical fixes (C-2, C-3) and the happy paths we've validated manually on the cluster.

### Approach

Standard Go `testing` + `testify` + fake client for unit tests. Ginkgo + Gomega for E2E against the live OCP 4.21 cluster. No envtest.

### Files

```
internal/testutil/testutil.go                      — scheme, fake client, fixture helpers
internal/webhook/pod_webhook_test.go               — webhook unit tests
internal/controller/placement_controller_test.go   — placement controller unit tests
internal/controller/profile_controller_test.go     — profile controller unit tests
test/e2e/e2e_suite_test.go                         — Ginkgo bootstrap, cluster client
test/e2e/e2e_test.go                               — E2E smoke tests
test/e2e/testutil/helpers.go                       — wait helpers
```

### Unit Tests

**Webhook (~10 tests)**

| Test | Why |
|---|---|
| No annotation → allowed | Webhook is a no-op for unmanaged pods |
| Profile not found → error | Bad annotation fails loudly |
| deviceClaims + Valid!=True → denied | **C-2 regression guard** |
| deviceClaims + Valid=True → allowed | Happy path |
| Blocking conflict: existing resourceClaims → denied | Conflict detection works |
| Defaults inject into all containers | Resource injection basics |
| Container targeting by name | Per-container targeting works |
| DRA claim template name correct | Template name `wto-<profile>-<claim>` |
| Queue label injected | Kueue integration works |
| Scheduling gate added, idempotent | Gate basics |

**Placement Controller (~5 tests)**

| Test | Why |
|---|---|
| Profile NotFound → ungate + ProfileDeleted event | **C-3 regression guard** |
| Quota insufficient → hold gated | Quota pre-flight works |
| Node placement → nodeSelector + tolerations applied, ungated | Node mode works |
| Queue placement → queue label set, ungated | Queue mode works |
| Pod without gate → no-op | Controller ignores unmanaged pods |

**Profile Controller (~6 tests)**

| Test | Why |
|---|---|
| New profile → finalizer added | **C-3 regression guard** |
| Deletion with gated pods → blocked + event | **C-3 regression guard** |
| Deletion with no gated pods → finalizer removed | Deletion completes |
| deviceClaim → RCT created with correct name and owner ref | DRA lifecycle works |
| DeviceClass exists → DeviceClassAvailable=True | Status reporting works |
| DeviceClass missing → DeviceClassAvailable=False | Status reporting works |

### E2E Smoke Tests (~5 tests)

| Test | What it validates | Label |
|---|---|---|
| CPU-only profile: pod gets resources and runs | Basic webhook + placement end-to-end | |
| DRA profile: pod gets GPU via ResourceClaim | DRA injection + GPU allocation + `nvidia-smi` | `Label("gpu")` |
| Queue profile: Kueue creates Workload | Webhook ordering + Kueue integration | |
| Profile deletion with gated pod: pod ungated | C-3 fix end-to-end | |
| Profile with invalid DeviceClass: status reflects it | Status conditions on real cluster | |

### Makefile

```makefile
test-local:
    go test ./internal/... -v -count=1

test-e2e:
    KUBECONFIG=$(KUBECONFIG) go test ./test/e2e/... -v -timeout 10m -count=1

test-e2e-no-gpu:
    KUBECONFIG=$(KUBECONFIG) go test ./test/e2e/... -v -timeout 10m -count=1 -ginkgo.label-filter="!gpu"
```

### Implementation Sequence

1. `internal/testutil/testutil.go` — scheme, fake client, fixture builders
2. Unit tests for all 3 source files (~21 tests)
3. E2E suite + smoke tests (~5 tests)
4. Makefile targets

---

## Tier 2: Production — Full Coverage

For when WTO graduates from POC to a production-ready operator. Builds on Tier 1.

### Additional Testing Stack

| Layer | Tool | Purpose |
|-------|------|---------|
| envtest | `sigs.k8s.io/controller-runtime/pkg/envtest` | Real etcd + kube-apiserver without a live cluster |
| Benchmarks | `testing.B` | Performance regression detection for O(n) operations |
| Fuzz | `testing.F` | Input validation for `resolveResources`, `checkBlockingConflicts` |

### Additional Unit Tests (~80 more cases)

Expand from Tier 1's regression guards to exhaustive edge case coverage:

**Webhook (+30 cases)**
- Decode failure → HTTP 400
- Nil pod annotations → Allowed
- Name takes precedence over index in `resolveResources`
- No match + no defaults → nil from `resolveResources`
- Override tracking: which field paths are recorded
- Nil Requests/Limits maps initialized (no panic)
- Multiple containers with independent resolution
- No containers + no defaults → no modification
- Conflicting queue labels → Denied
- Matching queue labels → no conflict
- Conflicting nodeSelector values → Denied
- Non-overlapping nodeSelector keys → no conflict
- Nil placement, nil node, nil queue → no conflict
- Priority class set on queue label
- No-op for node placement type on queue label injection
- Tracking annotations: nil map initialized, generation set, JSON overrides
- `AppliedAtAnno` not set (L-2 documentation)
- Multiple DRA claims all go to same container (H-1 documentation)
- Target index beyond container count → guard prevents panic

**Placement Controller (+20 cases)**
- Pod not found → no error (IgnoreNotFound)
- Pod without profile annotation → noop
- Profile get error (non-404) → requeues after 10s with event
- Nil placement → only gate removed
- Node placement with nil node → only gate removed
- Queue placement with nil queue → only gate removed
- Merges with existing nodeSelector (additive)
- Preserves existing tolerations (appends)
- Nil pod nodeSelector/labels → initialized
- Queue placement with priority class → both labels set
- Quota: no quotas in namespace → passes (fail-open)
- Quota: list error → passes (fail-open)
- Quota: multiple quotas, first fails
- Quota: fallback to bare resource name (not `requests.` prefix)
- Quota: Used not present → defaults to zero
- Quota: DRA device count=0 defaults to 1
- Quota: no quota key for device class → passes
- Quota: nil `claim.Request.Exactly` → skipped
- `hasSchedulingGate` — other gates don't match
- `removeSchedulingGate` — preserves other gates

**Profile Controller (+30 cases)**
- Profile not found → no error (IgnoreNotFound)
- Finalizer already present → skips update
- RCT already exists → updates spec via CreateOrUpdate
- ensureRCT error → propagated
- Find gated pods error → propagated
- Pods reference profile but not gated → not blocking
- Pods gated but reference different profile → not blocking
- updateStatus: Queue placement with empty queueName → Valid=False
- updateStatus: Queue placement with nil queue → Valid=False
- updateStatus: DRAEnabled condition set/unset
- nodeSatisfiesProfile: multiple labels, one missing → false
- nodeSatisfiesProfile: CPU missing from allocatable → false
- nodeSatisfiesProfile: memory missing from allocatable → false
- nodeSatisfiesProfile: custom resource missing → passes (non-CPU/memory optional)
- nodeSatisfiesProfile: tainted node still counted (H-2 documentation)
- countSatisfiableNodes/countAppliedWorkloads: list error → graceful degradation
- setCondition: ObservedGeneration set from profile.Generation
- setCondition: reason/message updated regardless of status change
- removeCondition: empty conditions → no panic
- removeCondition: removes from middle of list

### envtest Integration Tests (~15 cases)

Full webhook → controller pipeline with a real API server. Catches issues fake client can't: scheduling gate semantics, admission webhook registration, status subresource behavior.

```
test/integration/suite_test.go                   — envtest with CRDs, manager, webhook
test/integration/webhook_integration_test.go     — admission flow
test/integration/placement_integration_test.go   — gate lifecycle
test/integration/profile_integration_test.go     — RCT and status lifecycle
```

Tests:
- Pod with profile → mutated correctly (resources, gate, annotations)
- Pod without profile → not mutated
- Profile not found → pod rejected
- DRA profile valid → pod mutated with resourceClaims
- DRA profile invalid → pod denied
- Pod ungated after reconcile with placement
- Profile deleted → pod ungated (C-3 end-to-end in envtest)
- Quota exceeded → pod held; quota freed → pod ungated
- Finalizer added on creation, removed on clean deletion
- RCT created with owner reference, garbage collected on profile deletion
- Status conditions updated on DeviceClass changes
- SatisfiableNodes reflects node list
- AppliedWorkloads reflects pod list

### Additional E2E Tests (~16 more cases)

Expand E2E from Tier 1 smoke tests to cover validated scenarios from the test cluster:

- Node placement: pod lands on specific GPU node via nodeSelector
- Node placement: tolerations allow scheduling past GPU taint
- Quota hold and release end-to-end
- Override tracking: pod with existing resources gets overrides annotation
- Container targeting by name for InferenceService workloads
- satisfiableNodes counts GPU nodes correctly (expects 2)
- appliedWorkloads tracks referencing pods
- RCT garbage collected on profile deletion
- Profile deletion blocked by gated pods → delete pod → profile deleted
- DRA CEL selector: `productName == "Tesla T4"` → correct GPU
- Multi-node GPU distribution: 2 pods → 2 different T4 nodes
- DRA quota enforcement: quota=0 → ResourceClaim blocked
- Webhook ordering: pod has both WTO queue label and Kueue gate
- Queue placement: Kueue Workload admitted via ClusterQueue
- Profile with multiple deviceClaims (H-1 behavior documentation)
- Defaults override sidecars on InferenceService (H-6 behavior documentation)

### Benchmarks

- `BenchmarkCountSatisfiableNodes` — 500 nodes, 100 profiles
- `BenchmarkCountAppliedWorkloads` — 2000 pods, 100 profiles
- `BenchmarkFindGatedPodsForProfile` — 2000 pods during deletion

### Fuzz Tests

- `FuzzResolveResources` — random container names/indices, nil fields
- `FuzzCheckBlockingConflicts` — random pod specs vs profile specs

### Additional Makefile Targets

```makefile
test-integration:
    go test ./test/integration/... -v -timeout 5m -count=1

test-e2e-gpu:
    KUBECONFIG=$(KUBECONFIG) go test ./test/e2e/... -v -timeout 10m -count=1 -ginkgo.label-filter="gpu"

test-bench:
    go test ./internal/... -bench=. -benchmem -count=3

test-fuzz:
    go test ./internal/webhook/... -fuzz=FuzzResolveResources -fuzztime=30s
    go test ./internal/webhook/... -fuzz=FuzzCheckBlockingConflicts -fuzztime=30s
```
