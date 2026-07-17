# Objective Analysis Matrix: Kubernetes Resource Levels for GPU Injection in a Workload Placement Operator

## Preamble: Key Kubernetes Constraints (as of K8s 1.34-1.36)

Before analyzing each level, these constraints shape every option:

### Pod Spec Field Immutability

- **container.resources.requests/limits:** Immutable after pod creation via normal update. InPlacePodVerticalScaling (beta in 1.33, GA in 1.35) allows CPU/memory resize only via the `/resize` subresource — GPUs and extended resources cannot be resized in-place because the device plugin framework does not support dynamic reallocation. QoS class transitions (e.g., Guaranteed to Burstable) are blocked even for CPU/memory. Non-restartable init containers and ephemeral containers cannot be resized. MutablePodResourcesForSuspendedJobs (alpha 1.35, beta 1.36, KEP-5440) allows mutation of CPU, memory, GPU, and extended resources on suspended Jobs' pod templates — broader scope than InPlacePodVerticalScaling.
- **pod.spec.resourceClaims (DRA):** Immutable after pod creation. DRA structured parameters (KEP-4381) reached GA in K8s 1.34 — the `resource.k8s.io/v1` APIs (ResourceClaim, DeviceClass, ResourceClaimTemplate, ResourceSlice) are stable. Classic DRA (controller-based, KEP-3063) was removed entirely in K8s 1.32.
- **tolerations:** On live pods, you can only add new tolerations, never remove or modify existing ones (existing tolerations' `tolerationSeconds` field is the sole exception). Two feature gates govern Job-level relaxation: `JobMutableNodeSchedulingDirectives` (GA since 1.27) allows toleration, nodeSelector, nodeAffinity, label, and annotation mutation on suspended Jobs **that have never been unsuspended**. `MutableSchedulingDirectivesForSuspendedJobs` (alpha 1.35, beta 1.36) extends this to **any** currently suspended Job, including previously-run-then-re-suspended Jobs, and clears `.status.startTime` on suspension.
- **nodeSelector:** Immutable on ungated/running pods. On scheduling-gated pods (KEP-3838, GA in 1.27), additions are allowed but deletions and modifications are not — mutations must further constrain, never relax. If absent, it may be set.
- **nodeAffinity:** Immutable on ungated pods. On scheduling-gated pods (KEP-3838): `requiredDuringSchedulingIgnoredDuringExecution` allows only additions of NodeSelectorRequirements to `matchExpressions`/`fieldExpressions` (narrowing only). `preferredDuringSchedulingIgnoredDuringExecution` allows all updates (preferred terms are not authoritative). If nil, may be set to anything.
- **schedulingGates:** Can only be set at creation and removed afterward; cannot be added post-creation. PodSchedulingReadiness (KEP-3521) reached GA in K8s 1.30.
- **affinity (pod affinity/anti-affinity):** Immutable after pod creation on ungated pods. Inter-pod affinity/anti-affinity cannot be injected post-creation.
- **topologySpreadConstraints:** Immutable after pod creation. Must be set at creation time or via mutating webhook.
- **overhead:** Set automatically by the RuntimeClass admission controller based on `runtimeClassName`. Immutable after pod creation. Relevant for GPU pods using specialized runtimes (e.g., kata-containers) that consume additional resources beyond container requests.
- **runtimeClassName:** Immutable after pod creation. Must be set at creation time. Relevant when GPU workloads require specific container runtimes (e.g., nvidia-container-runtime, kata).
- **priorityClassName / priority:** Immutable after pod creation. The Priority admission controller resolves `priorityClassName` to an integer `priority` value at creation time. If the referenced PriorityClass does not exist, the pod is rejected. In K8s 1.36, PodGroup `priorityClassName` is also validated at admission (KEP-5710).
- **securityContext (pod and container level):** Immutable after pod creation. Relevant for GPU workloads that require elevated privileges (e.g., `privileged: true`, specific Linux capabilities for device access, or SELinux contexts for GPU device nodes).
- **volumes and volumeMounts:** Immutable after pod creation. Relevant because some DRA drivers use volume-based claim injection patterns and GPU workloads may need host-path volumes for driver libraries.
- **initContainers / sidecar containers:** Immutable after pod creation. Cannot be added post-creation. Native sidecar containers (`restartPolicy: Always` on init containers, GA in 1.29) follow the same immutability. Relevant for GPU driver/toolkit initialization patterns.
- **serviceAccountName:** Immutable after pod creation. Relevant if GPU access requires specific ServiceAccounts for RBAC or cloud provider IAM bindings.
- **labels and annotations (pod metadata):** Fully mutable after pod creation — can add, modify, and remove freely. However, changing labels that match a controller's selector (e.g., `pod-template-hash`) will orphan the pod from its ReplicaSet/controller. Total annotation size is capped at 256 KiB per object (all keys + values combined). Label values are capped at 63 characters; annotation values have no individual limit.

### DRA-Specific Constraints (K8s 1.34-1.36)

- **DeviceTaints and Tolerations (KEP-5055):** Alpha in 1.33, `DRADeviceTaints` beta/default-on in 1.36. Device taints are analogous to node taints — `NoSchedule` prevents allocation of tainted devices, `NoExecute` evicts pods using tainted devices via the device taint eviction controller. Tolerations are specified on `ResourceClaim.spec.devices.tolerations`. `DeviceTaintRules` (separate gate `DRADeviceTaintRules`, off by default in 1.36) allow admins to taint devices without driver cooperation — but require `--runtime-config=resource.k8s.io/v1beta2=true` on kube-apiserver even with the feature gate enabled. Relevant because WTO-injected DRA claims may target devices that become tainted.
- **DRA Admin Access (KEP-5018):** Beta in 1.34, GA in 1.36. The `adminAccess` field on ResourceClaims allows monitoring/diagnostic access to devices already allocated to other pods. Restricted to namespaces labeled `resource.k8s.io/admin-access: "true"`. Admin access is not exempt from device taints.
- **Prioritized Alternatives:** Stable in 1.36. ResourceClaims support `firstAvailable` — a prioritized list of subrequests. The scheduler selects the first satisfiable subrequest. Relevant for fallback GPU selection (e.g., prefer H100, fall back to A100).
- **DRA Partitionable Devices:** Enabled by default in 1.36. Allows a single physical device to be partitioned into multiple claims (e.g., MIG slices on NVIDIA GPUs).
- **ResourceClaim granular status authorization (1.36):** The `DRAResourceClaimGranularStatusAuthorization` feature gate adds finer-grained RBAC for `resourceclaims/status` updates. Components that previously needed only update/patch now require additional permissions.
- **Webhook round-trip serialization hazard:** Mutating webhooks built against Kubernetes client libraries < 1.32 can silently strip `spec.resourceClaims` from pods during JSON round-trip — the old API schema does not include DRA fields, so they are dropped. This produces the error `must specify one of: resourceClaimName, resourceClaimTemplateName`. All mutating webhooks in the admission chain must use client libraries >= 1.32 to preserve DRA fields.

### Admission Chain Ordering

- **Execution order:** AuthN → AuthZ → Mutating Admission (including MutatingAdmissionWebhook and LimitRanger defaults) → Object Schema Validation → Validating Admission (including ValidatingAdmissionWebhook, ResourceQuota, LimitRanger validation, PodSecurity, ValidatingAdmissionPolicy).
- **LimitRange:** Dual-phase — mutates to inject default resource requests/limits (if absent), then validates that resources fall within namespace limits. LimitRange can only set defaults for CPU and memory, not extended resources like `nvidia.com/gpu`. A mutating webhook that injects GPU resources may trigger LimitRange validation failures if the injected values exceed namespace `max` limits. Built-in mutating plugins (including LimitRanger) are re-run if a mutating webhook modifies the object, so LimitRanger will apply defaults to webhook-injected containers (e.g., sidecars).
- **ResourceQuota:** Validates after mutation. A mutating webhook that injects GPU resources will cause ResourceQuota to account for the injected resources. If the namespace quota is exceeded by the injected resources, the pod creation is rejected — even though the user's original spec was within quota.
- **Pod Security Admission (PSA):** Validates after mutation. If a mutating webhook injects fields that violate the namespace's Pod Security Standard (e.g., injecting `privileged: true` in a `restricted` namespace, or adding `hostPath` volumes), the pod is rejected. The user sees a PSA violation for fields they never specified.
- **ValidatingAdmissionPolicy (CEL-based):** Validates after mutation. Custom CEL policies see the post-mutation object. Policies that restrict GPU counts, specific resource types, or DRA claim shapes will evaluate the webhook-injected values.
- **Webhook ordering:** Mutating webhook invocation order is not deterministic between different webhook configurations. `reinvocationPolicy: IfNeeded` causes re-invocation of earlier webhooks if a later webhook modifies the object, but adds latency and complexity. The API server enforces a per-webhook timeout (default 10s, max 30s). Mutating webhooks are called sequentially, not in parallel — their timeouts compound.
- **Global admission context deadline:** The entire admission chain has a 30-second global context deadline. If cumulative webhook timeouts exceed this (e.g., 4 webhooks x 10s = 40s), pod admission fails with `context deadline exceeded` — even if every webhook has `failurePolicy: Ignore`. The per-webhook `Ignore` policy is bypassed because the failure comes from the global context, not any individual webhook.
- **MutatingAdmissionPolicy (K8s 1.36):** CEL-based in-process mutation (counterpart to ValidatingAdmissionPolicy). Beta in 1.36. Could eventually replace simple mutating webhooks, eliminating the external webhook latency and availability concerns.

### Workload CR Pod Template Behavior

- **Deployment:** Mutating `spec.template` triggers a rolling update (new ReplicaSet). Any change to the pod template — including metadata like labels or annotations — creates a new RS.
- **StatefulSet:** With `RollingUpdate` strategy, template changes trigger ordered pod replacement (one at a time, descending ordinal). With `OnDelete`, changes do not trigger automatic updates. `maxUnavailable` (stable in 1.24) can accelerate rollouts but increases risk.
- **Job:** Pod templates are immutable after creation (without feature gates). With `MutablePodResourcesForSuspendedJobs` (alpha 1.35, beta 1.36), resource fields (CPU, memory, GPU, and extended resources) can be mutated on suspended Jobs. With `MutableSchedulingDirectivesForSuspendedJobs` (alpha 1.35, beta 1.36), scheduling directives (nodeSelector, nodeAffinity, tolerations, labels, annotations, schedulingGates) can be mutated on suspended Jobs. Other template fields — `image`, `command`, `env`, `volumes`, `volumeMounts`, `runtimeClassName`, `securityContext` — remain immutable even on suspended Jobs. This means a WTO that needs to inject `runtimeClassName` or `volumes` must do so at Job creation time (via CREATE webhook), not after suspension.
- **ReplicaSet:** Template mutation does not affect existing pods. Only newly created pods inherit the updated template.

### Workload Resource Spec Field Immutability

These constraints determine which fields a WTO can mutate when operating at the CR level (Levels 3-6 in this analysis). Source: `ValidateXxxUpdate` functions in `pkg/apis/apps/validation/validation.go` and `pkg/apis/batch/validation/validation.go`.

#### Deployment (apps/v1)

| Field | Mutable? |
|---|:---:|
| `spec.selector` | **No** |
| `spec.template` | Yes (triggers rolling update) |
| `spec.replicas` | Yes |
| `spec.strategy` | Yes |
| `spec.minReadySeconds` | Yes |
| `spec.revisionHistoryLimit` | Yes |
| `spec.progressDeadlineSeconds` | Yes |
| `spec.paused` | Yes |

Only `spec.selector` is immutable. All other spec fields are freely mutable. Template changes trigger a new ReplicaSet (rolling update).

#### ReplicaSet (apps/v1)

| Field | Mutable? |
|---|:---:|
| `spec.selector` | **No** |
| `spec.template` | Yes (does not affect existing pods) |
| `spec.replicas` | Yes |
| `spec.minReadySeconds` | Yes |

Only `spec.selector` is immutable. Template changes do not affect existing pods — only newly created pods inherit the updated template.

#### StatefulSet (apps/v1)

| Field | Mutable? |
|---|:---:|
| `spec.selector` | **No** |
| `spec.serviceName` | **No** |
| `spec.volumeClaimTemplates` | **No** |
| `spec.podManagementPolicy` | **No** |
| `spec.template` | Yes (triggers ordered replacement) |
| `spec.replicas` | Yes |
| `spec.updateStrategy` | Yes |
| `spec.revisionHistoryLimit` | Yes |
| `spec.minReadySeconds` | Yes |
| `spec.persistentVolumeClaimRetentionPolicy` | Yes |
| `spec.ordinals` | Yes |

StatefulSet has the most immutable fields (4): `selector`, `serviceName`, `volumeClaimTemplates`, and `podManagementPolicy`. Template changes trigger ordered pod replacement (one at a time, descending ordinal) with `RollingUpdate` strategy, or no automatic update with `OnDelete`. A KEP to allow `volumeClaimTemplates` storage resize (KEP-4650) is in draft but not yet merged.

#### DaemonSet (apps/v1)

| Field | Mutable? |
|---|:---:|
| `spec.selector` | **No** |
| `spec.template` | Yes (triggers rolling update per node) |
| `spec.updateStrategy` | Yes |
| `spec.minReadySeconds` | Yes |
| `spec.revisionHistoryLimit` | Yes |

Only `spec.selector` is immutable. Template changes trigger a rolling update across nodes.

#### Job (batch/v1)

| Field | Mutable? | Conditions |
|---|:---:|---|
| `spec.selector` | **No** | |
| `spec.completionMode` | **No** | |
| `spec.podFailurePolicy` | **No** | |
| `spec.backoffLimitPerIndex` | **No** | |
| `spec.managedBy` | **No** | |
| `spec.successPolicy` | **No** | |
| `spec.completions` | Conditional | Indexed Jobs only, must equal parallelism, can only increase |
| `spec.template` (scheduling directives) | Conditional | Mutable when suspended (nodeSelector, nodeAffinity, tolerations, labels, annotations, schedulingGates). Always-on since K8s 1.27. Extended to re-suspended Jobs in 1.36 via `MutableSchedulingDirectivesForSuspendedJobs` |
| `spec.template` (container resources) | Conditional | Mutable when suspended with no active pods. Beta/default-on in K8s 1.36 via `MutablePodResourcesForSuspendedJobs`. Covers CPU, memory, GPU, extended resources |
| `spec.template` (all other fields) | **No** | image, command, env, volumes, runtimeClassName, securityContext remain immutable even when suspended |
| `spec.parallelism` | Yes | |
| `spec.backoffLimit` | Yes | |
| `spec.activeDeadlineSeconds` | Yes | Timer resets on suspend/resume cycle (1.35+) |
| `spec.ttlSecondsAfterFinished` | Yes | |
| `spec.suspend` | Yes | Designed for toggling |
| `spec.podReplacementPolicy` | Yes | |

Job has the most immutable fields (6) plus the most complex conditional mutability rules. For a WTO operating at the Job level, the key constraint is that `spec.template` fields beyond scheduling directives and resources are immutable — `runtimeClassName`, `volumes`, `volumeMounts`, `securityContext` cannot be injected after creation, even when suspended.

#### CronJob (batch/v1)

| Field | Mutable? |
|---|:---:|
| `spec.schedule` | Yes |
| `spec.jobTemplate` | Yes |
| `spec.suspend` | Yes |
| `spec.concurrencyPolicy` | Yes |
| `spec.successfulJobsHistoryLimit` | Yes |
| `spec.failedJobsHistoryLimit` | Yes |
| `spec.startingDeadlineSeconds` | Yes |
| `spec.timeZone` | Yes |

**All CronJob spec fields are fully mutable.** No `ValidateImmutableField` calls exist in `ValidateCronJobUpdate`. Changes to `spec.jobTemplate` only affect future Job creations — existing Jobs are not modified. This makes CronJob the most flexible workload resource for CR-level injection, but the injected template only takes effect on the next scheduled run.

#### ReplicationController (core/v1) — Legacy

| Field | Mutable? |
|---|:---:|
| `spec.selector` | Yes (unlike apps/v1 resources) |
| `spec.template` | Yes |
| `spec.replicas` | Yes |
| `spec.minReadySeconds` | Yes |

**All ReplicationController spec fields are mutable**, including `spec.selector`. This is the only pod-creating resource where the selector can be changed after creation. RC is a legacy resource superseded by Deployment/ReplicaSet but still fully supported in core/v1 with no removal timeline.

#### Cross-Resource Immutability Summary

| Resource | Immutable Spec Fields |
|---|---|
| **Deployment** | `selector` |
| **ReplicaSet** | `selector` |
| **DaemonSet** | `selector` |
| **StatefulSet** | `selector`, `serviceName`, `volumeClaimTemplates`, `podManagementPolicy` |
| **Job** | `selector`, `completionMode`, `podFailurePolicy`, `backoffLimitPerIndex`, `managedBy`, `successPolicy` + most template fields |
| **CronJob** | *(none)* |
| **ReplicationController** | *(none)* |

Key implications for a WTO operating at different levels:
- **Deployment/DaemonSet/ReplicaSet level:** Only `selector` is immutable. The pod template is freely mutable, making these the simplest targets for CR-level injection. Template changes trigger rollouts (except ReplicaSet).
- **StatefulSet level:** `volumeClaimTemplates` immutability is a significant constraint if the WTO needs to inject DRA-related volumes. `serviceName` and `podManagementPolicy` are also locked.
- **Job level:** The most restrictive. Template immutability (even with feature gate exceptions for scheduling directives and resources) means fields like `runtimeClassName`, `volumes`, and `securityContext` can only be set at Job creation time.
- **CronJob level:** Fully mutable, but changes only affect future Jobs — not existing or running ones.

---

## Level 1: Pod (Current WTO Approach -- Mutating Webhook on Pod CREATE)

### Mechanism

Mutating admission webhook intercepts pod CREATE requests. Injects container.resources, pod.spec.resourceClaims, scheduling gate, and queue labels synchronously before etcd persistence. A separate placement controller later injects nodeSelector and tolerations (which are add-only mutable on live pods).

### Genuine Pros

1. **Universal coverage without per-type code.** Every workload controller (Deployment, StatefulSet, Job, KServe InferenceService, PyTorchJob, SparkApplication, RayJob, Argo Workflow) eventually creates pods. A single webhook rule on pods/CREATE catches them all. Zero CRD-specific logic.

2. **No GitOps drift.** ArgoCD and Flux track Deployments, StatefulSets, Jobs, and other CRs -- not ephemeral pods. Mutating pods leaves Git-tracked manifests untouched. No `ignoreDifferences` configuration needed. No risk of Flux reverting injected fields.

3. **No rollout side effects.** Mutating a pod does not trigger Deployment rolling updates, StatefulSet replacements, or Job restarts. The pod template in the owning CR remains unchanged.

4. **Correct DRA injection timing.** Since resourceClaims are immutable after pod creation, the only reliable injection point is either (a) a mutating webhook on pod CREATE, or (b) mutating the parent CR's pod template before pod creation. The pod webhook is the simpler path because it requires no knowledge of which CR owns the pod or where its template lives.

5. **Kueue compatibility.** Kueue's pod integration mode uses its own scheduling gate (`kueue.x-k8s.io/admission`). WTO can inject its own gate alongside Kueue's. Kueue reads resource requests from the pod spec when constructing Workload objects, so pod-level injection feeds directly into Kueue's quota calculation.

6. **Container targeting is straightforward.** The webhook receives the full pod spec with all containers enumerated. Targeting by container name or index is a direct array operation. No need to resolve the CR's pod template structure first.

### Genuine Cons

1. **Observability gap -- the "ghost mutation" problem.** Users who `kubectl get deployment -o yaml` see no GPU resources in the pod template. The injected resources are only visible on the live pod (`kubectl get pod -o yaml`). This is confusing: "Where did my GPU request come from? My Deployment doesn't specify one." Diagnosis requires knowing to look at pods, not CRs.

2. **`kubectl diff` and `kubectl apply --dry-run=server` do not show injected fields.** Dry-run operations on the Deployment show the user's original spec. The GPU injection is invisible until the pod actually exists. This breaks pre-apply validation workflows.

3. **No profile change propagation.** If a WTO placement profile changes (e.g., GPU type changes from A100 to H100), existing pods are unaffected. There is no mechanism to trigger a rollout. The operator must either (a) require users to manually restart their workloads, (b) delete pods to force recreation (destructive), or (c) accept that old pods run with stale profiles until natural replacement. This is a genuine operational limitation.

4. **Race condition window.** Between pod creation and the placement controller adding nodeSelector/tolerations, there is a brief window where the pod exists with resources but without scheduling constraints. The scheduling gate mitigates this (the pod will not be scheduled), but if the gate mechanism fails, the pod could be scheduled to a wrong node.

5. **Webhook availability is on the critical path.** If the WTO webhook is down, pod creation either fails (with `failurePolicy: Fail`) or proceeds without injection (`failurePolicy: Ignore`). Either mode is problematic for GPU workloads: failing blocks all pod creation cluster-wide (for matched namespaces); ignoring silently creates pods without GPU resources.

6. **Conflict resolution is implicit and hard to audit.** When a user specifies `resources.requests.nvidia.com/gpu: 1` and the profile says `2`, the webhook must decide: override, merge, or skip. Whatever the policy, there is no declarative record of what was overridden. The user's original intent is lost once the pod is mutated.

7. **Debugging webhook mutations requires API server audit logs.** The mutation happens inside the API server admission chain. Standard `kubectl describe pod` shows the final state, not the diff. Diagnosing "why does my pod have 4 GPUs when I asked for 2" requires either audit logs, webhook-injected annotations, or a dedicated WTO status CR.

8. **Scaling bottleneck.** Every pod creation in watched namespaces passes through the webhook. For large batch job submissions (1000+ pods), the webhook becomes a throughput bottleneck. The webhook must respond in milliseconds or risk API server timeouts.

9. **Ordering conflicts with other webhooks.** Webhook ordering is not deterministic. If Istio's sidecar injector, Vault's secret injector, and WTO's resource injector all mutate the same pod, resource calculations may be wrong (e.g., WTO does not know about Istio's sidecar container resource consumption until after Istio's webhook runs, but ordering is not guaranteed).

---

## Level 2: ReplicaSet

### Mechanism

Mutating webhook or controller targets ReplicaSet objects. Injects fields into `spec.template.spec.containers[].resources`, tolerations, and resource claims in the ReplicaSet's pod template.

### Pros

1. **Slightly higher observability than pod-level.** The injected fields are visible in `kubectl get rs -o yaml`, which is one level up from pods.

2. **Pod template mutation propagates to new pods.** Once the RS template is mutated, all new pods created by the RS inherit the injected fields without needing a per-pod webhook.

3. **DRA injection works.** Mutating the RS pod template before pods are created means resourceClaims are set correctly.

### Cons

1. **ReplicaSets are not user-facing objects.** Users create Deployments, not ReplicaSets. RSs are implementation details of the Deployment controller. Mutating them creates confusion: the Deployment's pod template shows no GPU resources, but the RS (which users rarely inspect) does.

2. **Deployment controller conflict.** When a Deployment controller creates a new RS, it copies the pod template from the Deployment spec. If WTO mutates the RS, the Deployment controller does not know about the mutation. On the next Deployment reconciliation (any annotation change, scaling event), the Deployment controller may create a new RS from the original template, losing all WTO injections.

3. **Not universal.** StatefulSets do not use ReplicaSets. Jobs do not use ReplicaSets. KServe in Knative mode does not use ReplicaSets (it uses Knative Revisions). This level covers only Deployment-owned workloads.

4. **GitOps conflict.** ArgoCD tracks ReplicaSets as children of Deployments. While it primarily diffs at the Deployment level, RS mutations create observable divergence in the resource tree.

5. **Rollout interaction is unpredictable.** Mutating an RS template does not trigger a Deployment rollout (the Deployment's template hash does not change). But it creates a state where the RS's template differs from the Deployment's, which is an inconsistency the Deployment controller may or may not reconcile depending on future Deployment updates.

6. **RBAC scope creep.** The operator needs permissions to mutate ReplicaSets, which is unusual and may raise security review concerns.

### Verdict

This level is strictly dominated by both pod-level and Deployment-level approaches. It combines the worst aspects of both: not universal enough, not user-visible enough, and creates conflicts with the Deployment controller. **It should not be used.**

---

## Level 3: Deployment

### Mechanism

Controller or mutating webhook targets Deployment objects. Injects fields into `spec.template.spec.containers[].resources`, tolerations, node selector, and resource claims in the Deployment's pod template.

### Pros

1. **Excellent observability.** `kubectl get deployment -o yaml` shows the injected GPU resources directly in the pod template. Users can see exactly what resources their pods will get. `kubectl diff` works correctly.

2. **Profile change propagation via rolling update.** When a WTO profile changes, the operator can update the Deployment's pod template, which triggers a controlled rolling update. Existing pods are gracefully replaced with new ones carrying the updated profile. This is the standard Kubernetes mechanism for configuration changes.

3. **Clear conflict resolution.** The operator can implement strategic merge semantics: if the user specifies `resources.requests.cpu: 4` and the profile adds `nvidia.com/gpu: 1`, the result is both fields set. Conflicts are visible in the Deployment spec and can be audited.

4. **DRA injection works.** The pod template in the Deployment spec accepts resourceClaims. These propagate to pods created from the template.

5. **No webhook on the critical path for pod creation.** The operator mutates the Deployment once (via a controller reconcile loop or a webhook on Deployment CREATE/UPDATE). Pod creation then proceeds normally. This avoids the per-pod webhook scaling bottleneck.

6. **Kueue integration via admission-gated-by.** Kueue's `kueue.x-k8s.io/admission-gated-by` annotation (beta) is designed exactly for this use case: an external controller gates Kueue admission, mutates the Job/workload, then removes the annotation. WTO could use this for Jobs.

### Cons

1. **Not universal.** Deployments are one workload type. The operator must also handle StatefulSets (different API, different update behavior), Jobs (immutable pod templates after creation), KServe InferenceServices (different pod template paths depending on deployment mode), PyTorchJob (`spec.pytorchReplicaSpecs.<role>.template`), SparkApplication (`spec.driver` and `spec.executor` have separate non-standard specs), and many others. Each requires separate code paths.

2. **GitOps drift.** ArgoCD and Flux track Deployments. If WTO mutates the Deployment's pod template, the live state diverges from Git. The user must configure `ignoreDifferences` in ArgoCD (using jsonPointers or jqPathExpressions for specific resource fields) or `kustomize.toolkit.fluxcd.io/ssa: ignore` annotations in Flux. This is ongoing operational burden and a source of confusion.

3. **Rollout side effects are a double-edged sword.** Profile changes triggering rolling updates is a pro (propagation) AND a con (disruption). If WTO changes a profile that affects 500 Deployments, all 500 trigger rolling updates simultaneously, potentially exhausting cluster capacity. The operator needs rollout throttling, which adds significant complexity.

4. **Unintended rollouts from non-profile changes.** If WTO needs to update any annotation or label on the Deployment's pod template (not just resources), it triggers a rolling update. Even a metadata change like adding a WTO tracking label causes pod replacement.

5. **Job pod templates are immutable.** The Deployment-level approach does not generalize to Jobs. `batch/v1` Job pod templates cannot be mutated after creation. The operator would need to use a webhook for Jobs anyway, making the architecture inconsistent.

6. **Container targeting across CRs is complex.** A Deployment has `spec.template.spec.containers[]`. A PyTorchJob has `spec.pytorchReplicaSpecs.Worker.template.spec.containers[]`. A SparkApplication has `spec.driver.containers[]` vs `spec.executor.containers[]` (or uses `spec.driver.podTemplateFile` references). Each path must be known and coded.

7. **RBAC requires broad permissions.** The operator needs patch on Deployments in all namespaces (or watched namespaces). This is a significant privilege that allows modifying any Deployment's pod template, not just resource fields.

---

## Level 4: Job

### Mechanism

Controller or mutating webhook targets `batch/v1` Job objects. Injects fields into `spec.template.spec.containers[].resources`, tolerations, and resource claims.

### Pros

1. **Good Kueue integration.** Kueue's primary integration is at the Job level. The `kueue.x-k8s.io/admission-gated-by` annotation is specifically designed for external controllers that need to mutate resource requests before Kueue calculates quota. This is the alignment point Kueue's designers intended.

2. **Job pod templates can be mutated before pods are created via suspension.** If the Job starts in suspended state, the pod template can be mutated (with MutableSchedulingDirectivesForSuspendedJobs in K8s 1.35+ and MutablePodResourcesForSuspendedJobs in K8s 1.35+). This is the officially blessed pattern for Kueue-managed Jobs.

3. **Observability is good for batch workloads.** `kubectl get job -o yaml` shows the injected resources.

4. **No rolling update concern.** Jobs do not have rolling updates. Pods are created from the template as-is.

### Cons

1. **Only covers batch workloads.** Jobs are for batch processing. Long-running services use Deployments, StatefulSets, or custom CRs. This level does not address the majority of GPU workload types (inference services, notebooks, etc.).

2. **Job pod templates are immutable after creation (without feature gates).** Before K8s 1.35, once a Job is created (even if suspended), its pod template cannot be mutated. The MutablePodResourcesForSuspendedJobs and MutableSchedulingDirectivesForSuspendedJobs feature gates are alpha in 1.35, beta in 1.36. Many production clusters do not enable alpha features. This limits the approach to recent K8s versions.

3. **CronJob interaction.** CronJobs create Jobs. If WTO mutates the Job, the CronJob's template still has the original spec. Each CronJob-created Job must be caught individually.

4. **Kubeflow training jobs are not `batch/v1` Job.** PyTorchJob, TFJob, MPIJob, XGBoostJob are separate CRDs that create their own pods. They are not standard Jobs and would not be caught by a Job-level webhook.

5. **GitOps drift applies to Jobs.** If Jobs are managed by ArgoCD (e.g., as part of a Helm chart), mutating them causes drift.

### Verdict

Job-level is the right approach specifically for batch workloads integrated with Kueue, especially with K8s 1.35+ feature gates. But it cannot be the sole injection point; it must be combined with other levels for non-batch workloads.

---

## Level 5: StatefulSet

### Mechanism

Controller or mutating webhook targets `apps/v1` StatefulSet objects. Injects fields into `spec.template.spec.containers[].resources`, tolerations, and resource claims.

### Pros

1. **Good observability.** `kubectl get statefulset -o yaml` shows injected resources.

2. **Profile propagation is available** via StatefulSet's RollingUpdate strategy (ordered, one pod at a time by descending ordinal). This is more controlled than Deployment rollouts and suitable for stateful GPU workloads like model serving with persistent state.

3. **OnDelete strategy option.** StatefulSets support OnDelete update strategy, where template changes do not trigger automatic updates. The operator could mutate the template and let users control when pods are replaced, which is a better fit for some GPU workloads.

### Cons

1. **Only covers StatefulSet workloads.** Same universality problem as Deployment-level.

2. **StatefulSet rolling updates are slower and riskier.** StatefulSets update pods one at a time in order. For a StatefulSet with 8 GPU pods, a profile change takes 8 sequential pod restarts. If one pod gets stuck, the entire rollout stalls.

3. **PVC interaction.** StatefulSets often have PVC templates. Mutating the pod template does not affect PVCs, but rolling updates can cause PVC detach/reattach, which may cause data loss if not handled correctly.

4. **GitOps drift.** Same issue as Deployments.

5. **Rare as a direct user object for GPU workloads.** Most GPU workloads use Deployments (inference), Jobs (training), or custom CRs (KServe, Kubeflow). StatefulSets for GPU are less common.

### Verdict

StatefulSet-level injection is a niche requirement. It would only be needed if the operator adopts a per-CR-type strategy, and even then it covers a small fraction of GPU workloads.

---

## Level 6: Custom High-Level CR (KServe InferenceService, Kubeflow Notebook, etc.)

### Mechanism

Controller watches specific CRDs and mutates their pod template fields. For KServe InferenceService, this means mutating `spec.predictor.podSpec.containers[].resources` (or `spec.predictor.model.resources` for built-in runtimes). For Kubeflow Notebook, it means mutating `spec.template.spec.containers[].resources`. For PyTorchJob, `spec.pytorchReplicaSpecs.<role>.template.spec.containers[].resources`.

### Pros

1. **Best observability and user experience.** Users interact with InferenceServices, Notebooks, and PyTorchJobs directly. Seeing GPU resources injected at this level matches their mental model. `kubectl get inferenceservice -o yaml` shows the complete picture.

2. **Semantic-aware injection.** At the CR level, the operator can make intelligent decisions: for a PyTorchJob, inject GPUs only into Worker replicas, not the Master. For a SparkApplication, inject into executors but not the driver. For KServe, inject into the predictor but not the transformer. This requires CR-type knowledge but produces better results.

3. **Integration with CR-level controllers.** KServe's reconciler reads the InferenceService spec to decide deployment mode, scaling, and runtime. Injecting at the CR level means KServe sees the GPU resources when making these decisions, which it would not if injection happens at the pod level.

4. **Profile change propagation through CR reconciliation.** Updating the InferenceService spec triggers KServe's reconciler to update the underlying Deployment/Knative Service, which triggers a controlled rollout. The CR's controller handles the rollout semantics appropriate for that workload type.

5. **Better Kueue integration for supported CRs.** Kueue has native integrations for Jobs, PyTorchJobs, and other CRDs. Injecting resources at the CR level means Kueue sees the correct resources when the CR-level Kueue integration constructs the Workload object.

### Cons

1. **Requires per-CR-type code for every supported workload type.** This is the fundamental scalability problem. Each CRD has a different API structure:
   - **Deployment:** `spec.template.spec.containers[]`
   - **KServe InferenceService (raw mode):** `spec.predictor.podSpec.containers[]`
   - **KServe InferenceService (serverless):** `spec.predictor.model.resources` or `spec.predictor.podSpec.containers[]`
   - **PyTorchJob:** `spec.pytorchReplicaSpecs.<role>.template.spec.containers[]`
   - **TFJob:** `spec.tfReplicaSpecs.<role>.template.spec.containers[]`
   - **SparkApplication:** `spec.driver` and `spec.executor` have non-standard resource fields
   - **Kubeflow Notebook:** `spec.template.spec.containers[]`
   - **RayJob/RayCluster:** `spec.rayClusterSpec.headGroupSpec.template` and `spec.rayClusterSpec.workerGroupSpecs[].template`
   - **Argo Workflow:** Steps and DAG tasks with inline templates

   Every new CRD requires a new code path. The operator becomes a registry of CR-type adapters.

2. **Tight coupling to upstream CRD schemas.** When KServe changes its API (e.g., from v1beta1 to v1), when Kubeflow training-operator changes its spec structure, or when a new training framework appears, the WTO operator must be updated. This creates a maintenance burden proportional to the number of supported CRDs.

3. **GitOps drift is maximized.** These CRs are the primary objects stored in Git. Mutating them guarantees drift detection by ArgoCD and Flux. Every user must configure ignore rules, and the rules are CR-type-specific (different JSON paths for each CRD).

4. **RBAC blast radius is large.** The operator needs patch permissions on every supported CRD across all namespaces. This is a broad set of privileges: inferenceservices, notebooks, pytorchjobs, sparkapplications, rayjobs, rayclusters, plus standard workload types.

5. **Conflict with CR-level controllers.** If KServe's controller reconciles and overwrites the fields WTO injected, a reconciliation fight ensues. The operator must understand each CR's controller reconciliation behavior to inject in a way that "sticks." Some controllers reset fields they own; others preserve mutations.

6. **Cannot handle unknown CRDs.** If a user deploys a custom training framework CRD that WTO has never seen, pod-level injection would work automatically; CR-level injection would not.

7. **Webhook ordering with CR-level webhooks.** KServe, Kubeflow, and Spark all have their own mutating webhooks that set defaults on their CRDs. WTO's webhook must run after these (to know the final state) but webhook ordering is not guaranteed. This leads to non-deterministic injection.

---

## Comparison Matrix

Scoring: **1** (poor) to **5** (excellent). Justifications are provided above.

| Dimension | Pod (L1) | ReplicaSet (L2) | Deployment (L3) | Job (L4) | StatefulSet (L5) | Custom CR (L6) |
|---|:---:|:---:|:---:|:---:|:---:|:---:|
| 1. Universality | 5 | 1 | 2 | 2 | 1 | 2\* |
| 2. GitOps compatibility | 5 | 3 | 2 | 2 | 2 | 1 |
| 3. Rollout side effects | 5 (none) | 3 | 2 | 4 | 3 | 3 |
| 4. DRA/resource immutability | 5 | 4 | 4 | 3\*\* | 4 | 4 |
| 5. Observability | 2 | 2 | 4 | 4 | 4 | 5 |
| 6. Conflict resolution | 2 | 2 | 3 | 3 | 3 | 4 |
| 7. Implementation complexity | 5 (minimal) | 3 | 3 | 3 | 3 | 1 |
| 8. Operational debuggability | 2 | 2 | 4 | 4 | 4 | 4 |
| 9. Multi-tenancy / RBAC | 4\*\*\* | 3 | 3 | 3 | 3 | 2 |
| 10. Ecosystem integration | 4 | 2 | 3 | 5 | 3 | 4 |
| 11. Profile change propagation | 1 | 1 | 4 | 2 | 4 | 4 |
| 12. Container targeting | 4 | 4 | 4 | 4 | 4 | 5 |
| **Weighted Total (equal)** | **44** | **30** | **38** | **39** | **38** | **39** |

**Footnotes:**

\* Custom CR scores 2 on universality because it covers only the CRDs you code support for. Adding a new CRD requires new code.

\*\* Job scores 3 on DRA/resource immutability because Job pod templates are immutable after creation. The MutablePodResourcesForSuspendedJobs gate (alpha 1.35) relaxes this but requires the Job to start suspended, which requires cooperation from the submitter or another webhook.

\*\*\* Pod-level webhook scores 4 on RBAC because the webhook itself needs no RBAC permissions to mutate pods (it returns patches to the API server). The placement controller needs patch on pods for nodeSelector/tolerations, but this is a narrow permission. However, the MutatingWebhookConfiguration itself is a powerful cluster-scoped resource.

---

## Critical Assessment and Architectural Observations

### The pod-level approach's real Achilles' heel is profile propagation

The pod-level approach scores worst on profile change propagation (dimension 11). In a production GPU cluster where GPU types, driver versions, or DRA claim shapes change, there is no mechanism to roll out changes to running workloads. The operator must choose between:

- **Manual restarts:** Unacceptable operational burden at scale.
- **Pod deletion:** Destructive and uncoordinated; risks data loss for stateful workloads.
- **Acceptance of staleness:** Pods run with outdated profiles until natural replacement.

None of these is satisfactory for a production platform.

### The observability gap is real but mitigatable

The pod-level approach can partially compensate for the observability gap by:

- Injecting annotations on the pod documenting what was injected and from which profile (e.g., `wto.example.com/injected-resources: '{"nvidia.com/gpu": "2"}'`).
- Maintaining a WTO status CRD per namespace that records all active injections.
- Providing a kubectl plugin or CLI that shows injection status.

However, these mitigations add complexity and are not standard Kubernetes patterns.

**WTO status:** WTO implements the first mitigation — the webhook injects `workload-template.io/profile-generation` and `workload-template.io/overrides` annotations on mutated pods. The profile status CRD reports `appliedWorkloads` count and `satisfiableNodes`. A kubectl plugin is not implemented (deferred to post-MVP).

### The hybrid approach deserves serious consideration

A hybrid architecture could combine:

- **Pod-level webhook** for resource and DRA injection (leveraging universality and immutability correctness).
- **CR-level controller** for observability annotations, conflict detection, and profile change propagation (triggering rollouts by annotating the parent CR).

This would require the pod webhook to detect the owning CR (via ownerReferences chain), which adds complexity but is feasible.

**WTO status:** WTO already implements the first half of this hybrid — the pod webhook handles resource/DRA injection and the placement controller handles mutable fields (ADR-002). The CR-level observability side is partially designed (ADR-010 describes drift detection and owning-workload annotations like `workload-template.io/applied-summary`) but not yet implemented. The ownerReferences chain walk is not implemented.

### Kueue's admission-gated-by annotation favors the Job level

For batch/training workloads managed by Kueue, the Kueue project has explicitly designed the `admission-gated-by` annotation for external controllers that need to mutate resource requests before admission. This is a strong signal that the Kueue community expects CR-level (specifically Job-level) mutation for quota-managed workloads. Ignoring this pattern means swimming against the ecosystem current for Kueue-integrated workloads.

**WTO status:** WTO currently uses the pod-level approach for Kueue integration — the webhook injects the `kueue.x-k8s.io/queue-name` label at pod CREATE time, relying on alphabetical webhook ordering (`aaa-wto` fires before Kueue's webhook) to ensure Kueue sees the label (ADR-013). This works but is fragile (see ADR-013 fragility warning). For batch Jobs specifically, adopting `admission-gated-by` at the Job level would be a more robust integration path and is worth evaluating for a future phase.

### The "universality vs. intelligence" tradeoff is fundamental

Pod-level injection is maximally universal but minimally intelligent. It cannot distinguish between a PyTorchJob Worker and Master, between a SparkApplication driver and executor, or between KServe's predictor and transformer. CR-level injection is maximally intelligent but minimally universal. There is no level that is both universal and intelligent. The architecture must explicitly choose where on this spectrum to sit, or implement a layered approach.

---

## WTO Implementation Mapping

How this analysis maps to WTO's current architecture and ADRs:

| Analysis Dimension | WTO Decision | ADR |
|---|---|---|
| Injection level | Pod CREATE webhook (Level 1) | ADR-001 |
| Immutable vs mutable field split | Webhook handles immutable (resources, DRA claims), controller handles mutable (nodeSelector, tolerations, labels) | ADR-002 |
| Conflict resolution | Owned fields (profile wins), merged fields (additive), blocking conflicts (pod stays gated) | ADR-003 |
| Type system | Embed native K8s types, not custom abstractions | ADR-004 |
| Quota enforcement | Pre-flight validator, not enforcer; Kueue/ResourceQuota are authoritative | ADR-005 |
| Kueue integration | Discovery only, never management; pod-level queue label injection | ADR-006, ADR-013 |
| DRA strategy | DRA with device-plugin fallback | ADR-007 |
| CRD design | Single WorkloadProfile CRD (chose universality over intelligence) | ADR-008 |
| Failure mode | `failurePolicy: Fail` — silent GPU misconfiguration worse than blocked creation | ADR-009 |
| Profile propagation | Soft enforcement — detect and report drift, don't auto-restart | ADR-010 |

### Open questions from this analysis for future phases

1. **Hybrid CR-level observability:** Should WTO walk ownerReferences to annotate parent CRs (Deployment, InferenceService) with injection status? This would close the observability gap (dimension 5) without abandoning pod-level injection.

2. **Job-level Kueue integration via `admission-gated-by`:** For Kueue-managed batch Jobs, should WTO adopt the Job-level pattern alongside the pod webhook? This would align with Kueue's intended design and eliminate the `aaa-wto` webhook ordering fragility.

3. **`MutablePodResourcesForSuspendedJobs` (K8s 1.36):** When this reaches GA, WTO could mutate suspended Job pod templates directly instead of using a pod webhook for batch workloads. Worth tracking.

4. **DRA DeviceTaints (K8s 1.36):** WTO-injected DRA claims may target devices that become tainted. WTO should eventually detect device taints and warn via profile status conditions.

5. **`firstAvailable` for fallback GPU selection:** DRA's prioritized alternatives (prefer H100, fall back to A100) are stable in K8s 1.36. WTO's DeviceClaim model should support this when the Kueue integration matures.

### WTO source files

| Component | File | Role in this analysis |
|---|---|---|
| Pod webhook | `internal/webhook/pod_webhook.go` | Level 1 implementation — resource/DRA injection at pod CREATE |
| Placement controller | `internal/controller/placement_controller.go` | Mutable field injection (nodeSelector, tolerations, labels) on gated pods |
| Profile controller | `internal/controller/profile_controller.go` | ResourceClaimTemplate lifecycle, status conditions, finalizer protection |
| CRD types | `api/v1alpha1/workloadprofile_types.go` | WorkloadProfile spec/status definition |
| Webhook config | `config/webhook/webhook.yaml` | `aaa-wto` MutatingWebhookConfiguration |
| Unit tests | `internal/webhook/pod_webhook_test.go`, `internal/controller/*_test.go` | 31 subtests covering critical paths |
