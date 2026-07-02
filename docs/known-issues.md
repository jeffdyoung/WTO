# Known Issues

Issues identified during the initial audit of WTO (2026-07-02). Grouped by severity — Critical issues can cause stuck pods or block pod creation in production; High issues produce silently wrong behavior; Medium issues are undocumented limitations or code quality gaps; Low issues are minor inconsistencies.

## Critical

### C-1: Single replica with failurePolicy:Fail is a pod creation SPOF

**Location:** `config/manager/manager.yaml`, `config/webhook/webhook.yaml`

**Problem:** The deployment runs 1 replica. The webhook uses `failurePolicy: Fail`. During any pod restart, node drain, OOM kill, or rolling upgrade, the webhook is unavailable and ALL pod creation in WTO-enabled namespaces is blocked — including scale-up events, Job scheduling, and pod restarts for existing workloads.

ADR-009 explicitly states the mitigation (multiple replicas, anti-affinity, liveness/readiness probes, PodDisruptionBudget, `system-cluster-critical` priority class), but the manifests implement none of it.

**Impact:** WTO becomes the single point of failure for all pod scheduling in opted-in namespaces.

**Fix:** Add to `config/manager/manager.yaml`: replicas >= 2, anti-affinity, liveness/readiness probes on `/healthz` and `/readyz`, PDB with `minAvailable: 1`, `system-cluster-critical` priorityClassName. Tracked in Phase 10 (Hardening).

---

### C-2: Webhook references ResourceClaimTemplates before ProfileReconciler creates them

**Location:** `internal/webhook/pod_webhook.go` (injectDRAClaims), `internal/controller/profile_controller.go` (ensureResourceClaimTemplate)

**Problem:** A user creates a WorkloadProfile with deviceClaims and immediately creates a pod referencing that profile. The webhook fires synchronously during pod CREATE, generates the template name `wto-<profile>-<claim>`, and injects a `ResourceClaimTemplateName` reference into `pod.spec.resourceClaims`. If the ProfileReconciler has not yet reconciled, the ResourceClaimTemplate does not exist. The pod is admitted with a dangling reference — the DRA scheduler cannot find the template, and the pod goes Pending with a confusing error.

The webhook does not check whether the template exists, and does not check the profile's `Valid` condition status.

**Impact:** Pods created immediately after a new profile are admitted with dangling DRA references, leading to Pending pods with unclear errors.

**Fix:** The webhook should verify that the ResourceClaimTemplate `wto-<profile>-<claim>` exists before referencing it. If it does not exist, return an admission error with a clear message ("WorkloadProfile gpu-t4 is not yet ready — ResourceClaimTemplate wto-gpu-t4-gpu has not been created. Retry in a few seconds."). Alternatively, check the profile's `Valid` condition.

---

### C-3: Deleted WorkloadProfile causes permanently stuck pods

**Location:** `internal/controller/placement_controller.go` (Reconcile, lines 47-54)

**Problem:** If a WorkloadProfile is deleted while pods referencing it are still gated, the PlacementReconciler tries to fetch the profile, gets a NotFound error, emits a warning event, and requeues after 10 seconds. This cycle repeats indefinitely. The pod is stuck with the scheduling gate forever.

There is no finalizer on the WorkloadProfile to prevent deletion while gated pods reference it. There is no timeout or fallback to ungate the pod. Manual recovery requires patching the pod to remove the scheduling gate, which is not documented.

**Impact:** Deleted profiles leave pods permanently gated with no automated recovery path.

**Fix:** Two complementary approaches:
1. Add a finalizer to WorkloadProfile that prevents deletion while gated pods reference it (with an event telling the user which pods are blocking deletion).
2. In the PlacementReconciler, when the profile is NotFound, ungate the pod with a warning event ("WorkloadProfile deleted — pod ungated without placement configuration. Resources and DRA claims were set at creation time and remain intact.").

---

## High

### H-1: DRA claim container targeting is broken for multi-claim profiles

**Location:** `internal/webhook/pod_webhook.go` (injectDRAClaims, lines 134-155)

**Problem:** The method determines which container should receive the `resources.claims` reference by iterating `profile.Spec.Containers` and using `break` after the first matching entry. ALL device claims are linked to whichever container matches the first `containers[]` entry — regardless of which container the claim was intended for.

For a profile with two device claims targeting different containers (e.g., GPU for the training container, NIC for the network container), both claims are assigned to the same container. For profiles with no `containers[]` entries, all claims default to `containers[0]`.

**Impact:** Multi-claim profiles silently assign all claims to the wrong container.

**Fix:** Each DeviceClaim should optionally specify a target container name or index. The `injectDRAClaims` loop should resolve the target per-claim, not globally. For MVP, consider requiring all claims target the same container (and documenting this limitation) until the per-claim targeting is implemented.

---

### H-2: satisfiableNodes count ignores node taints

**Location:** `internal/controller/profile_controller.go` (nodeSatisfiesProfile, lines 180-189)

**Problem:** The toleration-matching loop sets a `matched` variable but the branch body is empty — `if !matched && tol.Key != "" { }` does nothing. The function returns `true` regardless of whether the node has taints that the profile does not tolerate.

A profile without a GPU toleration counts nodes tainted with `nvidia.com/gpu:NoSchedule` as satisfiable.

**Impact:** `satisfiableNodes` status field is overstated, giving users false confidence.

**Fix:** Implement the taint/toleration matching logic: for each node taint with effect `NoSchedule` or `NoExecute`, check if the profile's tolerations cover it. If any taint is uncovered, the node is not satisfiable.

---

### H-3: Quota pre-flight check only examines the first container's resources

**Location:** `internal/controller/placement_controller.go` (resolveProfileResources, lines 183-191)

**Problem:** `resolveProfileResources` returns `profile.Spec.Containers[0].Resources` or `profile.Spec.Defaults.Resources` — the resources of a single container. For a multi-container profile, the quota check validates against one container's request, but the pod's aggregate request is the sum of all containers.

**Impact:** Multi-container pods pass the pre-flight quota check, get ungated, then fail at real ResourceQuota enforcement. Users see contradictory signals — WTO says quota is fine, then Kubernetes says it's exceeded.

**Fix:** Sum resources across all containers the profile would inject into. For profiles using only `defaults`, multiply by the number of containers in the pod (unknown at profile reconciliation time, but known in the placement controller which has access to the pod).

---

### H-4: Queue label set in both webhook and controller with inconsistent profile generation

**Location:** `internal/webhook/pod_webhook.go` (injectQueueLabel), `internal/controller/placement_controller.go` (applyQueuePlacement)

**Problem:** The webhook injects `kueue.x-k8s.io/queue-name` at CREATE time. The controller re-applies the same label during ungating. If the profile is updated between pod creation and ungating (e.g., `localQueueName` changes), the controller applies the new queue name, overwriting what the webhook set. But resources and DRA claims reflect the old profile version (immutable after creation).

The pod has a resource configuration from profile generation N but a queue assignment from generation N+1.

**Impact:** Silent consistency violation — pod has mixed-generation configuration with no detection or warning.

**Fix:** The controller should compare the profile's current generation against the pod's `workload-tuning.io/profile-generation` annotation. If they differ, emit a warning event but use the generation that was captured at CREATE time (from the annotation). The queue label should not be overwritten if it was already set by the webhook.

---

### H-5: resource.k8s.io/v1 scheme may not be registered

**Location:** `cmd/main.go` (lines 23-26)

**Problem:** The code registers `clientgoscheme` and `wtoapi` into the scheme, but does not explicitly register `resourcev1 "k8s.io/api/resource/v1"`. The ProfileReconciler uses `client.List` for DeviceClassList and `client.Get` for ResourceClaimTemplate. If the client-go version does not include `resource.k8s.io/v1` in its default scheme, these calls return "no kind registered" errors at runtime.

**Impact:** Profile controller may panic or fail to reconcile on clusters where the scheme is not auto-registered.

**Fix:** Add explicit `utilruntime.Must(resourcev1.AddToScheme(scheme))` in `cmd/main.go`. The current code works on K8s 1.34 because client-go v0.36.0 includes resource.k8s.io/v1 in its default scheme, but this is not guaranteed across versions and should be explicit.

---

## Medium

### M-1: Init containers are not handled

**Location:** `internal/webhook/pod_webhook.go` (injectResources)

`injectResources` iterates only `pod.Spec.Containers`. Init containers are ignored. Workloads with GPU-needing init containers (data preparation, model download) cannot be targeted. This is an undocumented limitation. ADR-012 discusses per-container targeting without distinguishing init containers from regular containers.

---

### M-2: No duplicate deviceClaim name validation

**Location:** `api/v1alpha1/workloadprofile_types.go` (DeviceClaims field)

Two `deviceClaims` entries with the same `name` produce the same ResourceClaimTemplate name, causing overwrites. The webhook adds two `pod.spec.resourceClaims` entries with the same claim name, which is invalid and rejected by the API server with a cryptic error. No CEL validation rule prevents this at profile creation time.

---

### M-3: ResourceClaimTemplateName field path may differ across K8s versions

**Location:** `internal/webhook/pod_webhook.go` (line 133)

The code sets `ResourceClaimTemplateName` directly on `corev1.PodResourceClaim`. In some Kubernetes API versions, this field was restructured under `Source.ResourceClaimTemplateName`. If the Go module is compiled against a different client-go version, the field may be silently ignored. Pinning to client-go v0.36.0 (K8s 1.36) makes this safe for now, but the risk should be noted for future dependency updates.

---

### M-4: No custom metrics or observability instrumentation

**Location:** `cmd/main.go`

The metrics server is configured on port 8080 but no custom metrics are registered. No counters for pods mutated, pods gated/ungated, profiles reconciled, quota check outcomes, webhook latency, or error rates. The operator is a black box in production. Tracked in Phase 10 (Hardening).

---

### M-5: countSatisfiableNodes and countAppliedWorkloads are O(n) full-list operations

**Location:** `internal/controller/profile_controller.go`

`countSatisfiableNodes` lists all cluster nodes. `countAppliedWorkloads` lists all pods in the namespace. With 100 profiles, 500 nodes, and 2000 pods per namespace, each reconciliation performs 100 * (500 + 2000) comparisons. The deployment memory limit is 256Mi. Large clusters risk OOM, which feeds back into C-1 (single replica, no probes).

---

### M-6: Webhook does not validate profile Valid condition before injection

**Location:** `internal/webhook/pod_webhook.go`

A WorkloadProfile with `Valid: False` (e.g., DeviceClass deleted) is still used for injection. The pod gets DRA references to non-existent DeviceClasses, goes Pending with a DRA error, and the user sees no WTO-level indication that the profile was invalid.

Related to C-2 — both are cases where the webhook proceeds without checking profile readiness.

---

### M-7: ADR-005 and README describe features that are not implemented

**Location:** `docs/architecture-decisions.md` (ADR-005), `README.md`

The documents describe `QuotaFit` condition, `quotaSummary` status field, and continuous quota validation on the profile status. None of these exist in the code. The ProfileReconciler does not perform quota-related validation — only the PlacementReconciler checks quota at per-pod gate time. Engineers reading the docs will expect features that do not exist.

---

### M-8: RBAC is not least-privilege

**Location:** `config/rbac/rbac.yaml`

- `pods` has `update` verb, but the code only uses `Patch`. `update` is broader.
- `workloadprofiles` has `update` and `patch` on the main resource, but the controller only patches the status subresource.
- `workloadprofiles/finalizers` has `update`, but no finalizer is used. Dead RBAC.
- No RBAC for Kueue resources (ClusterQueue, LocalQueue) despite ADR-006 describing read access.

---

## Low

### L-1: json.Marshal error silently discarded

**Location:** `internal/webhook/pod_webhook.go` (setTrackingAnnotations, line 222)

`data, _ := json.Marshal(overrides)` discards the error. While unlikely to fail for `[]string`, a failure would silently lose the override record.

---

### L-2: AppliedAtAnno constant defined but never set

**Location:** `internal/webhook/pod_webhook.go` (line 23)

The constant `AppliedAtAnno = "workload-tuning.io/applied-at"` is defined but `setTrackingAnnotations` never sets it. The README lists it as a tracking annotation.

---

### L-3: ADR-013 aaa-wto naming is fragile

**Location:** `docs/architecture-decisions.md` (ADR-013)

The ADR states alphabetical ordering is part of the K8s API contract, which is correct. However, any third-party operator that also names their webhook `aa-*` or `aaa-*` could break the ordering. The ADR should acknowledge this and recommend CI validation of webhook ordering.

---

### L-4: Self-mutation risk if wto-system is labeled for WTO

If `wto-system` has `workload-tuning.io/enabled: "true"`, any WTO pod restart would be intercepted by its own webhook. Without the profile annotation it's a no-op, but if the namespace is labeled carelessly and any pod has the annotation, WTO must be running for WTO to start — a circular dependency. The webhook's `namespaceSelector` should explicitly exclude `wto-system`.
