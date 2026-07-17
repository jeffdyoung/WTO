# Architecture Decisions

This document records the architectural decisions made during WTO's design and the reasoning behind them. Each decision includes the alternatives considered and why they were rejected.

## Table of Contents

- [ADR-001: Pod-Level Injection](#adr-001-pod-level-injection)
- [ADR-002: Scheduling Gate Pattern](#adr-002-scheduling-gate-pattern)
- [ADR-003: Conflict Resolution Semantics](#adr-003-conflict-resolution-semantics)
- [ADR-004: Embed Native Kubernetes Types](#adr-004-embed-native-kubernetes-types)
- [ADR-005: Pre-flight Quota Validation](#adr-005-pre-flight-quota-validation)
- [ADR-006: Kueue Discovery, Not Management](#adr-006-kueue-discovery-not-management)
- [ADR-007: DRA with Device Plugin Fallback](#adr-007-dra-with-device-plugin-fallback)
- [ADR-008: Single CRD](#adr-008-single-crd)
- [ADR-009: Failure Policy](#adr-009-failure-policy)
- [ADR-010: Source of Truth with Soft Enforcement](#adr-010-source-of-truth-with-soft-enforcement)
- [ADR-011: UI Metadata in Annotations](#adr-011-ui-metadata-in-annotations)
- [ADR-012: Per-Container Targeting](#adr-012-per-container-targeting)
- [ADR-013: Webhook Ordering for Kueue Integration](#adr-013-webhook-ordering-for-kueue-integration)
- [ADR-014: Two-CRD Template-Binding Model](#adr-014-two-crd-template-binding-model)
- [ADR-015: Placement is Binding-Only](#adr-015-placement-is-binding-only)
- [ADR-016: namespaceSelector for Template Access Control](#adr-016-namespaceselector-for-template-access-control)

---

## ADR-001: Pod-Level Injection

**Status:** Accepted

**Context:** WTO must inject resources, DRA claims, tolerations, nodeSelectors, and Kueue labels into workloads. The injection can happen at different levels of the controller hierarchy: the workload CR (Notebook, InferenceService, Deployment), the intermediate controller (StatefulSet, ReplicaSet), or the pod.

**Decision:** WTO mutates pods only. It does not mutate Notebooks, InferenceServices, Deployments, StatefulSets, Jobs, or any other workload-level resource.

**Rationale:**

### Workload-CR mutation creates per-type coupling

A webhook that mutates workload CRs must define explicit JSON paths for every supported type. Each workload type structures its pod template differently:

| Kind | Containers path | NodeSelector path | Tolerations path |
|---|---|---|---|
| Notebook | `spec.template.spec.containers` | `spec.template.spec.nodeSelector` | `spec.template.spec.tolerations` |
| InferenceService | `spec.predictor.model` (flat map) | `spec.predictor.nodeSelector` | `spec.predictor.tolerations` |
| LLMInferenceService | `spec.template.containers` | `spec.template.nodeSelector` | `spec.template.tolerations` |
| Job | `spec.template.spec.containers` | `spec.template.spec.nodeSelector` | `spec.template.spec.tolerations` |
| PyTorchJob | `spec.pytorchReplicaSpecs.Worker.template.spec.containers` | ... | ... |

Every new workload type requires new path configurations, new webhook rules, new container-targeting logic, and new tests. The operator grows linearly with the number of supported workload types.

Pod-level injection requires one webhook rule, one set of injection logic, zero per-type configuration. Any controller that creates pods — including controllers that don't exist yet — gets WTO treatment automatically.

### Container targeting becomes heuristic-based

When mutating a workload CR, the webhook must guess which container should receive GPU resources. Different workload types use different conventions: some name the main container after the CR, some use a hardcoded name like `"main"`, some don't use a containers array at all. When the heuristic finds no match, the webhook must choose between blocking admission (breaking the workload) or skipping injection (silent failure — workload runs without GPU).

Pod-level injection enables explicit per-container targeting by name or index with a defaults fallback (see ADR-012). The profile author declares what they want. No guessing.

### GitOps tools detect workload-CR mutation as drift

A mutating webhook that modifies a workload CR's spec at admission time creates a gap between what is stored in Git and what is persisted in etcd. ArgoCD and Flux detect webhook-injected fields (resources, nodeSelector, tolerations, labels) as drift and attempt to revert them. The webhook re-mutates on the next admission, creating an infinite reconciliation loop. Workarounds exist (ArgoCD's `ignoreDifferences`, Flux's `fieldManager` config) but push complexity to every GitOps user.

Pod-level injection avoids this entirely. Pods are ephemeral resources that GitOps tools do not track for drift. The workload CR stored in etcd matches what is in Git.

### Workload-CR mutation causes unintended rollouts

Modifying a Deployment or StatefulSet's pod template spec triggers a rolling update. If a profile change causes re-injection into the workload CR on the next admission (e.g., an unrelated label update on the Notebook), it triggers a rollout — unplanned restarts of inference services and training jobs.

Pod-level injection never modifies workload CRs. Profile changes take effect on the next natural pod lifecycle event (restart, scale-up, eviction, manual rollout). A 3-day training run is not killed because an admin updated a profile's memory setting. See ADR-010.

### DRA requires pod-level injection regardless

On Kubernetes 1.34+, `pod.spec.resourceClaims` and `container.resources` are immutable after pod creation — even on gated pods (see ADR-002). DRA device claims must be set at pod CREATE time via a mutating admission webhook. Even if WTO mutated workload CRs for resource requirements and placement, it would still need a pod-level webhook for DRA injection. A workload-CR-level approach cannot fully replace pod-level mutation in a DRA world, so it would require maintaining two injection paths.

### Dashboard integration is simpler

With pod-level injection, the Dashboard writes one annotation (`workload-template.io/profile-name`) and reads WorkloadProfile status conditions (`Valid`, `DeviceClassAvailable`, `satisfiableNodes`). All injection logic is WTO's responsibility. The Dashboard does not need to understand resource injection semantics, per-type container paths, or scheduling configuration.

**Consequences:**

The pod's owning workload CR does not reflect injected resources in its pod template. WTO compensates by writing observability annotations on the owning workload (`workload-template.io/applied-summary`, `workload-template.io/applied-generation`) via the owner reference chain. These annotations are informational — they do not trigger reconciliation by the owning controller.

The `workload-template.io/profile-name` annotation must be present on the pod at creation time. The user (or their tooling) is responsible for placing this annotation where the workload controller will propagate it to the pod template. For Deployments and Jobs, this is `spec.template.metadata.annotations`. For Notebooks, the annotation should be on both the Notebook CR metadata and `spec.template.metadata.annotations` — the notebook controller propagates CR-level annotations to the StatefulSet pod template.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **Mutate workload CRs** (Notebook, InferenceService, Deployment) | Per-type path coupling, heuristic container targeting, GitOps drift, rollout side effects, and cannot handle DRA injection (immutable after pod creation). Requires maintaining two injection paths (workload-CR + pod-level for DRA). |
| **Mutate intermediate controllers** (StatefulSet, ReplicaSet) | Same GitOps problem. Also requires WTO to understand the ownership chain for every workload type. A Notebook creates a StatefulSet; an InferenceService creates a Deployment or Knative Service. WTO would need per-type logic to find the right intermediate object. |
| **Dual-level injection** (workload + pod) | Complexity with no clear benefit. The workload-level mutation still fights GitOps. The pod-level mutation via scheduling gate is sufficient — every pod creation is intercepted regardless of the controller that created it. |

**Why pod-level works:**

- **Universal.** Any controller that creates pods — Notebook, InferenceService, Deployment, StatefulSet, Job, CronJob, PyTorchJob, RayJob, Spark, custom controllers — automatically gets WTO treatment. No webhook configuration changes, no per-type mutation logic.
- **No rollout side effects.** Profile changes do not trigger rolling updates. Changes apply on the next natural pod lifecycle event (restart, scale-up, eviction, manual rollout).
- **No GitOps fights.** GitOps tools manage workload CRs. WTO mutates pods. Pods are ephemeral resources that GitOps tools do not track for drift.
- **Scheduling gate safety.** The gate guarantees no pod runs without WTO processing. Without gates, a pod could be scheduled before mutation completes.
- **DRA-mandatory.** Pod-level injection is required for DRA regardless — `pod.spec.resourceClaims` can only be set at pod CREATE time.

---

## ADR-002: Webhook Injection + Controller Placement

**Status:** Accepted (revised after spike validation, 2026-07-01)

**Context:** WTO needs to inject resources, set DRA claim references, validate quota, and set placement constraints before a pod is scheduled. The original design had the webhook only adding a scheduling gate, with all logic in the controller.

**Spike finding:** On Kubernetes 1.34 (OpenShift 4.21), `pod.spec.resourceClaims`, `container.resources.requests`, and `container.resources.limits` are **immutable after pod creation**, even on gated pods. The API server rejects updates to these fields with `spec: Forbidden: pod updates may not change fields other than...`. Only `nodeSelector`, `tolerations` (additions), `nodeAffinity`, and `labels` are mutable on gated pods. This was validated empirically — see `spike/FINDINGS.md`.

**Decision:** The webhook and controller split responsibilities along the Kubernetes mutability boundary:

**Webhook (synchronous, during CREATE admission) — immutable fields:**
- Read WorkloadProfile from informer cache
- Inject `container.resources` (requests/limits) into targeted containers
- Inject `pod.spec.resourceClaims[]` referencing pre-created ResourceClaimTemplates
- Inject `container.resources.claims[]` linking containers to device claims
- Add scheduling gate
- Set tracking annotations

**Controller (async, reconciles gated pods) — mutable fields + validation:**
- Pre-flight quota checks (ResourceQuota, ClusterQueue)
- Set `nodeSelector` and `tolerations` (Node placement mode)
- Set `kueue.x-k8s.io/queue-name` label (Queue placement mode)
- Remove scheduling gate

The scheduling gate still serves its original purpose: it holds the pod so the controller can validate quota and set placement before the pod reaches the scheduler. The webhook adds the gate alongside its injection work.

**Consequences:**

The webhook is no longer minimal. It reads WorkloadProfile CRs (from informer cache, not live API calls) and performs non-trivial mutation. Webhook availability is critical — `failurePolicy: Fail` means webhook downtime blocks pod creation in opted-in namespaces. Multiple replicas, anti-affinity, and `system-cluster-critical` priority are required.

ResourceClaimTemplates must exist before the webhook references them. The Profile Controller creates and maintains templates when WorkloadProfiles are created or updated. The webhook references them by name (`wto-<profile>-<claim>`) — it does not create templates during admission.

Profile changes do not propagate to pods that have already been created. Resources and DRA claims are set at CREATE time and cannot be modified afterward. This is consistent with ADR-010 (soft enforcement) but sharper — there is no mechanism to update these fields, even in theory.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **Webhook adds gate only, controller does everything** (original design) | Does not work. `pod.spec.resourceClaims` and `container.resources` are immutable after creation. The controller cannot inject these fields into a gated pod — the API server rejects the update. Validated on K8s 1.34. |
| **Do everything in the webhook, no controller** | Quota validation involves checking ResourceQuota and ClusterQueue state, which can be transiently unavailable. Blocking pod creation synchronously on quota checks is too strict — a brief Kueue outage would block all pod creation. The controller allows async retry with backoff. Also, `nodeSelector` and `tolerations` can be set by the controller, keeping the webhook focused on immutable fields. |
| **Controller creates ResourceClaimTemplates per pod** | The controller can create objects, but it cannot add `pod.spec.resourceClaims` to reference them (immutable). The template reference must exist in the pod spec at CREATE time, which means the webhook must set it, which means the template must already exist. Per-pod template creation by the controller has a chicken-and-egg problem. |

**Precedent:** The Multiarch Tuning Operator uses a similar split: the webhook adds a scheduling gate, and the PodReconciler modifies `nodeAffinity` (which IS mutable on gated pods) and removes the gate. WTO extends this pattern — the webhook does more (resource + DRA injection) because WTO manages more immutable fields.

---

## ADR-003: Conflict Resolution Semantics

**Status:** Accepted

**Context:** Workloads may arrive with resources, tolerations, nodeSelectors, or DRA claims already specified — set by users, GitOps pipelines, or other admission webhooks. When a workload also carries a WTO profile annotation, WTO must decide how to handle the overlap.

**Decision:** WTO defines three categories of fields with different conflict behavior:

**Owned fields — profile takes precedence:**
- `container.resources.requests` and `limits` for resources named in the profile
- `container.resources.claims` (DRA claim references)
- `pod.spec.resourceClaims` (DRA ResourceClaimTemplate references)
- `kueue.x-k8s.io/queue-name` label (Queue placement mode)

When the workload specifies a value for an owned field and the profile specifies a different value, the profile wins. WTO emits a Warning event on the pod listing the overridden fields and sets the annotation `workload-template.io/overrides` with the list of overridden field paths.

**Merged fields — additive, never removes:**
- `pod.spec.tolerations` — profile tolerations are appended alongside existing tolerations
- `pod.spec.nodeSelector` — profile keys are added alongside existing keys

Existing tolerations and nodeSelector entries that the profile does not specify are preserved. A user's `topology.kubernetes.io/zone: us-east-1a` nodeSelector coexists with the profile's `nvidia.com/gpu.product: Tesla-T4` nodeSelector.

**Untouched fields — WTO never modifies:**
- `pod.spec.nodeAffinity` (user's zone, topology, or custom affinity rules)
- `pod.spec.volumes`, `volumeMounts`
- `pod.spec.serviceAccountName`
- `container.image`, `command`, `args`, `env`
- `container.resources.requests` for resource names NOT in the profile
- All other pod spec fields

**Blocking conflicts — pod stays gated:**

Some conflicts indicate structural contradictions that would cause silent failures:

- Workload has existing `pod.spec.resourceClaims` AND profile has `deviceClaims` → duplicate device allocation risk
- Workload has `kueue.x-k8s.io/queue-name` label targeting a different queue than the profile specifies → ambiguous queue assignment
- Workload has a nodeSelector key that directly contradicts a profile nodeSelector key (same key, different value) → unsatisfiable constraints

For blocking conflicts, the pod stays gated with an error event explaining the contradiction and how to resolve it.

**Consequences:**

Users who deploy workloads via GitOps with a profile annotation should omit resource specifications from their manifests and let the profile govern. If they include resources, WTO overrides them and logs the action. This is predictable — the profile annotation means "WTO governs hardware placement."

Tolerations and nodeSelectors from non-hardware concerns (team placement, zone affinity, compliance taints) are always preserved. WTO only adds to these fields, never removes.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **Workload always wins** (skip conflicting fields) | The profile annotation would give a false impression of governance. A user sets `memory: 8Gi` but the profile says `memory: 64Gi` — which is the workload actually running with? Silent inconsistency between declared profile and actual resources. |
| **Reject all conflicts** (pod stays gated) | Too strict for real-world use. Legitimate scenarios exist: a workload has a custom toleration for a team taint AND uses a WTO profile for GPU resources. Rejecting this forces users to remove all pod spec customization to use WTO — adoption blocker. |
| **Full merge everywhere** (union of workload + profile) | Ambiguous for resources. If the workload requests `cpu: 4` and the profile requests `cpu: 16`, a merge to `max(4, 16) = 16` is one interpretation, but `sum(4, 16) = 20` is another. No merge semantic is universally correct for resource quantities. |

---

## ADR-004: Embed Native Kubernetes Types

**Status:** Accepted

**Context:** WTO needs types to represent resource requirements and DRA device requests in the WorkloadProfile CRD. These can be custom types designed for WTO's use case, or native Kubernetes types embedded directly.

**Decision:** The WorkloadProfile CRD embeds native Kubernetes types wherever one exists:

| WTO field | Embedded type | Pod spec target |
|---|---|---|
| `defaults.resources` / `containers[].resources` | `corev1.ResourceRequirements` | `container.resources` |
| `deviceClaims[].request` | `resource.k8s.io/v1.DeviceRequest` | `pod.spec.resourceClaims[]` via ResourceClaimTemplate |
| `placement.node.tolerations` | `[]corev1.Toleration` | `pod.spec.tolerations` |
| `placement.node.nodeSelector` | `map[string]string` | `pod.spec.nodeSelector` |

**Consequences:**

The webhook is pass-through for embedded types: it copies the struct into the pod spec (or ResourceClaimTemplate) without field-by-field mapping. When upstream Kubernetes adds fields to `corev1.ResourceRequirements` or `resource.k8s.io/v1.DeviceRequest`, WTO inherits them automatically after a Go dependency bump and CRD regeneration via `controller-gen`. No WTO code changes are needed for additive upstream evolution.

The trade-off is that WTO inherits the full surface area of embedded types, including fields that may not be relevant in a profile context (e.g., `DeviceRequest.adminAccess`). This is a dashboard/documentation concern, not a schema problem — the CRD is correct, and UIs can choose which fields to expose.

**WorkloadProfile is an admin/platform-team API, not an end-user API.** The `deviceClaims` field requires writing CEL expressions like `device.attributes["gpu.nvidia.com"].productName == "Tesla T4"`. This is powerful and correct — it exposes the full DRA expression language without WTO inventing a parallel abstraction. But it is not reasonable to expect data scientists or ML engineers to write CEL expressions. The expected usage model is:

1. **Platform admins** create WorkloadProfiles with appropriate resource requirements, CEL device selectors, and placement configuration.
2. **End users** (data scientists, ML engineers) select profiles by name — via the Dashboard dropdown, a workload annotation, or a namespace default.
3. **Dashboards** present profiles as human-readable choices (display name, description, GPU type, queue) and hide the CEL/DRA complexity behind annotations.

If WTO profiles are exposed directly to end users without a dashboard or CLI abstraction, adoption will suffer. The CEL expression syntax, DRA ResourceClaimTemplate lifecycle, and placement discriminated union are implementation details that belong behind a UX layer.

If upstream Kubernetes makes a breaking API version change (e.g., `resource.k8s.io/v2`), that requires the same migration effort as any Go import change — a new WTO CRD version with a conversion webhook.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **Custom types mirroring K8s types** | Every upstream field addition requires WTO to add a corresponding field, update mapping logic, release, and regenerate CRDs. Perpetual catch-up with upstream. This is the mistake made by predecessor systems that defined custom `HardwareIdentifier` types with `minCount`/`maxCount`/`defaultCount` — a taxonomy that added no value and diverged from the Kubernetes resource model. |
| **String-based references** (reference a ConfigMap containing serialized ResourceRequirements) | No schema validation at admission time. Typos in resource names or quantities are caught at pod scheduling, not at profile creation. Debugging requires reading ConfigMap contents, not `kubectl get workloadprofile -o yaml`. |
| **Thin wrapper types** (WTO struct with a single embedded K8s type field) | Adds a nesting level with no semantic value. `resources: { inner: { requests: ... } }` vs `resources: { requests: ... }`. The wrapper provides no additional validation or documentation surface. |

---

## ADR-005: Pre-flight Quota Validation

**Status:** Accepted

**Context:** When a pod is gated, WTO has a window to check whether the workload's resource requirements fit within available quota before ungating. Quota enforcement is ultimately the responsibility of Kueue (ClusterQueue quotas) and Kubernetes (namespace ResourceQuota), but violations discovered after ungating result in poor user experience — pods stuck Pending with events that don't reference the profile or explain what to change.

**Decision:** WTO performs pre-flight quota checks at two levels:

1. **Profile Controller** (continuous) — validates WorkloadProfile CRs against namespace ResourceQuota and ClusterQueue quota. Reports `QuotaFit` condition and `quotaSummary` on the profile status. Dashboards read this to show quota fitness before workload creation.

2. **Placement Controller** (per-pod, at gate time) — checks remaining quota capacity before ungating a pod. If quota is insufficient, the pod stays gated with an actionable event. The controller re-checks periodically (configurable interval, default 30s). When quota frees up, the pod proceeds automatically.

WTO is a **pre-flight validator, not a quota enforcer.** Kueue and ResourceQuota remain the authoritative enforcement mechanisms. WTO's checks are best-effort — a TOCTOU race exists between WTO's check and the pod reaching Kueue/ResourceQuota. This is acceptable because:

- 95% of quota violations are caught at the gate, with actionable, profile-aware messages
- The remaining 5% are caught by Kueue/ResourceQuota as they are today
- WTO never rejects pods — it holds them gated until quota is available, which is recoverable

**Consequences:**

WTO reads ResourceQuota, ClusterQueue, and LocalQueue objects. It never writes to them. It aggregates their state into WorkloadProfile status and uses it for per-pod pre-flight checks.

Profiles that are structurally impossible (request exceeds namespace quota ceiling, not just current usage) are distinguished from transiently unavailable (quota currently full but would fit if other workloads finish). Structural impossibility is reported as a persistent condition; transient unavailability triggers gated retry.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **WTO enforces quota directly** (reject pods that exceed quota) | Duplicates Kueue's and ResourceQuota's enforcement with a different implementation, creating inconsistency. Two systems disagreeing on quota availability causes user confusion. WTO would need to track quota consumption itself, duplicating state. |
| **No quota awareness** (let Kueue/ResourceQuota handle everything) | The status quo. Works, but users discover quota violations minutes after submission via cryptic Kueue events or `FailedResourceClaimCreation` async errors. No profile-aware context ("use a smaller profile" or "contact your admin to increase quota"). No pre-creation feedback in dashboards. |
| **WTO manages quota objects** (create/update ResourceQuota or ClusterQueue resources) | Conflicts with Kueue's ownership of ClusterQueue quotas and admin ownership of namespace ResourceQuota. WTO should never write to resources it does not own. |

---

## ADR-006: Kueue Discovery, Not Management

**Status:** Accepted

**Context:** WTO's Queue placement mode sets the `kueue.x-k8s.io/queue-name` label on pods, integrating with Kueue for admission control, quota, and fair sharing. WTO needs to understand the Kueue topology (ClusterQueues, LocalQueues, ResourceFlavors, deviceClassMappings) to validate profiles and report quota status.

**Decision:** WTO discovers Kueue infrastructure by reading existing objects. It never creates, updates, or deletes Kueue resources.

WTO works with any Kueue installation method: kueue-operator, upstream Helm chart, manual deployment, or platform-managed. It lists ClusterQueues, LocalQueues, ResourceFlavors, and (if available) the Kueue operator CR to understand the quota topology, framework integrations, and DRA deviceClassMappings.

If Kueue is not installed (CRDs absent), Queue placement mode is unavailable. Profiles using `placement.type: Queue` are marked `QueueReady: False` with guidance to install Kueue or switch to Node placement. Node placement and DRA work independently of Kueue.

If Kueue is installed but missing configuration WTO depends on (no `deviceClassMappings`, required framework integrations disabled, LocalQueue not found in namespace), WTO reports the specific gap in status conditions with actionable remediation steps.

**Consequences:**

WTO has no dependency on how Kueue was installed or who manages it. A cluster where the HPC team installed Kueue three years ago works the same as a fresh install. WTO adapts to whatever exists.

WTO cannot fix Kueue misconfigurations. If a ClusterQueue doesn't cover a DeviceClass, WTO reports the gap but cannot add the `deviceClassMapping`. The Kueue admin must act. WTO's status conditions serve as the communication channel.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **WTO manages Kueue resources** (creates ClusterQueues, LocalQueues, ResourceFlavors) | Conflicts with external Kueue management. If a platform operator already manages Kueue, two controllers writing to the same objects causes reconciliation fights. WTO would need to understand Kueue's full configuration surface (cohorts, borrowing limits, preemption policies, fair sharing weights) — scope it should not own. |
| **WTO requires a specific Kueue installation method** | Adoption blocker. Clusters with existing Kueue installations would need to re-deploy Kueue in a WTO-compatible way. |
| **Kueue is a hard dependency** | Clusters that need Node placement (direct nodeSelector/tolerations) without Kueue would be forced to install it. Kueue is valuable but optional — WTO should not mandate it. |

---

## ADR-007: DRA with Device Plugin Fallback

**Status:** Accepted

**Context:** DRA (`resource.k8s.io/v1`) is GA in Kubernetes 1.34 and provides attribute-based device selection via CEL expressions. However, many production clusters still use the device plugin model (`nvidia.com/gpu: 1`) — the DRA driver may not be installed, Kueue may lack `deviceClassMappings`, or the cluster may be on an older Kubernetes version.

**Decision:** WTO supports both models and auto-detects which to use:

1. **DRA mode** (preferred): profiles use `deviceClaims[]` with embedded `DeviceRequest`. WTO generates ResourceClaimTemplates. Activated when DRA DeviceClasses exist on the cluster and (if Queue placement) Kueue has `deviceClassMappings`.

2. **Device plugin mode** (fallback): profiles use `defaults.resources.requests["nvidia.com/gpu"]`. WTO injects the resource request directly into the container spec. Activated when DRA DeviceClasses are absent or the cluster is below Kubernetes 1.34.

WTO reports the active mode via the `DRAEnabled` status condition on each WorkloadProfile. Profile authors can use either model explicitly — WTO does not auto-convert between them.

**Consequences:**

WTO works on clusters at any point in the device-plugin-to-DRA transition. A profile using `deviceClaims` on a cluster without DRA is marked `DeviceClassAvailable: False` with guidance to install the DRA driver. A profile using device plugin resources on a DRA-enabled cluster works but misses DRA benefits (CEL selectors, topology constraints, MIG profiles).

A single cluster can have profiles using both models simultaneously — some workloads using DRA, others using device plugins. WTO handles each profile according to its own configuration.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **DRA only** | Adoption blocker. Many production clusters do not have DRA drivers installed. Clusters on Kubernetes < 1.34 cannot use DRA at all. Requiring DRA would exclude a large fraction of the target install base. |
| **Device plugin only** | Misses the architectural direction of Kubernetes device management. DRA is GA and provides capabilities (GPU model selection, MIG profiles, topology constraints) that device plugins cannot express. Building on a model that upstream is moving away from creates technical debt. |
| **Auto-convert device plugin profiles to DRA** | Lossy conversion. `nvidia.com/gpu: 1` has no GPU model, memory, or topology information. WTO would have to guess or use a catch-all DeviceClass. The resulting DRA claim would not provide the selection benefits that justify DRA. Better to let profile authors choose the model explicitly. |

---

## ADR-008: Single CRD

**Status:** Superseded by ADR-014

**Context:** The WorkloadProfile CRD bundles resource requirements, DRA device claims, and placement configuration in a single object. An alternative design decomposes these into separate CRDs (e.g., ResourceProfile + PlacementPolicy).

**Decision:** ~~WTO uses a single `WorkloadProfile` CRD.~~ This decision was superseded by ADR-014, which introduces a two-CRD model: cluster-scoped `WorkloadProfileTemplate` for admin-managed hardware definitions and namespace-scoped `WorkloadProfile` for tenant bindings with placement. Resource requirements, device claims, and placement are structurally separated within the spec (distinct fields, not interleaved) but not split into separate objects.

**Consequences:**

One annotation on a workload, one object to manage, one status to read. Users and dashboards interact with a single resource. The "one hardware choice" mental model is preserved — selecting a profile determines resources, devices, and placement in one action.

Reuse of placement across profiles requires duplication. If ten profiles share the same Queue placement, the `localQueueName` and `priorityClass` are specified in each profile independently. This is acceptable because placement configuration is small (two fields for Queue mode) and profiles are typically managed by automation or dashboards, not hand-edited.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **Two CRDs** (ResourceProfile + PlacementPolicy) | Maximum composability — same ResourceProfile with different PlacementPolicies per namespace. But fragments the user experience: two annotations, two objects to manage, two lookups in the webhook, two status resources to check. Dashboard must present pairs or pre-link them. The composability benefit is real but does not justify the UX cost for the common case where profiles are managed by platforms or admins, not composed ad-hoc by users. |
| **Thin coordinator** (WorkloadProfile references ConfigMap + ResourceClaimTemplate + PlacementPolicy) | Maximum Kubernetes-nativeness — every concern lives in the object type designed for it. But three or more objects to manage per "hardware choice." ConfigMap values are untyped strings with no schema validation. Multiple webhook lookups. Operational complexity outweighs architectural purity. |

---

## ADR-009: Failure Policy

**Status:** Accepted

**Context:** Admission webhooks have a `failurePolicy` that determines behavior when the webhook is unavailable: `Fail` (reject the request) or `Ignore` (admit the request without mutation).

**Decision:** WTO's webhook uses `failurePolicy: Fail`.

If the webhook is unavailable, pod creation is blocked rather than allowing pods to admit without hardware configuration. A pod that needs 4x A100 GPUs but admits without resource injection silently fails — it gets scheduled without GPU access, the workload produces wrong results or crashes, and the failure mode is not obvious.

**Consequences:**

WTO webhook availability is on the critical path for pod creation. WTO must be highly available: multiple replicas with anti-affinity, liveness/readiness probes, and `system-cluster-critical` priority class.

WTO must not intercept pods it does not manage. The webhook uses an object selector (or namespace selector) to match only pods with the `workload-template.io/profile-name` annotation. Pods without the annotation bypass the webhook entirely — WTO downtime does not affect unrelated workloads.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **failurePolicy: Ignore** | Safe for CPU architecture affinity — worst case, a pod lands on an incompatible node and fails to start, which is visible and recoverable. Not safe for GPU resource injection — a pod admitting without GPU resources runs on a CPU node, starts successfully, and produces incorrect results (training with no GPU acceleration) or hangs on CUDA calls with no clear error. Silent data corruption is worse than blocked creation. |
| **Separate webhooks with different policies** | Running two webhooks (Ignore for optional features, Fail for mandatory features) adds operational complexity. Which features are mandatory vs optional would need per-profile configuration. The scheduling gate already mitigates the downside of Fail — pods are gated, not running, so a brief WTO outage results in a queue of gated pods that process once WTO recovers. |

---

## ADR-010: Source of Truth with Soft Enforcement

**Status:** Accepted

**Context:** WTO continuously validates WorkloadProfiles against cluster state and can detect when a running workload's resources no longer match its declared profile (drift). The question is whether WTO should actively correct drift by re-patching running workloads.

**Decision:** WTO detects and reports drift but does not autonomously modify running workloads. Drift correction happens on the next pod lifecycle event (restart, scale-up, eviction, manual rollout).

When a WorkloadProfile is updated:
1. Profile Controller re-validates and updates status.
2. Profile Controller identifies pods referencing the profile whose `workload-template.io/profile-generation` annotation is stale.
3. Profile Controller sets a `Drifted` condition on stale pods and emits an event.
4. Profile Controller annotates the owning workload (Notebook, Deployment) with drift status.
5. On the next pod creation for that workload, the new pod picks up the updated profile.

**Consequences:**

A profile change does not cause immediate workload disruption. A 3-day training run is not killed because an admin updated a profile's memory setting. The admin (or automation) can trigger a rollout restart when ready.

Running workloads can be in a drifted state for the lifetime of their current pods. The drift is visible (condition, event, annotation) but not automatically resolved. This is an explicit design choice — WTO prioritizes workload stability over configuration consistency.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **Active drift correction** (re-patch running pods) | Pods are largely immutable after creation. Resources, DRA claims, and scheduling constraints cannot be changed on a running pod. Correction requires pod deletion and recreation — equivalent to a restart. Autonomously restarting GPU workloads (training jobs, inference services) causes data loss, service disruption, and potential cascade failures. |
| **No drift detection** | Users have no way to know that their running workload differs from the current profile version. A profile updated from 1x T4 to 2x T4 looks applied (annotation says the profile name) but the running pod has the old configuration. Silent inconsistency erodes trust in the profile as source of truth. |
| **Mandatory rollout on profile change** | Forces immediate disruption on all workloads referencing the profile. An admin updating a profile's description annotation would trigger rollouts. No way to batch profile updates and roll out once. |

---

## ADR-011: UI Metadata in Annotations

**Status:** Accepted

**Context:** Dashboards and UIs need metadata to render profile selectors: display names, descriptions, visibility flags, disabled state. This metadata can live in the CRD spec or in annotations.

**Decision:** All UI metadata lives in annotations:

| Annotation | Purpose |
|---|---|
| `workload-template.io/display-name` | Human-readable name shown in UIs |
| `workload-template.io/description` | Profile description |
| `workload-template.io/disabled` | `"true"` to hide from selection UIs |
| `workload-template.io/dashboard-feature-visibility` | Feature-gate visibility for dashboard rendering |

The WorkloadProfile spec contains only functional fields — fields that affect pod mutation.

**Consequences:**

The CRD schema is decoupled from UI concerns. Adding a new dashboard feature flag does not require a CRD version bump. Different UIs can use different annotation conventions without schema changes.

UI metadata has no schema validation. A typo in `workload-template.io/display-name` is not caught by the API server. This is acceptable because UI metadata has no functional impact — a misspelled display name does not affect workload placement.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **UI metadata in the spec** | Couples the CRD schema to UI rendering concerns. Every new dashboard feature (visibility flags, categorization tags, icon references) requires a CRD spec change and potentially a version bump. The spec becomes a mix of functional fields (resources, placement) and rendering hints (displayName, description) with different consumers and change cadences. Predecessor systems made this mistake and accumulated UI-only fields that the webhook ignored entirely (`minCount`, `maxCount`, `resourceType`). |
| **Separate UI CRD** | A dedicated CRD for UI metadata (e.g., `WorkloadProfileDisplay`) adds an object to manage per profile. The dashboard must join two resources to render a single profile entry. Over-engineering for what amounts to key-value display hints. |

---

## ADR-012: Per-Container Targeting

**Status:** Accepted

**Context:** Workload pods can have multiple containers with different resource requirements: a main training container needing GPUs, a sidecar for logging, an init container for data staging. WTO must inject resources into the correct container(s).

**Decision:** WorkloadProfile supports per-container targeting via `containers[]` entries that match by `name` or `index` (mutually exclusive per entry, CEL-enforced). A `defaults` section applies to containers not matched by any explicit entry.

Resolution order:
1. If a container matches an entry by `name`, that entry's resources are applied.
2. If a container matches an entry by `index`, that entry's resources are applied.
3. If a container matches entries by both `name` and `index`, `name` takes precedence.
4. If a container matches no entry, `defaults` is applied (if present).
5. If a container matches no entry and `defaults` is absent, the container is not modified.

At most one entry per name; at most one entry per index. Enforced by CEL validation on the CRD.

**Consequences:**

Profile authors can precisely control which containers receive GPU resources and which receive CPU-only resources. A profile can give the `main` container 4x A100 GPUs and 64Gi memory while leaving the `metrics-exporter` sidecar untouched.

Profile authors must know container names (or positions) for the workload types they target. For well-known workload types (Kubeflow Notebooks use `notebook`, KServe uses `kserve-container`), names are stable. For custom workloads, the profile author must align with the workload's container naming.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **Heuristic-based guessing** (look for "main", "notebook", or first container) | The approach used by predecessor systems. Breaks for multi-container pods with non-standard naming. Silently mutates the wrong container — a sidecar gets GPU resources, the main container gets nothing. No way for the profile author to control which container is targeted. |
| **All containers get the same resources** | Wasteful. A logging sidecar does not need 64Gi memory and 4x A100. Over-provisioning sidecars wastes quota and may prevent scheduling (node cannot satisfy the aggregate resource request). |
| **Label-based container matching** | Kubernetes does not support labels on containers — only on pods. Would require a custom convention (annotations with container-name-keyed values) that adds complexity without benefit over direct name/index targeting. |

---

## ADR-013: Webhook Ordering for Kueue Integration

**Status:** Accepted (2026-07-02)

**Context:** When Queue placement is configured, WTO's webhook injects the `kueue.x-k8s.io/queue-name` label at pod CREATE time so that Kueue's mutating webhook can see it and add its own scheduling gate (`kueue.x-k8s.io/admission`). Both gates must be present for the full flow: WTO validates quota and sets placement, Kueue manages workload admission. If Kueue's webhook doesn't see the queue label, it ignores the pod entirely and no Kueue Workload object is created.

Kubernetes invokes MutatingWebhookConfigurations in alphabetical order by name. Kueue's configuration is named `kueue-mutating-webhook-configuration`. If WTO's configuration sorted after Kueue's, Kueue would fire first, see no queue label, and skip the pod. WTO would then add the label, but Kueue's webhook would not be re-invoked.

**Decision:** WTO's MutatingWebhookConfiguration is named `aaa-wto` to sort alphabetically before `kueue-mutating-webhook-configuration`. This ensures WTO fires first, injects the queue label, and Kueue's webhook sees it on its pass.

**Consequences:**

The name `aaa-wto` is unconventional but explicit about its purpose. The ordering guarantee is stable — Kubernetes alphabetical ordering of MutatingWebhookConfigurations is part of the API contract.

Namespaces using Queue placement must have both labels: `workload-template.io/enabled: "true"` (for WTO's webhook) and `kueue.openshift.io/managed: "true"` (for Kueue's webhook).

**⚠ Fragility Warning:**

Alphabetical webhook ordering is an operational convention, not a strong contract. While Kubernetes does invoke MutatingWebhookConfigurations in alphabetical order by name (this is part of the API server implementation), the mechanism has inherent risks:

- **Naming collisions.** Any third-party operator that also names its webhook `aa-*` or `aaa-*` could sort before `aaa-wto`, breaking the guarantee. There is no reservation system for webhook names.
- **No declarative ordering.** Kubernetes provides no first-class webhook ordering mechanism (no `before:`/`after:` fields). Alphabetical sorting is the only lever, and it is implicit — nothing in the webhook manifest declares the dependency on firing before Kueue.
- **Silent breakage.** If ordering breaks (Kueue fires first, sees no queue label, skips the pod), the failure mode is silent: the pod is created, WTO adds the label, but no Kueue Workload object is ever created. The pod sits Pending with no Kueue events. Diagnosing this requires knowledge of webhook ordering semantics.

**Mitigations required:**

1. **CI conformance test.** A test should verify that after deploying both WTO and Kueue, the webhook ordering is correct: create a pod with a Queue-mode profile and assert that a Kueue Workload object is created within a reasonable timeout.
2. **Startup validation.** The operator should list MutatingWebhookConfigurations at startup and emit a Warning event if any webhook sorts before `aaa-wto` in namespaces where WTO is enabled.
3. **Documentation.** Operators deploying third-party admission webhooks alongside WTO must verify ordering. This ADR should be referenced in troubleshooting guides.

This is one of the highest-risk areas of the current design. The mechanism works today but is not robust against ecosystem changes. A Kubernetes-native webhook ordering mechanism (proposed but not yet accepted upstream) would be the proper long-term fix.

**Alternatives considered:**

| Alternative | Why rejected |
|---|---|
| **`reinvocationPolicy: IfNeeded` on Kueue's webhook** | Tested and confirmed working — Kueue re-invokes after WTO adds the label. However, the Kueue operator actively reconciles its webhook configuration and reverts manual changes. This makes the approach fragile — any Kueue operator reconciliation loop undoes the fix silently. |
| **WTO injects Kueue's scheduling gate directly** | WTO could add both its own gate and `kueue.x-k8s.io/admission`. This couples WTO to Kueue's internal gate name, which is not part of Kueue's public API and could change between versions. |
| **Controller-based queue label injection** (original design) | The original design set the queue label in the placement controller, not the webhook. This doesn't work because Kueue's webhook only fires at pod CREATE time — labels added later via patch don't trigger re-invocation. |

---

## ADR-014: Two-CRD Template-Binding Model

**Status:** Accepted. Supersedes ADR-008.

**Context:** ADR-008 chose a single WorkloadProfile CRD. The GPUaaS initiative and RHOAI multi-tenancy requirements revealed that hardware definition and tenant binding are distinct concerns owned by different personas. The ghost mutation problem — parent CRs not reflecting WTO-injected resources — blocks 7 GPUaaS requirements. The Model C architecture (WTO as resolution engine, component teams consume `resolvedSpec`) requires a pre-resolved spec in status.

**Decision:** Split into two CRDs following the StorageClass → PVC / DeviceClass → ResourceClaim pattern:

- **WorkloadProfileTemplate** (cluster-scoped): admin-created hardware blueprint — defaults, containers, deviceClaims. No placement.
- **WorkloadProfile** (namespace-scoped): user/automation-created binding — references a template via `templateRef`, adds placement. Supports inline spec (no `templateRef`) for simple deployments.

The profile controller resolves template + placement into `status.resolvedSpec`. The webhook reads `resolvedSpec` regardless of whether the profile uses a template or inline spec. Component teams (KServe, Notebook controller) read `resolvedSpec` to apply resources to their own CRs.

No overrides in MVP. Templates are pure hardware definitions; bindings add only placement.

**Consequences:**

Admin curates a hardware menu (templates). Users consume it (bindings with placement). Dashboard lists templates filtered by `namespaceSelector`, creates bindings when users select a profile. `resolvedSpec` provides the single source of truth for both the webhook (pod injection) and component teams (CR-level injection).

---

## ADR-015: Placement is Binding-Only

**Status:** Accepted

**Context:** Placement (Node or Queue mode) could live in the template, the binding, or both. LocalQueues are namespace-scoped — a cluster-scoped template cannot know which LocalQueue a tenant uses. Different tenants using the same `gpu-t4` template have different ClusterQueues with different quotas, borrowing limits, and priorities. Node placement is also tenant-specific (zone affinity, node pools).

**Decision:** Placement lives exclusively in the WorkloadProfile binding, never in the WorkloadProfileTemplate. Templates describe WHAT hardware a workload needs. Bindings describe WHERE it runs and WHOSE quota it draws from.

**Consequences:**

One template serves all tenants. No template proliferation for queue/zone variations. GitOps tenant onboarding (one YAML creates namespace + LocalQueue + WorkloadProfile) works naturally — placement is part of the tenant provisioning, not hardware definition. Dashboard shows templates as the "hardware menu" and creates bindings with the user's queue selection.

---

## ADR-016: namespaceSelector for Template Access Control

**Status:** Accepted

**Context:** With cluster-scoped templates visible to all namespaces, admins need a mechanism to restrict which tenants can use which templates. Four approaches were evaluated: RBAC on templates, Kueue-only enforcement, namespace allowlists, and `namespaceSelector` on templates.

**Decision:** Templates carry an optional `namespaceSelector` (same type as `ClusterQueue.namespaceSelector`). The profile controller validates the binding namespace's labels against the template's selector. Nil selector means all namespaces are allowed.

The dashboard reads `namespaceSelector` to filter templates client-side, showing only templates available to the current project. Kueue quota enforcement remains the backstop — even if a binding bypasses `namespaceSelector`, Kueue rejects workloads that exceed quota.

**Consequences:**

Admins control template access by labeling namespaces — the same labels they use for ClusterQueue.namespaceSelector. A future tenant onboarding operator that provisions namespace + labels + LocalQueue automatically determines which templates are available. Namespace is the smallest unit of access — no per-user granularity within a namespace.
