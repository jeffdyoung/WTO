package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	wtov1alpha1 "github.com/jeffdyoung/wto/api/v1alpha1"
)

const (
	ProfileAnnotation = "workload-tuning.io/profile-name"
	SchedulingGate    = "workload-tuning.io/scheduling-gate"
	GenerationAnno    = "workload-tuning.io/profile-generation"
	AppliedAtAnno     = "workload-tuning.io/applied-at"
	OverridesAnno     = "workload-tuning.io/overrides"
)

type PodMutatingWebhook struct {
	Client  client.Client
	Decoder admission.Decoder
}

func (w *PodMutatingWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := ctrl.Log.WithName("webhook")

	pod := &corev1.Pod{}
	if err := w.Decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	profileName, ok := pod.Annotations[ProfileAnnotation]
	if !ok {
		return admission.Allowed("no profile annotation")
	}

	log.Info("processing pod", "name", pod.Name, "generateName", pod.GenerateName, "profile", profileName)

	profile := &wtov1alpha1.WorkloadProfile{}
	if err := w.Client.Get(ctx, types.NamespacedName{
		Namespace: req.Namespace,
		Name:      profileName,
	}, profile); err != nil {
		log.Error(err, "failed to get WorkloadProfile", "profile", profileName)
		return admission.Errored(http.StatusInternalServerError,
			fmt.Errorf("WorkloadProfile %q not found in namespace %q: %w", profileName, req.Namespace, err))
	}

	if reason := w.checkBlockingConflicts(pod, profile); reason != "" {
		log.Info("blocking conflict detected", "reason", reason)
		return admission.Denied(fmt.Sprintf("WorkloadProfile %q conflict: %s", profileName, reason))
	}

	overrides := w.injectResources(pod, &profile.Spec)
	w.injectDRAClaims(pod, profile)
	w.injectQueueLabel(pod, &profile.Spec)
	w.addSchedulingGate(pod)
	w.setTrackingAnnotations(pod, profile, overrides)

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

func (w *PodMutatingWebhook) injectResources(pod *corev1.Pod, spec *wtov1alpha1.WorkloadProfileSpec) []string {
	var overrides []string

	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		resources := w.resolveResources(spec, container.Name, i)
		if resources == nil {
			continue
		}

		if container.Resources.Requests == nil {
			container.Resources.Requests = corev1.ResourceList{}
		}
		if container.Resources.Limits == nil {
			container.Resources.Limits = corev1.ResourceList{}
		}

		for k, v := range resources.Requests {
			if _, exists := container.Resources.Requests[k]; exists {
				overrides = append(overrides, fmt.Sprintf("containers[%d].resources.requests.%s", i, k))
			}
			container.Resources.Requests[k] = v
		}
		for k, v := range resources.Limits {
			if _, exists := container.Resources.Limits[k]; exists {
				overrides = append(overrides, fmt.Sprintf("containers[%d].resources.limits.%s", i, k))
			}
			container.Resources.Limits[k] = v
		}
	}

	return overrides
}

func (w *PodMutatingWebhook) resolveResources(spec *wtov1alpha1.WorkloadProfileSpec, containerName string, containerIndex int) *corev1.ResourceRequirements {
	for _, c := range spec.Containers {
		if c.Name != nil && *c.Name == containerName {
			return &c.Resources
		}
	}
	for _, c := range spec.Containers {
		if c.Index != nil && int(*c.Index) == containerIndex {
			return &c.Resources
		}
	}
	if spec.Defaults != nil {
		return &spec.Defaults.Resources
	}
	return nil
}

func (w *PodMutatingWebhook) injectDRAClaims(pod *corev1.Pod, profile *wtov1alpha1.WorkloadProfile) {
	for _, claim := range profile.Spec.DeviceClaims {
		templateName := fmt.Sprintf("wto-%s-%s", profile.Name, claim.Name)

		pod.Spec.ResourceClaims = append(pod.Spec.ResourceClaims, corev1.PodResourceClaim{
			Name:                      claim.Name,
			ResourceClaimTemplateName: &templateName,
		})

		// Link to the first targeted container, or the first container if no targeting
		targetIdx := 0
		for _, c := range profile.Spec.Containers {
			if c.Index != nil {
				targetIdx = int(*c.Index)
				break
			}
			if c.Name != nil {
				for i, pc := range pod.Spec.Containers {
					if pc.Name == *c.Name {
						targetIdx = i
						break
					}
				}
				break
			}
		}

		if targetIdx < len(pod.Spec.Containers) {
			pod.Spec.Containers[targetIdx].Resources.Claims = append(
				pod.Spec.Containers[targetIdx].Resources.Claims,
				corev1.ResourceClaim{Name: claim.Name},
			)
		}
	}
}

func (w *PodMutatingWebhook) injectQueueLabel(pod *corev1.Pod, spec *wtov1alpha1.WorkloadProfileSpec) {
	if spec.Placement == nil || spec.Placement.Type != wtov1alpha1.PlacementTypeQueue || spec.Placement.Queue == nil {
		return
	}
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels["kueue.x-k8s.io/queue-name"] = spec.Placement.Queue.LocalQueueName
	if spec.Placement.Queue.PriorityClass != nil {
		pod.Labels["kueue.x-k8s.io/priority-class"] = *spec.Placement.Queue.PriorityClass
	}
}

func (w *PodMutatingWebhook) addSchedulingGate(pod *corev1.Pod) {
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == SchedulingGate {
			return
		}
	}
	pod.Spec.SchedulingGates = append(pod.Spec.SchedulingGates, corev1.PodSchedulingGate{
		Name: SchedulingGate,
	})
}

func (w *PodMutatingWebhook) checkBlockingConflicts(pod *corev1.Pod, profile *wtov1alpha1.WorkloadProfile) string {
	if len(pod.Spec.ResourceClaims) > 0 && len(profile.Spec.DeviceClaims) > 0 {
		return "pod already has resourceClaims and profile has deviceClaims — dual device allocation risk. Remove existing resourceClaims or use a profile without deviceClaims."
	}

	if profile.Spec.Placement != nil && profile.Spec.Placement.Type == wtov1alpha1.PlacementTypeQueue {
		if existing, ok := pod.Labels["kueue.x-k8s.io/queue-name"]; ok {
			if profile.Spec.Placement.Queue != nil && existing != profile.Spec.Placement.Queue.LocalQueueName {
				return fmt.Sprintf(
					"pod has kueue.x-k8s.io/queue-name=%q but profile specifies queue %q — ambiguous queue assignment. Remove the label or use a matching profile.",
					existing, profile.Spec.Placement.Queue.LocalQueueName)
			}
		}
	}

	if profile.Spec.Placement != nil && profile.Spec.Placement.Type == wtov1alpha1.PlacementTypeNode && profile.Spec.Placement.Node != nil {
		for k, profileV := range profile.Spec.Placement.Node.NodeSelector {
			if podV, ok := pod.Spec.NodeSelector[k]; ok && podV != profileV {
				return fmt.Sprintf(
					"nodeSelector key %q: pod has %q but profile has %q — unsatisfiable constraint. Remove the pod's nodeSelector or use a compatible profile.",
					k, podV, profileV)
			}
		}
	}

	return ""
}

func (w *PodMutatingWebhook) setTrackingAnnotations(pod *corev1.Pod, profile *wtov1alpha1.WorkloadProfile, overrides []string) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[GenerationAnno] = fmt.Sprintf("%d", profile.Generation)

	if len(overrides) > 0 {
		data, _ := json.Marshal(overrides)
		pod.Annotations[OverridesAnno] = string(data)
	}
}
