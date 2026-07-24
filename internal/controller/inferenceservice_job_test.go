package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/defilantech/llmkube-kueue/internal/scheme"
	llmkubev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func ptr32(v int32) *int32 { return &v }

func isvcFixture(replicas *int32, suspend bool, modelRef string) *llmkubev1alpha1.InferenceService {
	return &llmkubev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
		Spec: llmkubev1alpha1.InferenceServiceSpec{
			ModelRef:  modelRef,
			Replicas:  replicas,
			Suspend:   suspend,
			Resources: &llmkubev1alpha1.InferenceResourceRequirements{GPU: 1},
		},
	}
}

func amdVulkanModel(name string) *llmkubev1alpha1.Model {
	return &llmkubev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: llmkubev1alpha1.ModelSpec{
			Hardware: &llmkubev1alpha1.HardwareSpec{
				GPU: &llmkubev1alpha1.GPUSpec{Vendor: "amd", Runtime: "vulkan", Count: 2},
			},
		},
	}
}

func TestSuspendSemantics(t *testing.T) {
	j := (*InferenceService)(isvcFixture(ptr32(3), false, ""))
	if j.IsSuspended() {
		t.Fatal("fresh isvc must not be suspended")
	}
	j.Suspend()
	if !j.IsSuspended() {
		t.Fatal("Suspend() must set spec.suspend")
	}
	if err := j.RunWithPodSetsInfo(context.Background(), nil, nil); err != nil {
		t.Fatalf("RunWithPodSetsInfo: %v", err)
	}
	if j.IsSuspended() {
		t.Fatal("RunWithPodSetsInfo must clear spec.suspend")
	}
	if got := *j.Spec.Replicas; got != 3 {
		t.Fatalf("suspend round-trip must preserve spec.replicas, got %d", got)
	}
	if j.RestorePodSetsInfo(context.Background(), nil) {
		t.Fatal("RestorePodSetsInfo must report no change (replicas preserved by design)")
	}
}

func TestFinishedNever(t *testing.T) {
	j := (*InferenceService)(isvcFixture(ptr32(1), false, ""))
	if _, _, finished := j.Finished(context.Background()); finished {
		t.Fatal("a serving workload never finishes")
	}
}

func TestPodSets(t *testing.T) {
	s, _ := scheme.New()
	cases := []struct {
		name      string
		isvc      *llmkubev1alpha1.InferenceService
		model     *llmkubev1alpha1.Model
		wantCount int32
		wantRes   corev1.ResourceName
		wantQty   int64
	}{
		{"nvidia default, nil replicas -> 1", isvcFixture(nil, false, ""), nil, 1, corev1.ResourceName("nvidia.com/gpu"), 1},
		{"amd vulkan via model, model count wins", isvcFixture(ptr32(2), false, "m"), amdVulkanModel("m"), 2, corev1.ResourceName("devic.es/dri-render"), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(s)
			if tc.model != nil {
				b = b.WithObjects(tc.model)
			}
			j := (*InferenceService)(tc.isvc)
			podSets, err := j.PodSets(context.Background(), b.Build())
			if err != nil {
				t.Fatalf("PodSets: %v", err)
			}
			if len(podSets) != 1 {
				t.Fatalf("want exactly 1 PodSet, got %d", len(podSets))
			}
			ps := podSets[0]
			if ps.Count != tc.wantCount {
				t.Errorf("Count = %d, want %d", ps.Count, tc.wantCount)
			}
			qty, ok := ps.Template.Spec.Containers[0].Resources.Requests[tc.wantRes]
			if !ok {
				t.Fatalf("PodSet requests missing %s: %+v", tc.wantRes, ps.Template.Spec.Containers[0].Resources.Requests)
			}
			if qty.Value() != tc.wantQty {
				t.Errorf("%s = %d, want %d", tc.wantRes, qty.Value(), tc.wantQty)
			}
		})
	}
}

func TestActivationAndReadiness(t *testing.T) {
	zero := (*InferenceService)(isvcFixture(ptr32(0), false, ""))
	if zero.IsWorkloadActive() {
		t.Fatal("zero desired replicas must deactivate the Workload (quota release)")
	}
	one := (*InferenceService)(isvcFixture(ptr32(1), false, ""))
	if !one.IsWorkloadActive() {
		t.Fatal("nonzero desired replicas must keep the Workload active")
	}
	one.Status.ReadyReplicas = 1
	one.Status.Replicas = 1
	if !one.PodsReady(context.Background(), nil) {
		t.Fatal("readyReplicas >= desired must report PodsReady")
	}
	if !one.IsActive() {
		t.Fatal("status.replicas > 0 must report IsActive")
	}
}
