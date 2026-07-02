# Workload Tuning Operator

The Workload Tuning Operator (WTO) ensures Kubernetes workloads land on the right hardware with the right resources. It is a single operator for workload-to-hardware placement: CPU architecture scheduling, GPU/accelerator allocation via Dynamic Resource Allocation (DRA), resource injection, and queue-based scheduling integration with Kueue.

WTO replaces scattered, webhook-only placement logic with a unified, controller-driven model. It gates pods at admission, validates constraints against live cluster state, injects resources and DRA claims, and only releases the pod for scheduling once everything is correct. The result: workloads fail fast with actionable feedback instead of sitting Pending with cryptic events.

## MVP Scope

### WorkloadProfile CRD

A namespaced Custom Resource that declares what hardware a workload needs and where it should run. The CRD embeds native Kubernetes types — not custom abstractions — so upstream API evolution requires only a dependency bump and CRD regeneration.

#### Resource Requirements

- `defaults.resources` — fallback `corev1.ResourceRequirements` applied to any container not explicitly targeted.
- `containers[]` — per-container overrides targeting by `name` or `index` (mutually exclusive, CEL-enforced). Replaces heuristic-based container guessing.
- Requests and limits use native `corev1.ResourceRequirements`. No custom type taxonomy, no UI fields in the spec.

#### DRA Device Claims

- `deviceClaims[]` — embedded `resource.k8s.io/v1.DeviceRequest` entries. Each entry maps directly to a DRA ResourceClaimTemplate generated at admission time.
- CEL selectors for GPU model, memory, CUDA version, topology, MIG profile — the full DRA expression language.
- Device plugin backward compatibility — profiles can use `resources.requests["nvidia.com/gpu"]` for clusters without DRA drivers. WTO detects which model the cluster uses and adapts.

#### Placement

- Discriminated union: `type: Queue` or `type: Node`.
- **Queue mode**: `localQueueName` + optional `priorityClass`. WTO sets the `kueue.x-k8s.io/queue-name` label. Kueue handles admission, quota, fair sharing, and preemption.
- **Node mode**: `nodeSelector` + `tolerations` injected directly into the pod spec.
- Device claims and placement are independent axes. A profile can have either, both, or neither.

#### UI Metadata

- Display name, description, visibility, and disabled state live exclusively in annotations (`workload-tuning.io/display-name`, `workload-tuning.io/description`, etc.).
- The spec contains only functional fields. Dashboards and UIs read annotations for rendering.

#### Status

WTO continuously validates profiles against live cluster state and reports:

- `Valid` — the profile is structurally correct.
- `DeviceClassAvailable` — referenced DRA DeviceClasses exist and have devices in the cluster.
- `QueueReady` — the referenced LocalQueue exists, its backing ClusterQueue covers the requested resources, and framework integrations are enabled.
- `QuotaFit` — the profile's resource requirements fit within namespace ResourceQuota and ClusterQueue nominal quota.
- `satisfiableNodes` — how many nodes can fulfill the profile.
- `appliedWorkloads` — how many workloads currently reference the profile.
- `quotaSummary` — namespace and ClusterQueue quota usage (CPU, memory, devices).

Profiles that are structurally impossible (DeviceClass doesn't exist, zero satisfiable nodes, profile exceeds namespace quota ceiling) are marked with clear conditions and reasons. Transient unavailability (quota temporarily full) is distinguished from permanent misconfiguration.

### Scheduling Gate Webhook

A `MutatingAdmissionWebhook` with `failurePolicy: Fail` that intercepts pod creation. The webhook handles all pod spec fields that are **immutable after creation** — resource requirements, DRA claim references, and the scheduling gate — because Kubernetes does not allow these fields to be modified on existing pods, even gated ones.

If the pod carries a `workload-tuning.io/profile-name` annotation, the webhook:

1. **Reads the WorkloadProfile** referenced by the annotation (from informer cache).
2. **Injects `corev1.ResourceRequirements`** into targeted containers (by name, index, or defaults).
3. **Injects DRA claim references** if the profile has `deviceClaims`:
   - Adds `pod.spec.resourceClaims[]` entries referencing pre-created ResourceClaimTemplates (managed by the Profile Controller).
   - Adds `container.resources.claims[]` references linking targeted containers to their device claims.
4. **Adds a scheduling gate** (`workload-tuning.io/scheduling-gate`) to hold the pod for controller-side validation.
5. **Sets tracking annotations**: `workload-tuning.io/profile-generation`, `workload-tuning.io/applied-at`.

If the pod has no profile annotation, the webhook is a no-op.

Why the webhook does injection, not just gating: `pod.spec.resourceClaims`, `container.resources.requests`, and `container.resources.limits` are immutable after pod creation on Kubernetes 1.34. A controller cannot add these fields to a gated pod — the API server rejects the update. Only `nodeSelector`, `tolerations`, `nodeAffinity`, and `labels` are mutable on gated pods. This was validated on OCP 4.21 / K8s 1.34 during spike testing (see `spike/FINDINGS.md`).

### Placement Controller

A controller that reconciles gated pods. It handles all pod spec fields that **are mutable** on gated pods — placement constraints and labels — plus pre-flight validation that should not block the synchronous admission path.

1. **Pre-flight checks**:
   - Profile exists and is valid.
   - Namespace ResourceQuota has remaining capacity for the requested resources (CPU, memory, DRA devices).
   - DRA DeviceClass quota (`<class>.deviceclass.resource.k8s.io/devices`) has remaining capacity.
   - LocalQueue exists in the namespace (Queue placement mode).
   - ClusterQueue covers the requested DeviceClasses in its `deviceClassMappings`.
2. **If any check fails**: the pod stays gated. An event is emitted on the pod with an actionable message (what failed, why, what to do). The controller re-checks periodically — if quota frees up or the admin fixes the configuration, the pod proceeds automatically.
3. **If all checks pass**:
   - Sets placement: `nodeSelector` + `tolerations` (Node mode) or `kueue.x-k8s.io/queue-name` label (Queue mode).
   - Removes the scheduling gate. The pod enters normal scheduling.

### Profile Controller

A controller that reconciles WorkloadProfile CRs:

- Watches DRA ResourceSlices, DeviceClasses, Kueue LocalQueues, ClusterQueues, and namespace ResourceQuota objects.
- On any change, re-validates affected WorkloadProfiles and updates their status conditions and quota summary.
- On WorkloadProfile spec change, re-queues all pods referencing that profile for reconciliation on their next restart or update. Does not restart running workloads autonomously.
- Detects drift: if a workload's actual resources no longer match the profile it references, sets a `Drifted` condition on the pod and emits an event. Does not auto-revert — drift correction happens on the next pod lifecycle event.

### Kueue Discovery

WTO does not manage Kueue. It discovers and adapts to whatever Kueue installation exists:

- Lists ClusterQueues, LocalQueues, and ResourceFlavors to understand quota topology.
- Reads the Kueue CR for framework integrations and `deviceClassMappings`.
- Reports gaps in WTO status conditions (missing framework integrations, missing deviceClassMappings, LocalQueue not found in namespace).
- Works with Kueue installed via any method (kueue-operator, upstream Helm, manual).
- If Kueue is not installed, Queue placement mode is unavailable. Node placement mode and DRA work independently.

### Backward Compatibility

- **Device plugin mode**: if the cluster has no DRA driver or Kueue lacks `deviceClassMappings`, WTO creates profiles using `resources.requests["nvidia.com/gpu"]` instead of DRA `deviceClaims`. A `DRAEnabled` status condition indicates which mode is active.
- **Clusters without Kueue**: Node placement mode works without Kueue. Profiles with `type: Queue` are marked `QueueReady: False` with guidance to install Kueue or switch to Node placement.

## Documentation

- [Architecture Decisions](docs/architecture-decisions.md) — ADRs covering injection level, conflict resolution, scheduling gates, quota validation, Kueue integration, DRA strategy, and CRD design.
- [Roadmap](docs/roadmap.md) — Phased plan from scaffold to production-ready MVP, including integration test matrix and open questions.

## Architecture

```
Pod CREATE
  │
  ▼
Webhook (synchronous, immutable fields):
  ├── Read WorkloadProfile (from cache)
  ├── Inject resources into containers
  ├── Inject DRA claim references (to pre-created templates)
  └── Add scheduling gate
  │
  ▼
Placement Controller (async, mutable fields):
  ├── Pre-flight quota checks
  ├── Set placement (nodeSelector/tolerations or Kueue label)
  └── Remove scheduling gate
  │
  ▼
Kueue: suspend, check quota, admit
  │
  ▼
kube-scheduler + DRA: allocate devices, bind pod to node
```

```
WorkloadProfile CR changed
  │
  ▼
Profile Controller:
  ├── Validate against cluster state
  ├── Create/update ResourceClaimTemplates from deviceClaims
  ├── Update status conditions + quota summary
  └── Re-queue affected pods for next reconciliation
```

### Design Principles

- **WTO is a pre-flight validator, not a quota enforcer.** Kueue and ResourceQuota remain the hard enforcement mechanisms. WTO catches violations early and provides actionable feedback.
- **Discover, never assume.** WTO reads Kueue, DRA, and node state to understand what's available. It never writes to Kueue objects or DRA resources (except ResourceClaimTemplates it owns).
- **Embed native types, don't reinvent them.** `corev1.ResourceRequirements` for resources, `resource.k8s.io/v1.DeviceRequest` for DRA. Upstream API additions require only a Go dependency bump and CRD regeneration.
- **Immutable in webhook, mutable in controller.** The webhook injects fields that Kubernetes makes immutable after pod creation (resources, DRA claims) and adds the scheduling gate. The controller handles fields that remain mutable on gated pods (nodeSelector, tolerations, labels) plus pre-flight validation. This split is dictated by the Kubernetes API, not by preference — validated on K8s 1.34.
- **Source of truth with soft enforcement.** WTO continuously validates profiles and detects drift. It does not auto-revert running workloads — drift correction happens on the next pod lifecycle event (restart, scale, update).

## Workload Types

MVP supports mutation of pods created by:

- Kubeflow Notebooks (`kubeflow.org/v1`)
- KServe InferenceServices (`serving.kserve.io/v1beta1`)
- KServe LLMInferenceServices (`serving.kserve.io/v1alpha1`, `serving.kserve.io/v1alpha2`)

The webhook intercepts pod creation generically. Support for additional workload types (Jobs, Deployments, StatefulSets, training operators) is additive — no architectural change required.

## Not in MVP

- **MTO arch detection merge**: CPU architecture scheduling remains in the Multiarch Tuning Operator. Integration is a follow-on.
- **Active drift correction**: WTO detects and reports drift but does not autonomously restart or re-patch running workloads.
- **Fractional GPU sharing**: MIG profiles are supported via DRA DeviceClasses. LD_PRELOAD-based memory isolation (HAMi-style) is out of scope.
- **GPU inventory aggregation**: WTO reads DRA ResourceSlices — it does not maintain its own device inventory.
- **Multi-cluster**: single-cluster only.
- **Profile templates / inheritance**: no template-to-instance hierarchy. Profiles are standalone.
- **Automated migration tooling**: no built-in conversion from prior hardware profile systems. Platform operators provide migration documentation and tooling as needed.

## Prerequisites

- Kubernetes 1.27+ (scheduling gates)
- DRA (`resource.k8s.io/v1`) requires Kubernetes 1.34+ / OpenShift 4.21+
- NVIDIA GPU Operator with DRA driver (for GPU device claims)
- Kueue (optional, required for Queue placement mode)

## Operational Notes

### Webhook Ordering with Kueue

WTO's MutatingWebhookConfiguration is named `aaa-wto` to ensure it fires before Kueue's `kueue-mutating-webhook-configuration`. This is required for Queue placement: WTO injects the `kueue.x-k8s.io/queue-name` label at pod CREATE time, and Kueue's webhook must see this label to add its own scheduling gate. See ADR-013 for details.

Namespaces using Queue placement need both labels:
- `workload-tuning.io/enabled: "true"` (WTO webhook)
- `kueue.openshift.io/managed: "true"` (Kueue webhook)

### Notebook Integration

For Kubeflow Notebooks, the `workload-tuning.io/profile-name` annotation must be set on both the Notebook CR metadata and in `spec.template.metadata.annotations` to ensure propagation through the StatefulSet to the pod.

## License

Apache License 2.0
