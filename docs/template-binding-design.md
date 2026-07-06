# WorkloadProfileTemplate + WorkloadProfile: Template-Binding Design

## Problem

WTO's single-CRD model (WorkloadProfile) has three weaknesses scored in the [approach analysis](approach-analysis.md):

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
apiVersion: workload-tuning.io/v1alpha1
kind: WorkloadProfileTemplate
metadata:
  name: gpu-t4
  annotations:
    workload-tuning.io/display-name: "Tesla T4 (1 GPU)"
    workload-tuning.io/description: "1x T4 GPU with 1 CPU and 4Gi memory"
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
apiVersion: workload-tuning.io/v1alpha1
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
  conditions:
  - type: Valid
    status: "True"
  - type: TemplateFound
    status: "True"
  - type: DeviceClassAvailable
    status: "True"
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
    workload-tuning.io/profile-name: my-inference   # references the binding
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

## What Changes from the Current Single-CRD Model

| Aspect | Current (single CRD) | Template-binding model |
|---|---|---|
| Admin creates | WorkloadProfile per namespace | WorkloadProfileTemplate once (cluster-scoped) |
| User creates | Nothing (profile already exists) | WorkloadProfile binding per workload or namespace |
| Pod annotation references | WorkloadProfile name | WorkloadProfile name (unchanged) |
| RCT owned by | WorkloadProfile | WorkloadProfile (unchanged) |
| Template reuse | Duplicate profiles across namespaces | One template, many bindings |
| Drift detection | Not implemented | Built-in via template/resolved generation comparison |
| Propagation | Not possible | Template update → RCT update → drift detection → user-triggered rollout |
| Observability | Profile status only | Profile status shows template ref, drift state, per-workload status |

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

4. **Migration from single-CRD.** Existing WorkloadProfiles become WorkloadProfileTemplates. New WorkloadProfiles (bindings) are created referencing them. The webhook annotation (`workload-tuning.io/profile-name`) still points to the binding, so workloads don't need to change.

5. **Dashboard UX.** The dashboard currently shows one dropdown (pick a profile). With template-binding, does the dashboard show templates? Or auto-create the binding when the user selects a template? The user-facing UX should remain "pick a hardware profile" — the template/binding split is an implementation detail.

6. **When to implement.** This is a post-MVP evolution. The current single-CRD model works for the POC. The template-binding split can be introduced as a v1alpha2 API version with a conversion webhook from v1alpha1.
