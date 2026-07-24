// Package webhook serves the admission-time defaults for Kueue-managed
// InferenceServices: a queue-labeled service starts suspended, so it can
// never run ahead of ClusterQueue admission. Hand-rolled (tekton-kueue
// style) rather than kueue's BaseWebhookFactory, whose Default path
// depends on the in-tree queue-manager options an external integration
// does not run.
package webhook

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"

	"github.com/defilantech/llmkube-kueue/internal/controller"
	llmkubev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// InferenceServiceDefaulter defaults spec.suspend=true on queue-labeled
// InferenceServices at admission. It never unsuspends and never touches
// any other field.
type InferenceServiceDefaulter struct {
	Client client.Client
}

// Default implements admission.CustomDefaulter.
func (d *InferenceServiceDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	isvc, ok := obj.(*llmkubev1alpha1.InferenceService)
	if !ok {
		return fmt.Errorf("expected an InferenceService, got %T", obj)
	}
	// manageJobsWithoutQueueName=false: only queue-labeled objects are
	// managed, so this is a no-op for anything the objectSelector lets
	// through by accident. The nil namespace selector is unused on the
	// false path (verified against kueue defaults.go: it is only read
	// inside WorkloadShouldBeSuspended's manageJobsWithoutQueueName
	// branch, which manageJobsWithoutQueueName=false skips entirely).
	return jobframework.ApplyDefaultForSuspend(ctx, controller.FromObject(isvc), d.Client, false, nil)
}

// SetupWithManager registers the defaulting webhook at controller-runtime's
// default path for the type:
// /mutate-inference-llmkube-dev-v1alpha1-inferenceservice
// (config/webhook/manifests.yaml must reference exactly this path; a
// mismatch fail-closes all labeled admission).
//
// WithCustomDefaulter (not the newer generic WithDefaulter) is used
// deliberately: Default takes runtime.Object so a single method can be
// exercised against both the typed InferenceService and an arbitrary wrong
// type (see TestDefaulterRejectsWrongType). WithDefaulter is generic over
// the concrete type passed to NewWebhookManagedBy and would require Default
// to take *llmkubev1alpha1.InferenceService directly, which cannot accept a
// *llmkubev1alpha1.Model argument to type-check the negative test.
func (d *InferenceServiceDefaulter) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &llmkubev1alpha1.InferenceService{}).
		WithCustomDefaulter(d). //nolint:staticcheck // deprecated but required: see doc comment above
		Complete()
}
