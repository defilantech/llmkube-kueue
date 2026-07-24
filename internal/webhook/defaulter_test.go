package webhook

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/defilantech/llmkube-kueue/internal/scheme"
	llmkubev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func isvc(labels map[string]string, suspend bool) *llmkubev1alpha1.InferenceService {
	return &llmkubev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", Labels: labels},
		Spec:       llmkubev1alpha1.InferenceServiceSpec{Suspend: suspend},
	}
}

func TestDefaulterSuspendsQueueLabeledCreate(t *testing.T) {
	s, _ := scheme.New()
	d := &InferenceServiceDefaulter{Client: fake.NewClientBuilder().WithScheme(s).Build()}
	obj := isvc(map[string]string{"kueue.x-k8s.io/queue-name": "inference"}, false)
	if err := d.Default(context.Background(), obj); err != nil {
		t.Fatalf("Default: %v", err)
	}
	if !obj.Spec.Suspend {
		t.Fatal("queue-labeled create must be defaulted to suspend=true")
	}
}

func TestDefaulterLeavesUnlabeledAlone(t *testing.T) {
	// Defense in depth: the objectSelector keeps unlabeled objects away from
	// the webhook entirely, but the defaulter itself must also be a no-op.
	s, _ := scheme.New()
	d := &InferenceServiceDefaulter{Client: fake.NewClientBuilder().WithScheme(s).Build()}
	obj := isvc(nil, false)
	if err := d.Default(context.Background(), obj); err != nil {
		t.Fatalf("Default: %v", err)
	}
	if obj.Spec.Suspend {
		t.Fatal("unlabeled object must not be suspended")
	}
}

func TestDefaulterNeverUnsuspends(t *testing.T) {
	s, _ := scheme.New()
	d := &InferenceServiceDefaulter{Client: fake.NewClientBuilder().WithScheme(s).Build()}
	obj := isvc(map[string]string{"kueue.x-k8s.io/queue-name": "inference"}, true)
	if err := d.Default(context.Background(), obj); err != nil {
		t.Fatalf("Default: %v", err)
	}
	if !obj.Spec.Suspend {
		t.Fatal("an already-suspended object must stay suspended")
	}
}

func TestDefaulterRejectsWrongType(t *testing.T) {
	s, _ := scheme.New()
	d := &InferenceServiceDefaulter{Client: fake.NewClientBuilder().WithScheme(s).Build()}
	if err := d.Default(context.Background(), &llmkubev1alpha1.Model{}); err == nil {
		t.Fatal("non-InferenceService object must error, not panic or pass")
	}
}
