# WTO Roadmap

This document tracks the path from design to production-ready MVP.

**Last updated:** 2026-07-01
**Cluster:** OCP 4.21.21 / K8s 1.34.8, 2x Tesla T4 (g4dn.xlarge)
**Image:** `quay.io/jeffdyoung/wto:latest`

## Phase 0: Scaffold — DONE (2026-07-01)

**Goal:** Buildable, deployable operator skeleton with CRD installed on a cluster.

- [x] Scaffold operator (`workload-tuning.io` API group, `github.com/jeffdyoung/wto` module)
- [x] Define `WorkloadProfile` types in `api/v1alpha1/`
  - `WorkloadProfileSpec`: `defaults`, `containers[]`, `deviceClaims[]`, `placement`
  - `WorkloadProfileStatus`: conditions, `satisfiableNodes`, `appliedWorkloads`
  - CEL validation rules: name/index mutual exclusivity, placement discriminated union
- [x] Generate CRDs via `controller-gen` (v0.19.0)
- [x] Verify CRD installs cleanly on OCP 4.21
- [x] Dockerfile (golang:1.26 → distroless), Makefile (all builds via podman containers)

**Not done:** OLM bundle scaffold (deferred to Phase 8).

## Phase 1: Webhook + Injection — DONE (2026-07-01)

**Goal:** Webhook injects resources, DRA claim references, and scheduling gate during pod CREATE.

**Architecture note:** Spike testing (2026-07-01) proved `pod.spec.resourceClaims` and `container.resources` are **immutable after creation** on K8s 1.34, even on gated pods. The webhook must handle these fields during the CREATE admission call. See `spike-archive/FINDINGS.md` and ADR-002 (revised).

- [x] `MutatingWebhookConfiguration` with `failurePolicy: Fail`, namespaceSelector
- [x] Webhook reads WorkloadProfile CR from informer cache
- [x] Injects `container.resources` (requests/limits) into targeted containers
- [x] Injects `pod.spec.resourceClaims[]` with `resourceClaimTemplateName` reference
- [x] Injects `container.resources.claims[]` linking containers to claims
- [x] Adds scheduling gate
- [x] Sets tracking annotations (`profile-generation`, `overrides`)
- [x] TLS via OpenShift service-ca (no cert-manager needed)
- [x] Container targeting: by name, by index, defaults fallback

## Phase 2: Placement Controller — DONE (2026-07-01)

**Goal:** Controller adds mutable placement fields and removes scheduling gate.

- [x] Node placement: `nodeSelector` + `tolerations` injected into gated pod
- [x] Queue placement: `kueue.x-k8s.io/queue-name` label set on pod
- [x] Scheduling gate removed after placement applied
- [x] Events emitted: `Ungated` with profile name
- [x] Error handling: missing profile → warning event, requeue with backoff
- [x] Integration test: Node mode verified on 2x T4 GPU cluster

**Not fully tested:** Queue mode (Kueue degraded on cluster — cert-manager dependency missing).

## Phase 3: DRA — DONE (2026-07-01)

**Goal:** Profile Controller creates ResourceClaimTemplates from `deviceClaims[]`.

- [x] Profile Controller watches WorkloadProfile CRs and owned ResourceClaimTemplates
- [x] `CreateOrUpdate` ResourceClaimTemplate from each `deviceClaims[]` entry
- [x] Template naming: `wto-<profile>-<claim>` (deterministic)
- [x] Owner reference: WorkloadProfile (garbage collected on profile deletion)
- [x] `DRAEnabled` and `DeviceClassAvailable` status conditions set
- [x] Integration test: profile with deviceClaims → auto-created template → pod gets GPU via DRA

**Not done:** Device plugin fallback auto-detection (deferred — DRA is active on cluster).

## Phase 4: Profile Controller — Validation and Status — DONE (2026-07-01)

**Goal:** WorkloadProfiles continuously report their fitness against cluster state.

- [x] `Valid` condition: checks dependencies (templates exist, placement config valid)
- [x] `DeviceClassAvailable` condition: referenced DeviceClasses exist in cluster
- [x] `DRAEnabled` condition: DRA API available
- [x] `satisfiableNodes`: count of nodes matching profile's nodeSelector + resource constraints
- [x] `appliedWorkloads`: count of pods referencing this profile

**Not done:**
- `QueueReady` condition (Kueue not fully functional on cluster)
- `QuotaFit` condition (structural vs transient quota distinction)
- `quotaSummary` field
- Drift detection (pods with stale `profile-generation` annotation)
- Periodic re-reconciliation for appliedWorkloads freshness

## Phase 5: Pre-flight Quota Checks — DONE (2026-07-01)

**Goal:** Gated pods are held until quota allows admission.

- [x] Placement Controller checks namespace ResourceQuota before ungating
- [x] Checks both traditional resources (cpu, memory) and DRA device quotas
- [x] Pod stays gated with `QuotaInsufficient` event and actionable message
- [x] Auto-retry every 30s — pod proceeds automatically when quota frees up
- [x] Integration test: quota filled → pod held → quota freed → pod ungated → GPU access confirmed

## Phase 6: Conflict Detection — DONE (2026-07-01)

**Goal:** Detect and handle conflicts between pod spec and profile.

- [x] **Blocking conflicts** (pod rejected at admission):
  - Pod has existing `resourceClaims` AND profile has `deviceClaims`
  - Pod has `kueue.x-k8s.io/queue-name` label pointing to different queue than profile
  - Pod has nodeSelector key contradicting profile's nodeSelector
- [x] **Overrides** (allowed with warning):
  - Container already has resources for a key the profile specifies → overridden, recorded in `workload-tuning.io/overrides` annotation
- [x] Integration test: conflict pod rejected with clear message

**Not done:**
- Per-container targeting edge cases (sidecar injection reordering, init containers)
- `conflictPolicy` field on WorkloadProfile (deferred to post-MVP)

## Phase 7: Integration Testing — OCP + Kueue + DRA

**Goal:** WTO works end-to-end on a production-like OpenShift cluster with real GPUs, Kueue, and DRA.

### Already Validated (during Phases 0-6)

- [x] Bare pod with WorkloadProfile → GPU accessible via DRA
- [x] Pod without annotation → completely unaffected
- [x] Node placement: pod lands on GPU node via nodeSelector
- [x] DRA with CEL selector (`productName == "Tesla T4"`)
- [x] Multi-node GPU distribution (2 pods → 2 different T4 nodes, different UUIDs)
- [x] Resource override warnings (annotation tracks overridden fields)
- [x] Conflict blocking (existing resourceClaims rejected)
- [x] Quota hold/release (pod gated → quota freed → pod proceeds → GPU access)
- [x] Profile status: Valid, DRA, Nodes columns populate correctly

### Validated (2026-07-02)

- [x] Queue placement with live Kueue — installed cert-manager, fixed stale Kueue CRD storedVersions, validated full flow: WTO injects queue label at CREATE → Kueue creates Workload → Kueue admits via ClusterQueue → GPU access confirmed. Required webhook ordering fix (ADR-013) and moving queue label injection from controller to webhook.
- [x] Kubeflow Notebook with WorkloadProfile — annotation must be on both Notebook CR metadata and `spec.template.metadata.annotations` for propagation through StatefulSet. DRA claim injected, GPU node selected, Tesla T4 accessible via `nvidia-smi`.
- [x] Device plugin fallback tested — confirmed DRA driver and device plugin are mutually exclusive at the GPU Operator level. When DRA is active, `nvidia.com/gpu` allocatable is 0. Fallback requires a cluster without DRA or mixed GPU types.

### Remaining

- [ ] KServe InferenceService with WorkloadProfile
- [ ] Job with WorkloadProfile
- [ ] Missing LocalQueue → `QueueReady: False`
- [ ] ClusterQueue quota exhausted → Kueue queues workload
- [ ] WTO webhook unavailable → pods with annotation rejected, others unaffected
- [ ] GitOps compatibility (ArgoCD sees no drift on workload CRs)
- [ ] Scale: 50 profiles, 100 simultaneous pods

## Phase 8: OLM Packaging and Release

**Goal:** WTO is installable via OLM on OpenShift and via Helm on vanilla Kubernetes.

- [ ] OLM bundle: CSV with install strategy, RBAC, webhook definition
- [ ] Catalog source for OperatorHub
- [ ] Helm chart (optional, for non-OLM clusters)
- [ ] Container image CI: multi-arch build (amd64, arm64)
- [ ] Release process: semantic versioning, changelog, GitHub releases
- [ ] Upgrade testing: v0.1.0 → v0.2.0

## Phase 9: Platform Integration Testing

**Goal:** Validate that AI/ML platforms can deploy and consume WTO as a managed component.

- [ ] Platform deploys WTO via embedded manifests (Kustomize)
- [ ] Platform manages WTO lifecycle: install, upgrade, removal, managementState
- [ ] Platform creates WorkloadProfiles in project namespaces
- [ ] Platform dashboard reads WorkloadProfile status for profile selector UI
- [ ] Migration tooling: prior hardware profile CRs → WorkloadProfile CRs
- [ ] End-to-end: dashboard → Notebook → WTO → Kueue → DRA → GPU

## Phase 10: Hardening

**Goal:** WTO is production-ready for multi-tenant GPU clusters.

- [ ] RBAC audit, user-facing roles (`workload-profile-viewer`, `workload-profile-admin`)
- [ ] Multiple replicas, anti-affinity, leader election, `system-cluster-critical`
- [ ] Prometheus metrics (`wto_pods_gated`, `wto_gate_duration_seconds`, etc.)
- [ ] ServiceMonitor + PrometheusRule for OpenShift monitoring
- [ ] Benchmark: gate-to-ungate < 2s p95 (resource-only), < 5s p95 (DRA)
- [ ] NetworkPolicy, Pod Security Standards compliance

## Open Questions

| Question | Impact | Status |
|---|---|---|
| `conflictPolicy` field on WorkloadProfile? | Strict teams make conflicts an error | Deferred to post-MVP |
| Auto-create profiles from cluster-scoped templates? | Simplifies project setup | Deferred to post-MVP |
| MTO integration path? | Merge, library import, or coordination | Deferred — standalone for now |
| Profile inheritance (base + overrides)? | Reduces duplication | Deferred to post-MVP |
| Dry-run mode? | Debugging and migration validation | Deferred to post-MVP |
| `firstAvailable` handling with Kueue? | Kueue doesn't support it | Blocked on upstream |
| CLI validation tool? | CI/CD pipelines and GitOps | Deferred to post-MVP |
| appliedWorkloads freshness? | Counts go stale without periodic re-reconciliation | Needs periodic requeue or pod watch |
