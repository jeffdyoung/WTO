package testutil

import (
	"encoding/json"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	wtov1alpha1 "github.com/jeffdyoung/wto/api/v1alpha1"
)

func NewScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(resourcev1.AddToScheme(s))
	utilruntime.Must(wtov1alpha1.AddToScheme(s))
	return s
}

func NewFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&wtov1alpha1.WorkloadProfile{}).
		Build()
}

func NewFakeRecorder(bufferSize int) *record.FakeRecorder {
	return record.NewFakeRecorder(bufferSize)
}

func DrainEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

// --- Pod Builder ---

type PodBuilder struct {
	pod *corev1.Pod
}

func NewPod(name, namespace string) *PodBuilder {
	return &PodBuilder{
		pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "busybox:latest"},
				},
			},
		},
	}
}

func (b *PodBuilder) WithProfileAnnotation(profileName string) *PodBuilder {
	if b.pod.Annotations == nil {
		b.pod.Annotations = map[string]string{}
	}
	b.pod.Annotations["workload-template.io/profile-name"] = profileName
	return b
}

func (b *PodBuilder) WithAnnotation(key, value string) *PodBuilder {
	if b.pod.Annotations == nil {
		b.pod.Annotations = map[string]string{}
	}
	b.pod.Annotations[key] = value
	return b
}

func (b *PodBuilder) WithLabel(key, value string) *PodBuilder {
	if b.pod.Labels == nil {
		b.pod.Labels = map[string]string{}
	}
	b.pod.Labels[key] = value
	return b
}

func (b *PodBuilder) WithWTOGate() *PodBuilder {
	b.pod.Spec.SchedulingGates = append(b.pod.Spec.SchedulingGates, corev1.PodSchedulingGate{
		Name: "workload-template.io/scheduling-gate",
	})
	return b
}

func (b *PodBuilder) WithSchedulingGate(name string) *PodBuilder {
	b.pod.Spec.SchedulingGates = append(b.pod.Spec.SchedulingGates, corev1.PodSchedulingGate{
		Name: name,
	})
	return b
}

func (b *PodBuilder) WithContainer(name string, requests, limits corev1.ResourceList) *PodBuilder {
	b.pod.Spec.Containers = append(b.pod.Spec.Containers, corev1.Container{
		Name:  name,
		Image: "busybox:latest",
		Resources: corev1.ResourceRequirements{
			Requests: requests,
			Limits:   limits,
		},
	})
	return b
}

func (b *PodBuilder) WithContainerNamed(name string) *PodBuilder {
	b.pod.Spec.Containers = append(b.pod.Spec.Containers, corev1.Container{
		Name:  name,
		Image: "busybox:latest",
	})
	return b
}

func (b *PodBuilder) WithNodeSelector(key, value string) *PodBuilder {
	if b.pod.Spec.NodeSelector == nil {
		b.pod.Spec.NodeSelector = map[string]string{}
	}
	b.pod.Spec.NodeSelector[key] = value
	return b
}

func (b *PodBuilder) WithResourceClaims(claims ...corev1.PodResourceClaim) *PodBuilder {
	b.pod.Spec.ResourceClaims = claims
	return b
}

func (b *PodBuilder) Build() *corev1.Pod {
	return b.pod.DeepCopy()
}

// --- Profile Builder ---

type ProfileBuilder struct {
	profile *wtov1alpha1.WorkloadProfile
}

func NewProfile(name, namespace string) *ProfileBuilder {
	return &ProfileBuilder{
		profile: &wtov1alpha1.WorkloadProfile{
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
				Namespace:  namespace,
				Generation: 1,
			},
		},
	}
}

func (b *ProfileBuilder) WithGeneration(gen int64) *ProfileBuilder {
	b.profile.Generation = gen
	return b
}

func (b *ProfileBuilder) WithDefaults(requests, limits corev1.ResourceList) *ProfileBuilder {
	b.profile.Spec.Defaults = &wtov1alpha1.ResourceDefaults{
		Resources: corev1.ResourceRequirements{
			Requests: requests,
			Limits:   limits,
		},
	}
	return b
}

func (b *ProfileBuilder) WithContainer(name *string, index *int32, requests, limits corev1.ResourceList) *ProfileBuilder {
	b.profile.Spec.Containers = append(b.profile.Spec.Containers, wtov1alpha1.ContainerResources{
		Name:  name,
		Index: index,
		Resources: corev1.ResourceRequirements{
			Requests: requests,
			Limits:   limits,
		},
	})
	return b
}

func (b *ProfileBuilder) WithDeviceClaim(claimName, deviceClassName string, count int64) *ProfileBuilder {
	b.profile.Spec.DeviceClaims = append(b.profile.Spec.DeviceClaims, wtov1alpha1.DeviceClaim{
		Name: claimName,
		Request: resourcev1.DeviceRequest{
			Name: claimName,
			Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: deviceClassName,
				Count:           count,
			},
		},
	})
	return b
}

func (b *ProfileBuilder) WithNodePlacement(nodeSelector map[string]string, tolerations []corev1.Toleration) *ProfileBuilder {
	b.profile.Spec.Placement = &wtov1alpha1.PlacementConfig{
		Type: wtov1alpha1.PlacementTypeNode,
		Node: &wtov1alpha1.NodePlacement{
			NodeSelector: nodeSelector,
			Tolerations:  tolerations,
		},
	}
	return b
}

func (b *ProfileBuilder) WithQueuePlacement(queueName string, priorityClass *string) *ProfileBuilder {
	b.profile.Spec.Placement = &wtov1alpha1.PlacementConfig{
		Type: wtov1alpha1.PlacementTypeQueue,
		Queue: &wtov1alpha1.QueuePlacement{
			LocalQueueName: queueName,
			PriorityClass:  priorityClass,
		},
	}
	return b
}

func (b *ProfileBuilder) WithCondition(condType string, status metav1.ConditionStatus, reason, message string) *ProfileBuilder {
	b.profile.Status.Conditions = append(b.profile.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: b.profile.Generation,
	})
	return b
}

func (b *ProfileBuilder) WithValidCondition(status metav1.ConditionStatus) *ProfileBuilder {
	return b.WithCondition(wtov1alpha1.ConditionValid, status, "Test", "test condition")
}

func (b *ProfileBuilder) WithTemplateRef(templateName string) *ProfileBuilder {
	b.profile.Spec.TemplateRef = &templateName
	b.profile.Spec.Defaults = nil
	b.profile.Spec.Containers = nil
	b.profile.Spec.DeviceClaims = nil
	return b
}

func (b *ProfileBuilder) WithResolvedSpec(spec *wtov1alpha1.WorkloadProfileSpec) *ProfileBuilder {
	b.profile.Status.ResolvedSpec = spec
	return b
}

func (b *ProfileBuilder) WithTemplateGeneration(gen int64) *ProfileBuilder {
	b.profile.Status.TemplateGeneration = &gen
	return b
}

func (b *ProfileBuilder) Resolve() *ProfileBuilder {
	resolved := b.profile.Spec.DeepCopy()
	b.profile.Status.ResolvedSpec = resolved
	return b
}

func (b *ProfileBuilder) WithFinalizer() *ProfileBuilder {
	b.profile.Finalizers = append(b.profile.Finalizers, "workload-template.io/profile-protection")
	return b
}

func (b *ProfileBuilder) WithDeletionTimestamp() *ProfileBuilder {
	now := metav1.Now()
	b.profile.DeletionTimestamp = &now
	return b
}

func (b *ProfileBuilder) Build() *wtov1alpha1.WorkloadProfile {
	return b.profile.DeepCopy()
}

// --- Template Builder ---

type TemplateBuilder struct {
	template *wtov1alpha1.WorkloadProfileTemplate
}

func NewTemplate(name string) *TemplateBuilder {
	return &TemplateBuilder{
		template: &wtov1alpha1.WorkloadProfileTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
				Generation: 1,
			},
		},
	}
}

func (b *TemplateBuilder) WithDefaults(requests, limits corev1.ResourceList) *TemplateBuilder {
	b.template.Spec.Defaults = &wtov1alpha1.ResourceDefaults{
		Resources: corev1.ResourceRequirements{
			Requests: requests,
			Limits:   limits,
		},
	}
	return b
}

func (b *TemplateBuilder) WithDeviceClaim(claimName, deviceClassName string, count int64) *TemplateBuilder {
	b.template.Spec.DeviceClaims = append(b.template.Spec.DeviceClaims, wtov1alpha1.DeviceClaim{
		Name: claimName,
		Request: resourcev1.DeviceRequest{
			Name: claimName,
			Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: deviceClassName,
				Count:           count,
			},
		},
	})
	return b
}

func (b *TemplateBuilder) WithNamespaceSelector(matchLabels map[string]string) *TemplateBuilder {
	b.template.Spec.NamespaceSelector = &metav1.LabelSelector{
		MatchLabels: matchLabels,
	}
	return b
}

func (b *TemplateBuilder) Build() *wtov1alpha1.WorkloadProfileTemplate {
	return b.template.DeepCopy()
}

// --- WorkloadTypeConfig Builder ---

type WTCBuilder struct {
	wtc *wtov1alpha1.WorkloadTypeConfig
}

func NewWTC(name string) *WTCBuilder {
	return &WTCBuilder{
		wtc: &wtov1alpha1.WorkloadTypeConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
				Generation: 1,
			},
		},
	}
}

func (b *WTCBuilder) WithGVKR(group, version, kind, resource string) *WTCBuilder {
	b.wtc.Spec.Group = group
	b.wtc.Spec.Version = version
	b.wtc.Spec.Kind = kind
	b.wtc.Spec.Resource = resource
	return b
}

func (b *WTCBuilder) WithPodTemplatePath(path string) *WTCBuilder {
	b.wtc.Spec.PodTemplatePath = &path
	return b
}

func (b *WTCBuilder) WithAnnotationPaths(paths ...string) *WTCBuilder {
	b.wtc.Spec.AnnotationPaths = paths
	return b
}

func (b *WTCBuilder) WithKnownContainers(names ...string) *WTCBuilder {
	b.wtc.Spec.KnownContainerNames = names
	return b
}

func (b *WTCBuilder) WithNativePropagation(v bool) *WTCBuilder {
	b.wtc.Spec.NativePropagation = v
	return b
}

func (b *WTCBuilder) Build() *wtov1alpha1.WorkloadTypeConfig {
	return b.wtc.DeepCopy()
}

// --- ProfileBuilder additions ---

func (b *ProfileBuilder) WithTargetKind(name string) *ProfileBuilder {
	b.profile.Spec.TargetKind = &name
	return b
}

// --- Admission Request Helper ---

func NewAdmissionRequest(pod *corev1.Pod) admission.Request {
	raw, _ := json.Marshal(pod)
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Namespace: pod.Namespace,
			Operation: admissionv1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
}

// --- Helpers ---

func StringPtr(s string) *string { return &s }
func Int32Ptr(i int32) *int32    { return &i }

func ResourceList(cpu, memory string) corev1.ResourceList {
	rl := corev1.ResourceList{}
	if cpu != "" {
		rl[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		rl[corev1.ResourceMemory] = resource.MustParse(memory)
	}
	return rl
}

func init() {
	// Ensure admission.Request has the right defaults
	_ = http.StatusOK
}
