# Examples

End-to-end examples validated on OCP 4.21 / K8s 1.34 with 2x Tesla T4 GPUs, Kueue v1.3.1, and RHOAI v3.5.0-ea.2.

## Setup

Before running these examples, ensure:

1. WTO is deployed (`make deploy` from the WTO root)
2. The target namespace has the label `workload-tuning.io/enabled: "true"`
3. For Queue placement examples, the namespace also needs `kueue.openshift.io/managed: "true"`

```bash
oc label ns my-namespace workload-tuning.io/enabled=true
oc label ns my-namespace kueue.openshift.io/managed=true  # only for Queue placement
```

## Examples

| File | What it demonstrates |
|---|---|
| **Profiles** | |
| `01-profile-cpu-only.yaml` | CPU/memory profile with no GPU, no placement |
| `02-profile-gpu-node.yaml` | DRA GPU profile with Node placement (nodeSelector + tolerations) |
| `03-profile-gpu-queue.yaml` | DRA GPU profile with Queue placement (Kueue LocalQueue) |
| `04-profile-kserve-targeted.yaml` | Per-container targeting for InferenceService (targets `kserve-container` by name) |
| **Workloads** | |
| `10-pod-basic.yaml` | Bare pod with profile annotation |
| `11-job-gpu.yaml` | Batch Job with GPU profile |
| `12-notebook-gpu.yaml` | Kubeflow Notebook with GPU profile |
| `13-inferenceservice-gpu.yaml` | KServe InferenceService with per-container targeted profile |
| **Infrastructure** | |
| `20-namespace-setup.yaml` | Namespace with required labels, ResourceQuota, and LocalQueue |
