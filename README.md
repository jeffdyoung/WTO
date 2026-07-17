# Workload Template Operator

The Workload Template Operator (WTO) ensures Kubernetes workloads land on the right hardware with the right resources. It is a single operator for workload-to-hardware placement: CPU architecture scheduling, GPU/accelerator allocation via Dynamic Resource Allocation (DRA), resource injection, and queue-based scheduling integration with Kueue.

WTO replaces scattered, webhook-only placement logic with a unified, controller-driven model. It gates pods at admission, validates constraints against live cluster state, injects resources and DRA claims, and only releases the pod for scheduling once everything is correct. The result: workloads fail fast with actionable feedback instead of sitting Pending with cryptic events.

## MVP Scope

### Three CRDs

WTO uses a three-CRD model:

- **WorkloadProfileTemplate** (cluster-scoped) — admin-managed hardware definitions: `defaults`, `containers`, `deviceClaims`, `namespaceSelector` ACL. Templates define WHAT hardware to use.
- **WorkloadProfile** (namespace-scoped) — tenant binding: references a template via `templateRef` (or defines resources inline), adds `placement` (WHERE to run), and optionally sets `targetKind` (WHICH workload type). The profile controller resolves template + placement into `status.resolvedSpec`.
- **WorkloadTypeConfig** (cluster-scoped) — pluggable workload type registry: describes how each workload type (Notebook, InferenceService, Job, etc.) structures its pod template so WTO can propagate annotations automatically. Cluster admins and components can add/remove types without code changes.

All CRDs embed native Kubernetes types — not custom abstractions — so upstream API evolution requires only a dependency bump and CRD regeneration.

### WorkloadProfile

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

- Display name, description, visibility, and disabled state live exclusively in annotations (`workload-template.io/display-name`, `workload-template.io/description`, etc.).
- The spec contains only functional fields. Dashboards and UIs read annotations for rendering.

#### Status

WTO continuously validates profiles against live cluster state and reports:

- `Valid` — the profile is structurally correct and all dependencies exist.
- `TemplateFound` / `NamespaceAllowed` — template exists and namespace passes the template's `namespaceSelector` ACL.
- `DeviceClassAvailable` — referenced DRA DeviceClasses exist.
- `TargetKindValid` — referenced WorkloadTypeConfig exists and container names are compatible.
- `QueueReady` — the referenced LocalQueue exists (not yet implemented).
- `QuotaFit` — the profile's resource requirements fit within namespace quota (not yet implemented).
- `resolvedSpec` — the fully-merged spec (template hardware + profile placement). This is the single source of truth consumed by the webhook and component teams.
- `satisfiableNodes` — how many nodes can fulfill the profile.
- `appliedWorkloads` — how many workloads currently reference the profile.

Profiles that are structurally impossible (DeviceClass doesn't exist, zero satisfiable nodes, profile exceeds namespace quota ceiling) are marked with clear conditions and reasons. Transient unavailability (quota temporarily full) is distinguished from permanent misconfiguration.

### Scheduling Gate Webhook

A `MutatingAdmissionWebhook` with `failurePolicy: Fail` that intercepts pod creation. The webhook handles all pod spec fields that are **immutable after creation** — resource requirements, DRA claim references, and the scheduling gate — because Kubernetes does not allow these fields to be modified on existing pods, even gated ones.

If the pod carries a `workload-template.io/profile-name` annotation, the webhook:

1. **Reads the WorkloadProfile** and its `status.resolvedSpec` (pre-resolved by the Profile Controller).
2. **Injects `corev1.ResourceRequirements`** into targeted containers (by name, index, or defaults).
3. **Injects DRA claim references** if the resolved spec has `deviceClaims`:
   - Adds `pod.spec.resourceClaims[]` entries referencing pre-created ResourceClaimTemplates.
   - Adds `container.resources.claims[]` references linking targeted containers to their device claims.
4. **Adds a scheduling gate** (`workload-template.io/scheduling-gate`) to hold the pod for controller-side validation.
5. **Sets cost attribution labels**: `workload-template.io/profile-name` and `workload-template.io/template-name` as pod labels (Prometheus-visible for cost management and metric correlation).
6. **Sets tracking annotations**: `workload-template.io/profile-generation`, `workload-template.io/template-generation`, `workload-template.io/overrides`.

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

- **Template resolution**: resolves `templateRef` → reads the `WorkloadProfileTemplate`, merges template hardware with profile placement, writes the result to `status.resolvedSpec`.
- **Namespace ACL**: validates the profile's namespace against the template's `namespaceSelector`. Rejects with `NamespaceAllowed=False` if the namespace doesn't match.
- **Target kind validation**: if `targetKind` is set, validates container name compatibility against the referenced `WorkloadTypeConfig`.
- **ResourceClaimTemplate lifecycle**: creates/updates `ResourceClaimTemplate` CRs from `deviceClaims[]`, with owner references for garbage collection.
- **Status conditions**: `Valid`, `TemplateFound`, `NamespaceAllowed`, `DeviceClassAvailable`, `DRAEnabled`, `TargetKindValid`.
- **Metrics**: `satisfiableNodes` count, `appliedWorkloads` count.
- **Finalizer**: `workload-template.io/profile-protection` prevents deletion while gated pods reference the profile.
- Watches `WorkloadProfileTemplate` changes and re-reconciles bound profiles via field indexer.

### WorkloadType Controller

A controller that reconciles `WorkloadTypeConfig` CRs:

- Checks whether the referenced CRD (e.g., `kubeflow.org/v1 Notebook`) exists on the cluster via the discovery client.
- Registers dynamic watches on the PropagationReconciler for each available workload type.
- Reports `CRDAvailable` and `WatchActive` status conditions.

### Propagation Controller

A controller with no static watches — receives dynamic watches from the WorkloadType Controller:

- When a registered workload CR (Notebook, Job, InferenceService, etc.) has the `workload-template.io/profile-name` annotation on its metadata, propagates it to the pod template path defined in the `WorkloadTypeConfig`.
- Uses the unstructured client for generic CR mutation (works with any CRD without compile-time type knowledge).
- Skips propagation when `nativePropagation: true` (trusts the component controller to handle it).

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

- [Architecture Decisions](docs/architecture-decisions.md) — 16 ADRs covering injection level, conflict resolution, scheduling gates, quota validation, Kueue integration, DRA strategy, CRD design, and template-binding model.
- [Roadmap](docs/roadmap.md) — 11-phase plan from scaffold to production-ready MVP.
- [Template-Binding Design](docs/template-binding-design.md) — Two-CRD model rationale and YAML sketches.
- [Known Issues](docs/known-issues.md) — Open bugs and limitations with severity ratings.
- [GPUaaS Readiness](docs/wto_gpuaas_readiness.md) — Gap analysis against 530 Jira issues across 13 categories.
- [Approach Analysis](docs/approach-analysis.md) — Evaluation of 6 injection levels with scoring matrix.
- [Test Plan](docs/test-plan.md) — Unit, integration, and E2E test strategy.

## Architecture

```
WorkloadProfileTemplate (cluster-scoped, admin-managed)
  │
  ▼
Profile Controller:
  ├── Resolve template + placement → status.resolvedSpec
  ├── Validate namespace ACL, targetKind, DeviceClass availability
  ├── Create/update ResourceClaimTemplates from deviceClaims
  └── Update status conditions
```

```
WorkloadTypeConfig (cluster-scoped, admin/component-managed)
  │
  ▼
WorkloadType Controller:
  ├── Check CRD existence (discovery client)
  └── Register dynamic watch on Propagation Controller
        │
        ▼
Propagation Controller:
  └── Workload CR (Notebook, Job, etc.) has profile annotation
      → Propagate annotation to pod template path
```

```
Pod CREATE (from any workload controller, or directly)
  │
  ▼
Webhook (synchronous, immutable fields):
  ├── Read resolvedSpec from WorkloadProfile status
  ├── Inject resources into containers
  ├── Inject DRA claim references
  ├── Set cost labels (profile-name, template-name)
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

### Design Principles

- **WTO is a pre-flight validator, not a quota enforcer.** Kueue and ResourceQuota remain the hard enforcement mechanisms. WTO catches violations early and provides actionable feedback. To be explicit: WTO improves failure explanation and avoids obviously impossible scheduling attempts. It does not replace Kubernetes, Kueue, ResourceQuota, the scheduler, or the DRA driver as sources of enforcement. WTO status conditions are best-effort early warnings, not guarantees — a TOCTOU race exists between WTO's check and the pod reaching the actual enforcement point.
- **Discover, never assume.** WTO reads Kueue, DRA, and node state to understand what's available. It never writes to Kueue objects or DRA resources (except ResourceClaimTemplates it owns).
- **Embed native types, don't reinvent them.** `corev1.ResourceRequirements` for resources, `resource.k8s.io/v1.DeviceRequest` for DRA. Upstream API additions require only a Go dependency bump and CRD regeneration.
- **Immutable in webhook, mutable in controller.** The webhook injects fields that Kubernetes makes immutable after pod creation (resources, DRA claims) and adds the scheduling gate. The controller handles fields that remain mutable on gated pods (nodeSelector, tolerations, labels) plus pre-flight validation. This split is dictated by the Kubernetes API, not by preference — validated on K8s 1.34.
- **Source of truth with soft enforcement.** WTO continuously validates profiles and detects drift. It does not auto-revert running workloads — drift correction happens on the next pod lifecycle event (restart, scale, update).

## Workload Types

WTO ships with 5 default `WorkloadTypeConfig` CRs:

| Name | GVK | Pod Template Path | Propagation |
|------|-----|-------------------|-------------|
| `pod` | core/v1 Pod | n/a | Native (leaf) |
| `job` | batch/v1 Job | `spec.template` | WTO propagates |
| `notebook` | kubeflow.org/v1 Notebook | `spec.template` | WTO propagates |
| `inferenceservice` | serving.kserve.io/v1beta1 InferenceService | `spec.predictor.annotations` | WTO propagates |
| `pytorchjob` | kubeflow.org/v1 PyTorchJob | `spec.pytorchReplicaSpecs.Worker.template` | WTO propagates |

Cluster admins can register additional workload types (RayJob, SparkApplication, MPIJob, etc.) by creating new `WorkloadTypeConfig` CRs — no code changes required. Custom types need an accompanying RBAC `ClusterRole` labeled `workload-template.io/aggregate-to-wto: "true"`.

The pod webhook intercepts pod creation generically and remains the universal fallback regardless of WorkloadTypeConfig.

## Not in MVP

- **MTO arch detection merge**: CPU architecture scheduling remains in the Multiarch Tuning Operator. Integration is a follow-on.
- **Active drift correction**: WTO detects and reports drift but does not autonomously restart or re-patch running workloads.
- **Fractional GPU sharing**: MIG profiles are supported via DRA DeviceClasses. LD_PRELOAD-based memory isolation (HAMi-style) is out of scope.
- **GPU inventory aggregation**: WTO reads DRA ResourceSlices — it does not maintain its own device inventory.
- **Multi-cluster**: single-cluster only.
- **Automated migration tooling**: no built-in conversion from prior hardware profile systems. Platform operators provide migration documentation and tooling as needed.

## Platform Compatibility

### Currently validated

- **OpenShift 4.21** / Kubernetes 1.34 on AWS (ROSA)
- NVIDIA GPU Operator v26.3.3 with DRA driver v25.12.0
- Kueue v1.3.1 (via kueue-operator)
- RHOAI v3.5.0-ea.2

### OpenShift-specific assumptions in the current implementation

The following components use OpenShift-specific mechanisms that would need alternatives for vanilla Kubernetes:

| Component | OpenShift mechanism | Vanilla K8s alternative | Status |
|---|---|---|---|
| **Webhook TLS** | `service.beta.openshift.io/inject-cabundle` / `serving-cert-secret-name` (OpenShift service-ca) | cert-manager with `Certificate` CR | Planned (Phase 8) |
| **Deployment tooling** | `oc apply` in Makefile | `kubectl apply` / Helm chart | Planned (Phase 8) |
| **Kueue namespace label** | `kueue.openshift.io/managed: "true"` | Upstream Kueue label convention | Planned (Phase 8) |
| **OLM packaging** | OLM bundle for OperatorHub | Helm chart | Planned (Phase 8) |

WTO's core design (CRD, webhook, controllers, scheduling gate pattern) is Kubernetes-native and not OpenShift-specific. The portability gap is in deployment and TLS bootstrapping, not in the operator logic.

### Target

- Vanilla Kubernetes 1.34+ with cert-manager and Helm (Phase 8)
- Any Kubernetes distribution supporting scheduling gates (1.27+) for non-DRA profiles

## Prerequisites

- Kubernetes 1.27+ (scheduling gates)
- DRA (`resource.k8s.io/v1`) requires Kubernetes 1.34+ / OpenShift 4.21+
- NVIDIA GPU Operator with DRA driver (for GPU device claims)
- Kueue (optional, required for Queue placement mode)

## Operational Notes

### Webhook Ordering with Kueue

WTO's MutatingWebhookConfiguration is named `aaa-wto` to ensure it fires before Kueue's `kueue-mutating-webhook-configuration`. This is required for Queue placement: WTO injects the `kueue.x-k8s.io/queue-name` label at pod CREATE time, and Kueue's webhook must see this label to add its own scheduling gate. See ADR-013 for details.

Namespaces using Queue placement need both labels:
- `workload-template.io/enabled: "true"` (WTO webhook)
- `kueue.openshift.io/managed: "true"` (Kueue webhook)

### Workload Annotation Placement

WTO mutates pods, not workload CRs (see ADR-001). The `workload-template.io/profile-name` annotation must be present on the **pod** at creation time. The `PropagationReconciler` automates this for registered workload types: place the annotation on the workload CR's `metadata.annotations`, and WTO propagates it to the pod template path defined in the `WorkloadTypeConfig`.

For workload types without a `WorkloadTypeConfig`, users must place the annotation directly on the pod template. The table below shows the correct path for each type:

| Workload Kind | Annotation placement | Notes |
|---|---|---|
| **Pod** | `metadata.annotations` | Direct — no propagation needed. |
| **Job** | `spec.template.metadata.annotations` | Standard pod template path. |
| **CronJob** | `spec.jobTemplate.spec.template.metadata.annotations` | Nested through Job template. |
| **Deployment** | `spec.template.metadata.annotations` | Standard pod template path. |
| **StatefulSet** | `spec.template.metadata.annotations` | Standard pod template path. |
| **Kubeflow Notebook** | Both `metadata.annotations` AND `spec.template.metadata.annotations` | Notebook controller propagates CR-level annotations to the StatefulSet pod template. Set both to ensure propagation. |
| **KServe InferenceService** | `metadata.annotations` | KServe propagates CR-level annotations to predictor pods. Validated on RHOAI v3.5.0-ea.2. |
| **PyTorchJob** | `spec.pytorchReplicaSpecs.<role>.template.metadata.annotations` | Per-replica-type pod template. Set on `Worker`, `Master`, etc. as needed. |
| **RayJob / RayCluster** | `spec.rayClusterSpec.headGroupSpec.template.metadata.annotations` and `spec.rayClusterSpec.workerGroupSpecs[].template.metadata.annotations` | Separate templates for head and worker groups. |
| **MPIJob** | `spec.mpiReplicaSpecs.<role>.template.metadata.annotations` | Per-role pod template. |
| **SparkApplication** | `spec.driver.annotations` and `spec.executor.annotations` | Spark uses a non-standard annotation path. |

**If your workload type is not listed:** create a `WorkloadTypeConfig` CR describing the GVK and pod template path, and WTO will handle propagation automatically. Alternatively, place the annotation on the pod template directly.

For workload types with a `WorkloadTypeConfig`, WTO's `PropagationReconciler` handles annotation propagation automatically — users only need to annotate the workload CR's `metadata.annotations`.

## License

Apache License 2.0
