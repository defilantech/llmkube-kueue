// Package controller implements the Kueue external integration for LLMKube
// InferenceServices: the jobframework GenericJob adapter and its wiring.
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"
	"sigs.k8s.io/kueue/pkg/podset"

	llmkubev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/apiutil"
)

// InferenceService adapts an LLMKube InferenceService to Kueue's GenericJob.
// Suspension maps to spec.suspend (LLMKube scales the workload to zero while
// preserving spec.replicas, see LLMKube#1251), so admission = clear suspend
// and quota release at scale-to-zero = Workload deactivation.
type InferenceService llmkubev1alpha1.InferenceService

var (
	gvk = llmkubev1alpha1.GroupVersion.WithKind("InferenceService")

	_ jobframework.GenericJob                      = (*InferenceService)(nil)
	_ jobframework.JobWithCustomWorkloadActivation = (*InferenceService)(nil)
)

// NewInferenceServiceJob returns an empty GenericJob for the reconciler
// factory (it decodes the live object into it).
func NewInferenceServiceJob() jobframework.GenericJob { return &InferenceService{} }

// fromObject adapts a decoded InferenceService into a GenericJob. Unused in
// this slice; it is the hook issue #3's BaseWebhookFactory needs.
//
//nolint:unused // issue #3 webhook hook
func fromObject(o runtime.Object) jobframework.GenericJob {
	return (*InferenceService)(o.(*llmkubev1alpha1.InferenceService))
}

func (j *InferenceService) Object() client.Object {
	return (*llmkubev1alpha1.InferenceService)(j)
}

func (j *InferenceService) GVK() schema.GroupVersionKind { return gvk }

func (j *InferenceService) IsSuspended() bool { return j.Spec.Suspend }

func (j *InferenceService) Suspend() { j.Spec.Suspend = true }

// RunWithPodSetsInfo unsuspends the service. Flavor-derived node selectors
// from podSetsInfo are intentionally NOT applied in this slice: LLMKube owns
// the pod template (accelerator scheduling comes from the Model), so
// admission only releases the suspend gate. Revisit with flavor guidance
// (epic slice 2).
func (j *InferenceService) RunWithPodSetsInfo(_ context.Context, _ client.Client, _ []podset.PodSetInfo) error {
	j.Spec.Suspend = false
	return nil
}

// RestorePodSetsInfo reports no change: suspension never mutates
// spec.replicas or the pod template, so there is nothing to restore.
func (j *InferenceService) RestorePodSetsInfo(_ context.Context, _ []podset.PodSetInfo) bool {
	return false
}

// Finished never reports finished: a serving workload does not complete.
// Quota is released through scale-to-zero deactivation (IsWorkloadActive)
// or deletion, not completion.
func (j *InferenceService) Finished(_ context.Context) (string, bool, bool) {
	return "", false, false
}

func (j *InferenceService) desiredReplicas() int32 {
	if j.Spec.Replicas == nil {
		return 1
	}
	return *j.Spec.Replicas
}

// PodSets describes the service as one PodSet: count = desired replicas,
// and per-replica requests carrying the GPU resource LLMKube will schedule
// (resolved from the referenced Model via the exported apiutil mapping) plus
// CPU/memory passthrough. This is what Kueue quotas against.
func (j *InferenceService) PodSets(ctx context.Context, c client.Client) ([]kueue.PodSet, error) {
	var model *llmkubev1alpha1.Model
	if j.Spec.ModelRef != "" && c != nil {
		m := &llmkubev1alpha1.Model{}
		err := c.Get(ctx, types.NamespacedName{Name: j.Spec.ModelRef, Namespace: j.Namespace}, m)
		switch {
		case err == nil:
			model = m
		case client.IgnoreNotFound(err) != nil:
			return nil, fmt.Errorf("resolving modelRef %q: %w", j.Spec.ModelRef, err)
		}
		// Not found: fall through with a nil Model; the apiutil helpers
		// resolve NVIDIA defaults, matching the operator's first-sight
		// behavior at admission time.
	}

	requests := corev1.ResourceList{}
	if count := apiutil.GPUCount((*llmkubev1alpha1.InferenceService)(j), model); count > 0 {
		requests[apiutil.GPUResourceName(model)] = *resource.NewQuantity(int64(count), resource.DecimalSI)
	}
	if j.Spec.Resources != nil {
		if j.Spec.Resources.CPU != "" {
			if q, err := resource.ParseQuantity(j.Spec.Resources.CPU); err == nil {
				requests[corev1.ResourceCPU] = q
			}
		}
		if j.Spec.Resources.Memory != "" {
			if q, err := resource.ParseQuantity(j.Spec.Resources.Memory); err == nil {
				requests[corev1.ResourceMemory] = q
			}
		}
	}

	return []kueue.PodSet{{
		Name:  "main",
		Count: j.desiredReplicas(),
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:      "inference",
					Resources: corev1.ResourceRequirements{Requests: requests},
				}},
			},
		},
	}}, nil
}

func (j *InferenceService) IsActive() bool { return j.Status.Replicas > 0 }

func (j *InferenceService) PodsReady(_ context.Context, _ client.Client) bool {
	desired := j.desiredReplicas()
	return desired > 0 && j.Status.ReadyReplicas >= desired
}

// IsWorkloadActive deactivates the Workload at zero desired replicas:
// Kueue evicts a deactivated Workload without requeueing it, which is the
// scale-to-zero quota release. Scaling back up reactivates and re-queues
// for admission.
func (j *InferenceService) IsWorkloadActive() bool { return j.desiredReplicas() > 0 }
