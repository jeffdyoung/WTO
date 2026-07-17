# WTO GPUaaS Readiness Assessment

How WTO addresses the GPUaaS requirements distilled from 530 Jira issues across OCPSTRAT, RHAIRFE, RHAISTRAT, RFE, RHOAIENG. Cross-references `requirements/technical-requirements.md` and `requirements/requirements-ownership.md`.

Architecture context: WTO operates under **Model C (Resolution Engine)** — WTO resolves profiles and exposes `resolvedSpec` in WorkloadProfile status; component teams (KServe, Notebook controller, llm-d, training operator) consume `resolvedSpec` to apply resources to their own CRs; the pod-level webhook remains as a universal fallback. See `template-binding-design.md` for the full architecture rationale.

Last updated: 2026-07-15

---

## 1. GPU Device Selection & Allocation

**WTO's role:** Primary owner of DRA DeviceRequest + ResourceClaimTemplate lifecycle. Profiles abstract DeviceClasses behind admin-created templates.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| Attribute-based GPU selection (model, memory, CEL selectors) | OCPSTRAT-2384, RHAIRFE-1817, RHAISTRAT-1470 | **Primary** — `deviceClaims[].request` embeds native DRA `DeviceRequest` | **Built** |
| MIG partition selection | RHAISTRAT-737, RHAIRFE-1885 | **Profile** — CRD supports CEL selectors on partitioned devices | CRD ready, no examples/validation |
| Same-family heterogeneous GPU (prefill/decode disaggregation) | RHAIRFE-2528 | **Admission-time** — `firstAvailable` DRA selector for fallback GPU selection | **Not built** — CRD doesn't expose `firstAvailable` |
| Time-sliced / virtual GPU | RHAIRFE-1715 | None — driver-level config; WTO selects if driver publishes as DRA | N/A |
| Device health status in pod status | OCPSTRAT-2113 | None — upstream K8s 1.38 | N/A |
| Device taints and tolerations | OCPSTRAT-2117 | **Gap** — WTO CRD needs toleration field on `deviceClaims[]` for K8s 1.35+ DRADeviceTaints | **Not built** |
| Device binding conditions | OCPSTRAT-2116 | None — upstream K8s | N/A |
| Extended resource bridge | OCPSTRAT-2382 | None — K8s DRAExtendedResource feature gate handles automatically | N/A |
| GPU model/memory discovery for right-sizing | RHAIRFE-2645 | **Provider** — `satisfiableNodes` + DeviceClass enumeration via `resolvedSpec` | **Partial** — `satisfiableNodes` ignores taints (H-2), no DeviceClass enumeration API |

### Bug-revealed requirements

| Bug | What it tells us | WTO response |
|---|---|---|
| RHAISTRAT-868 | "None" accelerator exposes all GPUs | Profile enforces explicit opt-in — no profile annotation = no injection |
| RHAIRFE-494 | Users need GPU requests of 0 (CPU-only) | Profile `defaults` can omit GPU resources; `deviceClaims` is optional |
| RHOAIENG-35532 | Model deployment fails with HWP + GPU | Model C: KServe consumes `resolvedSpec`; pod webhook as fallback |
| RHOAIENG-28911 | Unable to assign GPU with Hardware Profile | WTO replaces fragile profile-to-device mapping with DRA DeviceRequest |

**Scorecard: 2 built, 2 partial, 2 not built, 3 not WTO scope.**

---

## 2. Hardware Profile Lifecycle

**WTO's role:** Primary owner. WorkloadProfileTemplate (cluster-scoped) + WorkloadProfile (namespace-scoped binding) replace HardwareProfile. Single owner of CRD, webhook, and reconciler — eliminates the current 4-way split.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| Profile CRD + reconciler (single owner) | — | **Primary** — WorkloadProfileTemplate + WorkloadProfile two-CRD model | **Built** — two-CRD model implemented (2026-07-17) |
| Per-container resource targeting (name/index) | RHAIRFE-834, RHAISTRAT-1164, RHOAIENG-49069 | **Primary** — ADR-012 | **Built**, but init containers not handled (M-1) |
| nodeSelector + tolerations in profile | RHOAIENG-26497, RHOAIENG-50827 | **Primary** — `placement.node` | **Built** |
| Merge semantics (additive tolerations) | RHOAIENG-47369, RHOAIENG-47774 | **Primary** — ADR-003 | **Built** |
| Profile changes don't disrupt running workloads | WTO ADR-010 | **Primary** — drift detection + soft enforcement | **Design only** — no code |
| Deleted profile doesn't strand workloads | RHOAIENG-50913, RHOAIENG-36425, RHOAIENG-50026 | **Primary** — finalizer pattern (C-3) | **Built** |
| Profile status conditions (Valid, QuotaFit, QueueReady, etc.) | — | **Primary** — 5 condition types | **Partial** — Valid, DRAEnabled, DeviceClassAvailable built; QueueReady, QuotaFit not implemented (M-7) |
| SDK discovers available profiles | RHAIRFE-1815 | **Provider** — WorkloadProfile status API, `resolvedSpec` | Works via kubectl; no SDK integration |
| Accelerator type awareness (profile knows GPU model) | RHAIRFE-834, RHAISTRAT-1164 | **Primary** — `deviceClassName` + CEL selectors in `resolvedSpec` | **Partial** — no GPU model discovery/enumeration |
| Injection for ALL workload types | RHOAIENG-76305, RHOAIENG-50153 | **Model C: dual path** — component teams consume `resolvedSpec`; pod webhook as universal fallback | Pod webhook **built**; `resolvedSpec` contract **not built** |
| Cleanup on profile switch (remove stale state) | RHOAIENG-26263, RHOAIENG-38028 | **Primary** — requires drift detection (ADR-010) | **Not built** |
| Profile trace and replay for simulation | RHAIRFE-2355, RHAISTRAT-2121 | None — CLI/benchmarking tool scope | N/A |

### Bug-revealed requirements

| Bug | What it tells us | WTO response |
|---|---|---|
| RHOAIENG-76305 | Tolerations/nodeSelector not injected into ISVC after webhook migration | Model C: KServe consumes `resolvedSpec` for ISVC; pod webhook as fallback |
| RHOAIENG-67403 | Post-Kueue-enable, HWPs can't use nodeSelectors/tolerations | WTO `placement` is a discriminated union — Node and Queue modes are explicit, not conflicting |
| RHOAIENG-59801 | HWP webhook sets limits differently than dashboard | WTO is single source of truth via `resolvedSpec` — dashboard reads, not reimplements |
| RHOAIENG-57621 | Workbench uses "Default" for both Request and Limit | WTO CRD has explicit `requests` and `limits` fields — no ambiguity |
| RHOAIENG-50827 | API drops nodeSelector/tolerations during reconciliation | WTO CRD schema preserves scheduling fields; CEL validation enforces structure |
| RHOAIENG-49069 | Migration injects into ALL containers | ADR-012: per-container targeting by `name` or `index` |
| RHOAIENG-47369, 47774 | HWP clobbers InferenceService tolerations | ADR-003: tolerations are appended (additive merge), never replaced |
| RHOAIENG-38028 | Updating HWP annotation doesn't remove old tolerations | Requires drift detection (ADR-010) — not yet built |
| RHOAIENG-26263, 27374 | nodeSelector not cleared when switching profiles | Requires drift detection (ADR-010) — not yet built |
| RHOAIENG-37391 | Project-scoped HWPs visible in new projects | WorkloadProfile is namespace-scoped — inherent scope isolation |
| RHOAIENG-22516 | Workbench blocked if no HWPs exist | Profile annotation is optional — no annotation = no injection, workload proceeds |
| RHOAIENG-66855 | Dashboard selects CUDA instead of ROCm for AMD | `resolvedSpec` carries `deviceClassName` — dashboard reads accelerator type from profile, not guessing |

**Scorecard: 5 built, 4 partial, 2 not built, 1 not WTO scope.**

---

## 3. Quota & Admission Control

**WTO's role:** Pre-flight quota validator. Kueue and ResourceQuota are the authoritative enforcers — WTO checks before ungating and reports actionable conditions.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| Per-namespace GPU quota enforcement | RFE-4, RFE-3315, RHAIRFE-2609 | None — Kueue ClusterQueue + K8s ResourceQuota | N/A |
| Hierarchical quota (org → team → project) | RHAIRFE-2609, RHAISTRAT-1793 | None — Kueue Cohorts (OCPSTRAT-3444) | N/A |
| Pre-submission quota validation with profile-aware messages | WTO ADR-005 | **Primary** — PlacementReconciler checks quota before ungating | **Partial** — per-pod check works; only examines first container (H-3); profile-level `QuotaFit` not implemented |
| Quota visibility in dashboard before workload creation | RHAIRFE-1787, RHAISTRAT-1577 | **Provider** — `quotaSummary` + `QuotaFit` condition in WorkloadProfile status | **Not built** |
| Filter HWPs by available LocalQueues | RHOAIENG-58655 | None — dashboard-side filtering on `placement.queue.localQueueName` | N/A |
| Cross-queue HWP scope isolation | RHOAIENG-67395 | **Enabler** — WorkloadProfile is namespace-scoped | **Built** (CRD is namespace-scoped) |

**Scorecard: 1 built, 1 partial, 1 not built, 3 not WTO scope.**

---

## 4. Fair-Share Scheduling & Multi-Tenancy

**WTO's role:** Queue assignment only — profile's `placement.queue` sends workloads to the right Kueue queue. Kueue does the scheduling math.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| Fair-share scheduling with guaranteed quotas + borrowing | RHAIRFE-2611, RHAIRFE-2605 | **Queue assignment** — profile's `placement.queue` routes to Kueue queues | **Built** |
| Admission fair sharing | OCPSTRAT-2588 | None — Kueue GA | N/A |
| GPU preemption | OCPSTRAT-2383 | None — upstream K8s 1.38 | N/A |
| Backfill scheduling | RHAIRFE-2602 | None — Kueue borrowingLimit | N/A |
| Idle GPU reclaim | RHAISTRAT-408, RHAIRFE-537 | None — needs Device Reclaim Controller (new component) | N/A |
| GPU-only idle culling | RHAIRFE-440 | None — Notebook Controller (already implemented) | N/A |
| Multi-tenant scheduling policies | RHAIRFE-1818 | **Queue assignment** — WTO assigns per-tenant queues via profile | **Built** |
| Hard multi-tenancy via virtualized OCP | RHAIRFE-2291, RHAISTRAT-2006 | None — HyperShift/HCP architecture decision | N/A |

**Scorecard: 2 built, 0 partial, 0 not built, 5 not WTO scope.**

---

## 5. Topology-Aware Placement

**WTO's role:** Routes workloads to Kueue TAS-managed queues. Kueue TAS handles topology placement.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| NVLink/InfiniBand topology-aware GPU placement | RHAIRFE-2597, RHAIRFE-2603 | **Queue assignment** — routes to Kueue TAS-managed queues | **Built** (queue label injection) |
| Topology-aware scheduling for multi-node training | RHAIRFE-2546, OCPSTRAT-3258 | **Queue assignment** | **Built** |
| NUMA-aware GPU placement | RFE-6498, RFE-8675 | **CEL selector** — can filter on PCIe topology when driver publishes `numaNode` | CRD ready, depends on NVIDIA DRA driver |
| Gang scheduling (all-or-nothing) | RHAIRFE-2596, RFE-3170 | None — Kueue workload groups via JobSet/MPIJob | N/A |

**Scorecard: 2 built, 1 CRD ready (driver-dependent), 0 not built, 1 not WTO scope.**

---

## 6. GPU Observability & Health

**WTO's role:** Metadata provider. WTO's `resolvedSpec` carries device info for workload-to-GPU correlation. Profile name as pod label enables metric attribution.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| GPU topology and utilization dashboard | RHAISTRAT-1757, RHAIRFE-1789 | **Model C: provider** — `resolvedSpec` carries device info for workload-to-GPU correlation | **Partial** — `resolvedSpec` written to status (2026-07-17); consumer SDK not formalized |
| GPU metrics in model serving dashboards | RHAIRFE-1862, RHAISTRAT-1895 | None — COO + DCGM | N/A |
| DCGM metrics correlation with Ray/training | RHAIRFE-2544 | **Label provider** — profile name as pod label for metric correlation | **Built** (2026-07-17) — webhook sets `workload-template.io/profile-name` and `workload-template.io/template-name` as pod labels |
| Per-team GPU utilization observability | RHAIRFE-1819, RHAIRFE-1717 | None — COO dashboards + Kueue queue metrics | N/A |
| GPU fleet health monitoring | RHAIRFE-2601, RHAIRFE-2598 | None — vendor GPU operators | N/A |
| GPU card failure alerts | RFE-5641 | None — vendor GPU operators | N/A |
| GPU metrics on OCP console | RFE-8500, RFE-8648 | None — OCP upstream | N/A |
| Proactive GPU resource limit alerting for DSPs | RHAIRFE-2150 | None — OCP Monitoring alert rules | N/A |
| DRA resource visibility in OCP console | RFE-9480 | None — OCP upstream | N/A |

**Scorecard: 1 built (cost labels), 1 partial (resolvedSpec), 0 not built, 7 not WTO scope.**

---

## 7. Cost Attribution & Chargeback

**WTO's role:** Metadata provider. Profile annotations/labels on pods enable OpenCost to attribute GPU cost to workloads/tenants.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| Per-tenant GPU usage reporting | RHAIRFE-2294, RHAISTRAT-1821 | **Metadata provider** — profile labels on pods for OpenCost attribution | **Built** (2026-07-17) — profile name set as pod label |
| GPU compute accounting and chargeback | RHAIRFE-2607, RHAIRFE-2600 | **Metadata provider** — profile + template labels on pods | **Built** (2026-07-17) |
| GPU cost tracking per workload | RHAIRFE-1725, RHAIRFE-1699 | **Metadata provider** — profile + template labels on pods | **Built** (2026-07-17) |
| Cost visualization dashboard | RHAIRFE-1726 | None — COO Perses dashboards | N/A |
| Showback/chargeback reporting | RHAIRFE-1727 | None — OpenCost | N/A |
| Budget alerts and controls | RHAIRFE-1728 | None — OCP Monitoring AlertManager rules | N/A |
| Loki-based showback | RHAISTRAT-1314 | None — Loki pipeline | N/A |

**Scorecard: 3 built (label gap closed 2026-07-17), 0 partial, 0 not built, 4 not WTO scope.**

---

## 8. GPU Resource Booking & Reservation

**WTO's role:** Capacity discovery only. Time-based booking requires a new Capacity Booking Service.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| Calendar-based GPU resource booking | RHAIRFE-2196, RHAIRFE-2202 | None — Capacity Booking Service (new component) | N/A |
| Advance GPU reservation | RHAIRFE-2604 | None — Capacity Booking Service | N/A |
| Self-service GPU booking | RHAIRFE-2646 | None — Capacity Booking Service | N/A |
| GPU capacity discovery and right-sizing | RHAIRFE-2645 | **Provider** — `satisfiableNodes` + DeviceClass enumeration | **Partial** — taint matching broken (H-2), no enumeration API |
| Self-service GPU pool provisioning | RHAIRFE-1790, RHAISTRAT-1450 | None — dashboard UI + Kueue LocalQueueDefaulting | N/A |

**Scorecard: 0 built, 1 partial, 0 not built, 4 not WTO scope.**

---

## 9. Workload Integration

**WTO's role:** Under Model C, WTO provides `resolvedSpec` for component teams and a universal pod-level fallback. Queue label injection enables Kueue integration for any workload type.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| Kueue manages ALL RHOAI workloads | RHAIRFE-549, RHAISTRAT-667 | **Model C: dual path** — pod webhook injects queue label universally; component teams apply `resolvedSpec` for CR-level Kueue integration | Pod webhook **built**; `resolvedSpec` **not built**; no Kueue CRD discovery |
| Kueue info in workbench overview | RHAIRFE-629, RHAISTRAT-497 | None — dashboard reads Kueue Workload status | N/A |
| Kueue info in model deployments overview | RHAIRFE-630, RHAISTRAT-432 | None — dashboard | N/A |
| Training job queuing through Kueue | RHAIRFE-1884 | **Fallback** — pod webhook handles if Training Operator doesn't consume `resolvedSpec` | **Built** (fallback path) |
| EvalHub + Kueue | RHAIRFE-1823, RHAISTRAT-1686 | **Fallback** — pod webhook handles | **Built** (fallback path) |
| Native RayService Kueue integration | RHAIRFE-2540, RHAISTRAT-2164 | None — upstream KubeRay work | N/A |
| Elastic Ray jobs in Kueue | RHAIRFE-909, OCPSTRAT-3126 | None — Kueue elastic jobs | N/A |
| Kueue support for KSO (Spark) | RHAIRFE-1186, RHAISTRAT-1477 | None — Kueue + KSO | N/A |
| vLLM batch inference with Kueue borrowing | RHAIRFE-1801 | None — Kueue borrowing | N/A |
| Data Science Pipelines GPU support | RHAISTRAT-354 | **Fallback** — pod webhook injects at pipeline step pod level | **Built** (fallback path) |
| Dynamic GPU scaling of training jobs | RHAIRFE-1343 | None — Kueue elastic jobs | N/A |
| Scale-to-zero for GPU inference | RHAISTRAT-1893 | None — KServe + WVA | N/A |

**Scorecard: 3 built (fallback paths), 0 partial, 1 not built (resolvedSpec), 8 not WTO scope.**

---

## 10. DRA Platform Capabilities

**WTO's role:** Primary owner of ResourceClaim lifecycle for RHOAI workloads. Creates ResourceClaimTemplates, injects claim references into pods.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| ResourceClaim support for workloads | OCPSTRAT-3133 | **Primary** — creates RCTs, injects claim refs into pods | **Built** |
| Partitionable devices (MIG via DRA) | OCPSTRAT-3055 | **Profile** — CEL selectors on partitioned devices | CRD ready, no validation/examples |
| Consumable capacity | OCPSTRAT-2398 | None — upstream K8s | N/A |
| Pod priority and preemption for DRA | OCPSTRAT-2383 | None — upstream K8s 1.38 | N/A |
| Cluster autoscaler + Karpenter with DRA | OCPSTRAT-1913 | None — upstream | N/A |
| DRA for networking | OCPSTRAT-2836 | None — upstream | N/A |
| DRA device quota tracking in Kueue | — | None — kueue-operator deviceClassMappings | N/A |

**Scorecard: 1 built, 1 CRD ready, 0 not built, 5 not WTO scope.**

---

## 11. Hardware Breadth

**WTO's role:** Profile definitions. WTO's CRD is hardware-agnostic by design — any device published via DRA or device-plugin can be targeted. Per-vendor GPU operators own the drivers.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| AMD MI355X/MI350X | RHAIRFE-786, RHAISTRAT-1444 | **Profile** — profiles targeting AMD DeviceClasses | CRD ready, no AMD example profiles |
| AMD Instinct quantization | RHAIRFE-2562 | None — vllm-rocm runtime | N/A |
| AMD SR-IOV dynamic VF allocation | RHAIRFE-1741 | None — AMD GPU Operator driver | N/A |
| Intel Crescent Island GPU | RHAIRFE-1287, RHAIRFE-726 | **Profile** — profiles targeting Intel DeviceClasses | CRD ready, no Intel example profiles |
| NVIDIA DGX Spark (GB10) | RHAIRFE-316, RHAISTRAT-1568 | **Profile** | CRD ready, only Tesla T4 examples exist |
| NVIDIA RTX 6000D (GB202) | RHAIRFE-2548 | **Profile** | CRD ready, no example |
| Confidential GPU (NVIDIA CC) | RHAIRFE-2133, RFE-7759 | **Placement** — profile carries nodeSelector for CC nodes | CRD ready, no CC example |
| Multi-device attestation | RFE-7012 | None — Trustee attestation service | N/A |

**Scorecard: 0 built (CRD is hardware-agnostic but no example profiles beyond Tesla T4), 3 not WTO scope.**

---

## 12. Migration & Backward Compatibility

**WTO's role:** None. WTO is greenfield — no HWP v1 migration path. Existing migrations (AP → HWP v1) are handled by rhods-operator.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| AP → HWP v1 migration | RHOAIENG-32011 | None — done by rhods-operator | N/A |
| HWP v1 → WTO WorkloadProfile | — | **Dropped** — no migration, WTO is greenfield | N/A |
| HWP API version migrations | RHOAIENG-32989 | None — rhods-operator | N/A |
| HWP webhook migration to module operator | RHOAIENG-62580 | None — rhods-operator | N/A |
| Kueue migration | RHAISTRAT-1175, RHAISTRAT-1711 | None — kueue-operator | N/A |

**Scorecard: No WTO work. Migration requirement 12.2 dropped.**

---

## 13. UX & Self-Service

**WTO's role:** Status API provider. Dashboard and SDK consumers read WorkloadProfile status conditions and `resolvedSpec` to power UI.

| Requirement | Evidence | WTO Role | Status |
|---|---|---|---|
| Kueue quota hierarchy visualization | RHAIRFE-1787, RHAISTRAT-1577 | None — dashboard reads Kueue CRDs directly | N/A |
| Kueue local queues exposed to end users | RHAIRFE-452 | None — Kueue RBAC | N/A |
| Kueue RBAC improvement | RHAISTRAT-476 | None — Kueue ClusterRole definitions | N/A |
| Namespace admin limits Kueue access | RHAIRFE-563 | None — Kueue Role/RoleBinding | N/A |
| GPU resource visibility for platform admins | RHAISTRAT-1819, RHAIRFE-2149 | None — dashboard | N/A |
| Kueue admission feedback for HWPs | RHOAIENG-62285 | **Provider** — `QuotaFit` + `QueueReady` conditions, `quotaSummary` | **Not built** (M-7) |
| HWP selection from Sandbox CR | RHOAIENG-73190 | **Provider** — WorkloadProfile list API | CRD exists, status incomplete |
| Gang scheduling impact on HWP/UX | RHOAIENG-72960 | None — dashboard UX design | N/A |
| Topology-aware scheduling impact on HWP/UX | RHOAIENG-72959 | None — dashboard UX design | N/A |
| GitOps-native Kueue queue management | RHAISTRAT-1804 | None — Kueue CRDs are already declarative | N/A |

**Scorecard: 0 built, 1 partial (CRD exists), 1 not built, 8 not WTO scope.**

---

## Summary Scorecard

| Category | Built | Partial | Not Built | Not WTO Scope |
|---|---|---|---|---|
| 1. GPU Device Selection | 2 | 2 | 2 | 3 |
| 2. HWP Lifecycle | 5 | 4 | 2 | 1 |
| 3. Quota & Admission | 1 | 1 | 1 | 3 |
| 4. Fair-Share & Multi-Tenancy | 2 | 0 | 0 | 5 |
| 5. Topology-Aware Placement | 2 | 1 | 0 | 1 |
| 6. GPU Observability & Health | 0 | 0 | 2 | 7 |
| 7. Cost Attribution & Chargeback | 0 | 0 | 3 | 4 |
| 8. GPU Booking & Reservation | 0 | 1 | 0 | 4 |
| 9. Workload Integration | 3 | 0 | 1 | 8 |
| 10. DRA Platform | 1 | 1 | 0 | 5 |
| 11. Hardware Breadth | 0 | 0 | 0 | 5 |
| 12. Migration | 0 | 0 | 0 | 5 |
| 13. UX & Self-Service | 0 | 1 | 1 | 8 |
| **Total** | **16** | **11** | **12** | **59** |

Of the 39 requirements in WTO's scope: **16 built, 11 partial, 12 not built.**

---

## Top Gaps for GPUaaS MVP

Ordered by number of categories blocked.

### 1. `resolvedSpec` contract (Model C foundation)

**Blocks:** Categories 2, 6, 7, 9, 13 (5 categories, 7+ requirements)

WorkloadProfile status must expose the fully-resolved spec (template + overrides merged) as a stable API that component teams and the dashboard consume. This is the foundation of Model C — without it, every consumer reimplements profile resolution and the HWP bug class recurs.

**Status: CLOSED (2026-07-17).** `resolvedSpec` is written to `WorkloadProfile.Status.ResolvedSpec` by the profile controller. Consumer SDK not yet formalized.

### 2. Two-CRD refactor (WorkloadProfileTemplate + WorkloadProfile)

**Blocks:** Categories 2, 8 (template reuse, drift detection, capacity discovery)

**Status: CLOSED (2026-07-17).** Two-CRD model implemented: cluster-scoped `WorkloadProfileTemplate` + namespace-scoped `WorkloadProfile` with `templateRef`, `namespaceSelector` ACL, and template resolution in the profile controller. Additionally, a third CRD `WorkloadTypeConfig` was added for pluggable workload type registration.

**Design:** `template-binding-design.md`

### 3. `QuotaFit` / `QueueReady` status conditions

**Blocks:** Categories 3, 13 (quota visibility, Kueue admission feedback)

Constants are declared in Go types but never set by any controller. The profile controller needs to read Kueue LocalQueue objects (for `QueueReady`) and check namespace ResourceQuota + DRA device quotas (for `QuotaFit`). Requires new RBAC for Kueue CRDs.

**Known issue:** M-7

### 4. Profile name as pod label (not just annotation)

**Blocks:** Categories 6, 7 (observability, cost attribution)

**Status: CLOSED (2026-07-17).** The webhook now sets both `workload-template.io/profile-name` and `workload-template.io/template-name` as pod labels via `setCostLabels()`. Prometheus and Cost Management can filter by profile and template.

### 5. Init container support

**Blocks:** Category 2 (per-container targeting completeness)

The webhook only iterates `pod.Spec.Containers`, silently skipping init containers. Workloads with GPU init containers (driver toolkit init, data loading) get no resources injected.

**Known issue:** M-1

### 6. Drift detection (ADR-010)

**Blocks:** Category 2 (profile change propagation, cleanup on profile switch)

ADR-010 designs drift detection: compare `profile-generation` annotation on running pods against current profile generation. Report `Drifted` condition. No code exists.

**Depends on:** Two-CRD refactor provides `templateGeneration` vs `resolvedGeneration` comparison for free.

### 7. Device taint tolerations on `deviceClaims`

**Blocks:** Category 1 (device taints, K8s 1.35+)

DRADeviceTaints (beta in K8s 1.36) allow devices to be tainted. WTO-injected DRA claims need a toleration field so profiles can target tainted devices. The CRD currently has no such field.

### 8. Multi-claim container targeting fix (H-1)

**Blocks:** Category 1, 10 (DRA correctness)

DRA claims are injected into the wrong container in multi-container pods. The `injectDRAClaims` function doesn't correctly map claims to their targeted containers.

**Known issue:** H-1

### 9. Pre-flight quota check — multi-container fix (H-3)

**Blocks:** Category 3 (quota validation correctness)

`resolveProfileResources` only examines the first container's resources when checking quota. Multi-container pods can bypass quota.

**Known issue:** H-3

### 10. Hardware breadth example profiles

**Blocks:** Category 11 (non-NVIDIA hardware validation)

Only Tesla T4 example profiles exist. Need example profiles for AMD MI300X/MI355X, Intel Crescent Island, NVIDIA H100/A100/GB10, and confidential GPU nodes to validate CRD expressiveness.

---

## Requirements Not in WTO Scope

These require new components or are owned by other teams. WTO's interface to each is noted.

| Component | Requirements Served | WTO Interface |
|---|---|---|
| **Kueue** | Fair-share scheduling, hierarchical quota, gang scheduling, elastic jobs, topology-aware placement | WTO assigns workloads to queues via `placement.queue`; Kueue does the math |
| **K8s DRA (upstream)** | Partitionable devices, device taints, preemption, consumable capacity, extended resource bridge | WTO embeds native DRA types; upstream features arrive automatically |
| **Vendor GPU Operators** (NVIDIA, AMD, Intel) | Driver lifecycle, device health monitoring, GPU card failure alerts, hardware-specific features | WTO profiles target vendor DeviceClasses; operators publish devices via DRA |
| **odh-dashboard** | Quota hierarchy visualization, Kueue info in workbench/model overview, profile selection UI, GPU resource visibility | Dashboard reads `WorkloadProfile.status.resolvedSpec`, `quotaSummary`, and conditions |
| **COO** (Cluster Observability Operator) | GPU topology dashboards, cost visualization, per-team utilization dashboards | COO Perses dashboards query DCGM + Kueue metrics; WTO profile labels enable workload correlation |
| **OpenCost** | Per-tenant GPU usage, chargeback, showback | OpenCost attributes cost by pod labels; WTO provides profile-name label (gap #4) |
| **Capacity Booking Service** (new) | Calendar-based GPU booking, advance reservation, self-service provisioning | Does not exist yet; would translate reservations into Kueue quota adjustments |
| **Device Reclaim Controller** (new) | Idle GPU reclaim | Does not exist yet; would monitor DCGM utilization and suspend/evict idle workloads |
