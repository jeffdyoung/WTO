# WorkloadProfileTemplate + WorkloadProfile: Template-Binding Design

## Status: MVP Scope

This design is required for the GPUaaS MVP. The ghost mutation problem — parent CRs (InferenceService, Notebook, Deployment) not reflecting WTO-injected GPU resources — blocks 7 GPUaaS requirements:

| Req | Description | Why ghost mutations block it |
|---|---|---|
| 2.7 | Profile status conditions (QuotaFit, QueueReady) | Dashboard reads WorkloadProfile status, needs resolved spec + quota info |
| 2.10 | Injection for all workload types | CR-level controllers (KServe) make scaling/runtime decisions without seeing injected resources |
| 3.4 | Quota visibility before workload creation | Dashboard shows the CR — no GPU resources means no pre-submit signal |
| 3.7 | Kueue admission feedback for HWPs | Dashboard surfaces feedback from the workload CR, which knows nothing about injected claims |
| 6.1 | GPU topology/utilization dashboard | Correlating GPU metrics to workloads requires the workload CR to carry device info |
| 7.3 | GPU cost tracking per workload | Cost dashboards link back to the owning CR — invisible injection means invisible cost attribution |
| 13.5 | Kueue admission feedback for HWPs | Same as 3.7 — dashboard reads CRs |

WTO ships with the two-CRD model from day one. There is no single-CRD phase and no HWP migration.

## Problem

WTO's pod-level injection (ADR-001) has three weaknesses scored in the [approach analysis](approach-analysis.md):

- **Profile propagation (1/5):** No mechanism to roll out template changes to running workloads.
- **Observability (2/5):** Parent CRs don't show injected resources — "ghost mutations."
- **Debuggability (2/5):** Users must know to inspect pods, not their Deployments/InferenceServices.

## Proposal: Two-CRD Template-Binding Model

Split the current WorkloadProfile into two CRDs following the StorageClass → PVC / DeviceClass → ResourceClaim pattern:

**WorkloadProfileTemplate** — cluster-scoped (or admin-namespace-scoped), reusable hardware blueprint. Admin creates these. Defines resources, DRA claims, placement rules.

**WorkloadProfile** — namespace-scoped, per-workload binding. References a template. Becomes the top-level CR for observability, drift detection, and propagation. Owns the ResourceClaimTemplates.

```
WorkloadProfileTemplate "gpu-t4" (cluster-scoped, admin creates once)
    ↑ (templateRef)
WorkloadProfile "my-inference" (ns: ml-team, user/automation creates)
    ↓ (owns)
ResourceClaimTemplate "wto-my-inference-gpu" (ns: ml-team)
```

## CRD Sketches

### WorkloadProfileTemplate

```yaml
apiVersion: workload-template.io/v1alpha1
kind: WorkloadProfileTemplate
metadata:
  name: gpu-t4
  annotations:
    workload-template.io/display-name: "Tesla T4 (1 GPU)"
    workload-template.io/description: "1x T4 GPU with 1 CPU and 4Gi memory"
spec:
  defaults:
    resources:
      requests: { cpu: "1", memory: "4Gi" }
      limits: { cpu: "2", memory: "8Gi" }
  deviceClaims:
  - name: gpu
    request:
      name: gpu
      exactly:
        deviceClassName: gpu.nvidia.com
        count: 1
        selectors:
        - cel:
            expression: 'device.attributes["gpu.nvidia.com"].productName == "Tesla T4"'
  placement:
    type: Queue
    queue: { localQueueName: gpu-queue }
```

### WorkloadProfile (binding)

```yaml
apiVersion: workload-template.io/v1alpha1
kind: WorkloadProfile
metadata:
  name: my-inference
  namespace: ml-team
spec:
  templateRef: gpu-t4
  # Optional per-workload overrides — merged on top of template
  # overrides:
  #   defaults:
  #     resources:
  #       requests: { cpu: "2" }   # override CPU, inherit everything else
  #   placement:
  #     type: Node
  #     node: { nodeSelector: { "zone": "us-east-1a" } }
status:
  templateGeneration: 3
  resolvedGeneration: 3        # matches templateGeneration when up-to-date
  appliedWorkloads: 2
  driftedWorkloads: 0
  satisfiableNodes: 3
  # resolvedSpec: the fully-resolved spec after merging template + overrides.
  # This is the single source of truth for what WTO will inject into pods.
  # Dashboard reads this instead of the parent CR to show GPU/DRA/quota info.
  resolvedSpec:
    defaults:
      resources:
        requests: { cpu: "1", memory: "4Gi" }
        limits: { cpu: "2", memory: "8Gi" }
    deviceClaims:
    - name: gpu
      request:
        name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
          count: 1
          selectors:
          - cel:
              expression: 'device.attributes["gpu.nvidia.com"].productName == "Tesla T4"'
    placement:
      type: Queue
      queue: { localQueueName: gpu-queue }
  # quotaSummary: pre-flight quota check result at the profile level.
  # Dashboard reads this to show quota fit before workload creation.
  quotaSummary:
    fit: true
    message: "gpu-queue has 4 available devices, profile requests 1"
    checkedAt: "2026-07-15T10:30:00Z"
  conditions:
  - type: Valid
    status: "True"
  - type: TemplateFound
    status: "True"
  - type: DeviceClassAvailable
    status: "True"
  - type: QueueReady
    status: "True"
    message: "LocalQueue gpu-queue exists and is active"
  - type: QuotaFit
    status: "True"
    message: "Namespace quota allows 1 gpu.nvidia.com device"
  - type: Drifted
    status: "False"
```

## Lifecycle

### 1. Admin creates the template

```yaml
kubectl apply -f gpu-t4-template.yaml
```

No ResourceClaimTemplates are created yet. The template is a spec — a declaration of "what T4 GPU access looks like."

### 2. User creates the binding

```yaml
kubectl apply -f my-inference-profile.yaml
```

The WorkloadProfile references `templateRef: gpu-t4`.

### 3. Profile controller reconciles

The controller reads the template, resolves the full spec (template + any overrides), and creates the ResourceClaimTemplate in the user's namespace:

```
WorkloadProfileTemplate "gpu-t4" (cluster)
    ↓ reads spec
WorkloadProfile "my-inference" (ns: ml-team)
    ↓ owns
ResourceClaimTemplate "wto-my-inference-gpu" (ns: ml-team)
    spec.spec.devices.requests:
    - name: gpu
      exactly:
        deviceClassName: gpu.nvidia.com
        count: 1
        selectors: [{ cel: { expression: '...' } }]
```

The RCT is named `wto-<profile>-<claim>`, owned by the WorkloadProfile. Garbage collected on profile deletion.

Status updated:
- `templateGeneration` set from the template's `metadata.generation`
- `resolvedGeneration` matches (no drift)
- `Valid: True` after RCT creation confirmed

### 4. User creates a workload

```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: my-model
  namespace: ml-team
  annotations:
    workload-template.io/profile-name: my-inference   # references the binding
spec:
  predictor:
    model:
      modelFormat: { name: sklearn }
      storageUri: "s3://models/my-model"
```

### 5. Webhook fires on the resulting pod

```
Pod CREATE in ns: ml-team
  │
  ├─ Webhook reads WorkloadProfile "my-inference"
  ├─ Resolves templateRef → WorkloadProfileTemplate "gpu-t4"
  ├─ Merges template spec + profile overrides
  │
  └─ Injects into pod:
     ├─ container.resources: { cpu: "1", memory: "4Gi" }
     ├─ pod.spec.resourceClaims: [{ name: "gpu", templateName: "wto-my-inference-gpu" }]
     ├─ container.resources.claims: [{ name: "gpu" }]
     ├─ scheduling gate
     ├─ kueue.x-k8s.io/queue-name: gpu-queue
     └─ annotations: profile-name, profile-generation, template-generation
```

### 6. Kubernetes creates a ResourceClaim from the template

```
ResourceClaimTemplate "wto-my-inference-gpu"
    ↓ K8s creates per-pod claim automatically
ResourceClaim "my-model-predictor-abc12-gpu-x7k9f" (auto-generated name)
    spec.devices.requests:
    - name: gpu
      exactly:
        deviceClassName: gpu.nvidia.com
        count: 1
        selectors: [...]
    status:
      allocation:
        devices:
          results:
          - driver: gpu.nvidia.com
            device: gpu-0
            pool: worker-gpu-1
```

### 7. DRA scheduler + placement controller

```
DRA scheduler:
  ├─ Finds a Tesla T4 matching the CEL selector in ResourceSlices
  ├─ Allocates gpu-0 on worker-gpu-1
  └─ Sets claim.status.allocation

Placement controller:
  ├─ Checks quota (ResourceQuota, DRA device quota)
  ├─ Applies placement (queue label already set by webhook)
  ├─ Removes scheduling gate
  └─ Pod enters normal scheduling → lands on GPU node
```

### 8. Pod runs with GPU

```
$ kubectl exec my-model-predictor-abc12 -- nvidia-smi
Tesla T4, 15360 MiB
```

## Template Change Propagation

Admin updates `gpu-t4` to use A100 instead of T4:

```yaml
# Admin edits the template
kubectl edit workloadprofiletemplate gpu-t4
# Changes: productName "Tesla T4" → "NVIDIA A100-SXM4-80GB"
# Changes: deviceClassName gpu.nvidia.com stays the same
```

### What happens

```
Template "gpu-t4" generation 1 → 2
    │
    ↓ Profile controller detects all WorkloadProfiles referencing "gpu-t4"
    │
WorkloadProfile "my-inference" (ns: ml-team):
    status:
      templateGeneration: 2      # updated from template
      resolvedGeneration: 1      # stale — RCT still has T4 spec
      driftedWorkloads: 2
      conditions:
      - type: Drifted
        status: "True"
        message: "Template gpu-t4 updated (gen 2), profile last resolved at gen 1.
                  2 running workloads have stale configuration."
    │
    ↓ Profile controller updates RCT "wto-my-inference-gpu" with A100 spec
    │
    resolvedGeneration: 2        # now matches templateGeneration
    │
    ↓ Existing pods: keep T4 claims (immutable, still running)
    ↓ New pods: get A100 claims from the updated RCT
    │
    ↓ Dashboard shows: "my-inference: 2 drifted workloads — restart to apply A100"
    │
    ↓ User clicks "Restart" (or automation sets restartedAt annotation on parent CR)
    │
    ↓ New pods created → webhook → updated RCT → A100 allocated
    │
    driftedWorkloads: 0
    conditions:
    - type: Drifted
      status: "False"
```

## Why RCTs Come from the Profile, Not the Template

1. **RCTs are namespace-scoped.** A cluster-scoped template can't own namespace-scoped RCTs — cross-namespace owner references are invalid. The profile lives in the user's namespace and can own the RCT.

2. **Independent per-team.** Two teams using the same `gpu-t4` template get independent RCTs. Team A's quota consumption doesn't appear in Team B's objects.

3. **Clean garbage collection.** Delete the profile → RCT deleted via owner reference → no orphaned claims on next pod creation.

4. **Overrides work.** If a profile overrides the template's device count from 1 to 2, the RCT reflects the override, not the raw template.

5. **Matches K8s patterns.** DeviceClass (cluster-scoped blueprint) → ResourceClaim (namespace-scoped binding with actual allocation). StorageClass → PVC. Users already understand this.

## Architecture Summary

| Aspect | Design |
|---|---|
| Admin creates | WorkloadProfileTemplate once (cluster-scoped) |
| User/automation creates | WorkloadProfile binding per workload or namespace |
| Pod annotation references | WorkloadProfile name |
| RCT owned by | WorkloadProfile |
| Template reuse | One template, many bindings across namespaces |
| Drift detection | Built-in via templateGeneration vs resolvedGeneration |
| Propagation | Template update → RCT update → drift detection → user-triggered rollout |
| Observability | `resolvedSpec` shows exactly what will be injected; `quotaSummary` shows quota fit |
| Dashboard integration | Reads WorkloadProfile status — does not need to inspect parent CR or pods |

## Injection Architecture: Centralized vs Federated vs Resolution Engine

### Context

An alternative to WTO's centralized pod-level injection is the **federated model**: each component team (KServe, Notebook controller, llm-d, training operator) reads the profile annotation on workloads they own and injects resources into their own CRs. This is the direction the current HWP implementation takes — the webhook, dashboard, notebook controller, and KServe controller each independently read the HWP and modify workloads.

### Model A: Federated (each component team owns injection)

```
Admin creates Profile
  → KServe controller reads annotation, injects into InferenceService
  → Notebook controller reads annotation, injects into Notebook pod template
  → llm-d controller reads annotation, injects into LLMInferenceService
  → Training operator reads annotation, injects into PyTorchJob workers
```

**Strengths:**
- No ghost mutations — each controller modifies the CR it owns, so `kubectl get isvc -o yaml` shows GPU resources naturally
- Semantic awareness — KServe knows to inject into the predictor not the transformer; training operator knows workers get GPUs, master doesn't
- Scales with org structure — each team owns their integration
- No webhook ordering fragility

**Weaknesses (evidenced by current HWP bugs):**

The current HWP approach IS federated. Each component team independently reads the HWP annotation and injects resources. The result is a class of bugs caused by N teams implementing the same profile-reading logic slightly differently:

| Bug | Root Cause |
|---|---|
| RHOAIENG-47369, 47774 | KServe controller **clobbers** InferenceService tolerations instead of merging |
| RHOAIENG-49069 | Migration injects identifiers into **ALL containers** — no per-container targeting |
| RHOAIENG-59801 | Webhook sets limits differently than dashboard — **inconsistent implementations** |
| RHOAIENG-38028 | Updating HWP annotation doesn't remove old tolerations — **no cleanup logic** |
| RHOAIENG-26263, 27374 | nodeSelector not cleared when switching profiles — **each team forgot edge cases** |
| RHOAIENG-50827 | API drops nodeSelector and tolerations — **reconciliation logic differs per controller** |
| RHOAIENG-66855 | Dashboard selects CUDA image instead of ROCm for AMD — **profile semantics interpreted differently** |

Adding DRA to this model multiplies the problem: each component team would need to understand ResourceClaimTemplates, CEL device selectors, DeviceClass references, and claim-to-container binding. That is a significant API surface to replicate correctly across 5+ teams.

### Model B: Centralized (WTO's current pod-level injection)

```
Admin creates WorkloadProfile
  → WTO webhook intercepts ALL pod CREATEs
  → Injects resources, DRA claims, queue labels universally
  → One team, one code path, one bug surface
```

**Strengths:**
- Single implementation, single bug surface — the HWP bug class cannot occur
- DRA complexity (ResourceClaimTemplates, CEL selectors, claim lifecycle) contained in one operator
- Universal coverage without per-team integration work
- Consistent behavior across all workload types

**Weaknesses:**
- Ghost mutations — parent CRs don't show injected resources (blocks 7 GPUaaS requirements)
- No semantic awareness — cannot distinguish PyTorchJob worker from master, KServe predictor from transformer
- Webhook availability on critical path for all pod creation in watched namespaces
- `aaa-wto` webhook ordering is fragile

### Model C: WTO as Resolution Engine + Component Teams as Consumers (Proposed)

```
Admin creates WorkloadProfileTemplate (cluster-scoped)
User/automation creates WorkloadProfile (namespace-scoped binding)
  → WTO resolves template + overrides → status.resolvedSpec
  → WTO manages ResourceClaimTemplate lifecycle (create, update, GC)
  → WTO validates quota → status.quotaSummary
  → WTO sets QueueReady, QuotaFit, DeviceClassAvailable conditions

Component controllers consume resolvedSpec:
  → KServe reads resolvedSpec, applies to InferenceService (knows predictor vs transformer)
  → Notebook controller reads resolvedSpec, applies to Notebook pod template
  → llm-d reads resolvedSpec, applies to LLMInferenceService
  → Pod webhook remains as universal fallback for workloads without a component integration
```

**What WTO owns (centralized — the hard parts):**
- Profile CRD lifecycle (WorkloadProfileTemplate + WorkloadProfile)
- Template resolution (merge template + overrides → resolvedSpec)
- DRA ResourceClaimTemplate lifecycle (create, update, garbage collect)
- Quota pre-flight validation (quotaSummary, QuotaFit condition)
- Kueue discovery (QueueReady condition)
- DeviceClass validation (DeviceClassAvailable condition)
- Drift detection (templateGeneration vs resolvedGeneration)
- Pod-level webhook as universal fallback

**What component teams own (distributed — the semantic parts):**
- Reading `resolvedSpec` from WorkloadProfile status
- Applying the resolved spec to their specific CR in the CR-appropriate way
- Container targeting decisions (which container gets GPUs in their workload type)

**Why this is different from Model A (federated):**
- Component teams do NOT re-implement profile resolution, DRA claim creation, merge semantics, cleanup logic, or quota checking
- `resolvedSpec` is a pre-computed, validated, ready-to-apply contract — not "read the HWP CRD and figure out merge semantics yourself"
- The HWP bug class cannot occur because the complex logic (merging, cleanup, DRA) is centralized in WTO
- Component teams apply a simple, pre-resolved spec to their own CRs — a thin integration, not a full profile engine

**Why this is different from Model B (centralized):**
- No ghost mutations — component controllers modify the CRs they own
- Semantic awareness — each team applies the spec in the way that makes sense for their workload type
- No webhook ordering fragility for component-integrated workloads (the pod webhook is a fallback, not the primary path)
- CR-level observability comes naturally — the CR shows what was applied

**Tradeoff:**
- Component teams still need integration code (thin: read resolvedSpec, apply to CR)
- WTO must define and maintain the resolvedSpec contract as a stable API
- Two injection paths exist (component-level for integrated workloads, pod webhook for everything else) — must ensure consistency

### Decision

Model C is the proposed architecture. The `resolvedSpec` in WorkloadProfile status is the contract between WTO and component teams. The pod-level webhook (ADR-001) remains for universal fallback coverage.

## What Stays the Same

- **Pod-level injection.** The webhook still mutates pods at CREATE time. Universality preserved.
- **Scheduling gate pattern.** Webhook injects immutable fields + gate, controller handles mutable fields + ungate.
- **Kueue integration.** Queue label injected at pod CREATE, same ordering.
- **Conflict detection.** Same ADR-003 semantics.
- **DRA with device-plugin fallback.** Same ADR-007 logic.

## Open Questions

1. **Should WorkloadProfileTemplate be cluster-scoped or namespace-scoped?** Cluster-scoped matches DeviceClass/StorageClass. Namespace-scoped allows team-specific templates without cluster admin. Could support both (ClusterWorkloadProfileTemplate + WorkloadProfileTemplate).

2. **Auto-creation of bindings.** Should a namespace-level default profile be auto-created when a template has a `namespaceSelector`? This would reduce the user's work to zero — admin creates template with selector, profiles appear automatically in matching namespaces.

3. **Override semantics.** Strategic merge? Replace? Per-field? How deep do overrides go — can you override a single CEL selector within a deviceClaim?

4. **Dashboard UX.** The dashboard currently shows one dropdown (pick a profile). With template-binding, does the dashboard show templates? Or auto-create the binding when the user selects a template? The user-facing UX should remain "pick a hardware profile" — the template/binding split is an implementation detail. The dashboard reads `WorkloadProfile.status.resolvedSpec` and `quotaSummary` to display GPU resources and quota fit — this replaces reading the parent CR's pod template.

5. **Parent CR observability.** Under Model C (resolution engine), this is largely resolved: component controllers that consume `resolvedSpec` modify their own CRs, so parent CRs naturally reflect injected resources. The remaining gap is workloads that fall through to the pod-level webhook (no component integration). For those, the dashboard reads WorkloadProfile status directly. The ownerReference-walk annotation approach (Path B from prior discussion) may still be useful for CLI debugging of fallback-path workloads, but is no longer blocking.

6. **resolvedSpec stability.** `resolvedSpec` in WorkloadProfile status becomes the contract between WTO and all component teams. Changes to its shape are breaking changes for every consumer. It should be treated as a versioned API — field additions are safe, field removals or semantic changes require a version bump.
