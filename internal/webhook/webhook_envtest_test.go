package webhook

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	llmkubev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	queueNameLabel = "kueue.x-k8s.io/queue-name"

	pollInterval      = 250 * time.Millisecond
	eventuallyTimeout = 10 * time.Second
)

// newTestNamespace creates a uniquely named namespace for a spec and
// schedules its deletion for test cleanup.
func newTestNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "webhook-envtest-"}}
	if err := k8sClient.Create(testCtx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})
	return ns.Name
}

func newTestModel(ns, name string) *llmkubev1alpha1.Model {
	return &llmkubev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		// A bare source with no spec.hardware resolves to the NVIDIA default
		// (see pkg/apiutil.GPUResourceName); irrelevant here beyond
		// satisfying the required field.
		Spec: llmkubev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
	}
}

func newTestISVC(ns, name, modelRef string, labels map[string]string, suspend bool) *llmkubev1alpha1.InferenceService {
	replicas := int32(1)
	return &llmkubev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: llmkubev1alpha1.InferenceServiceSpec{
			ModelRef:  modelRef,
			Suspend:   suspend,
			Replicas:  &replicas,
			Resources: &llmkubev1alpha1.InferenceResourceRequirements{GPU: 1},
		},
	}
}

// eventually polls check until it reports done, failing the test if
// eventuallyTimeout elapses first.
func eventually(t *testing.T, check func(ctx context.Context) (done bool, err error)) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(testCtx, pollInterval, eventuallyTimeout, true, check); err != nil {
		t.Fatalf("condition not met within %s: %v", eventuallyTimeout, err)
	}
}

// TestLabeledCreateIsSuspended: creating a queue-labeled InferenceService
// through the real API server, with spec.suspend explicitly set to false by
// the client, is stored with spec.suspend == true. This proves the served
// path (/mutate-inference-llmkube-dev-v1alpha1-inferenceservice) matches
// config/webhook/manifests.yaml's clientConfig.path end to end - envtest's
// WebhookInstallOptions only rewrites clientConfig to reach the local
// server, it doesn't fabricate the path, so a divergence here means the two
// have drifted (the #416 class of bug).
func TestLabeledCreateIsSuspended(t *testing.T) {
	ns := newTestNamespace(t)
	model := newTestModel(ns, "model")
	if err := k8sClient.Create(testCtx, model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	isvc := newTestISVC(ns, "svc", model.Name, map[string]string{queueNameLabel: "inference"}, false)
	if err := k8sClient.Create(testCtx, isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}
	if !isvc.Spec.Suspend {
		t.Fatal("Create response must already reflect the defaulted spec.suspend=true")
	}

	eventually(t, func(ctx context.Context) (bool, error) {
		var got llmkubev1alpha1.InferenceService
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: isvc.Name}, &got); err != nil {
			return false, err
		}
		return got.Spec.Suspend, nil
	})
}

// TestUnlabeledCreateUntouched: an InferenceService with no queue-name label
// never routes to the webhook at all (objectSelector), so it is stored
// exactly as sent. envtest's WebhookInstallOptions preserves objectSelector,
// failurePolicy, and rules from the loaded manifest - only clientConfig is
// rewritten to reach the local server - so this genuinely exercises the
// selector rather than a test-only stand-in for it.
func TestUnlabeledCreateUntouched(t *testing.T) {
	ns := newTestNamespace(t)
	model := newTestModel(ns, "model")
	if err := k8sClient.Create(testCtx, model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	isvc := newTestISVC(ns, "svc", model.Name, nil, false)
	if err := k8sClient.Create(testCtx, isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}
	if isvc.Spec.Suspend {
		t.Fatal("unlabeled create must not be suspended by the webhook")
	}

	var got llmkubev1alpha1.InferenceService
	if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: isvc.Name}, &got); err != nil {
		t.Fatalf("get isvc: %v", err)
	}
	if got.Spec.Suspend {
		t.Fatal("stored unlabeled isvc must not be suspended")
	}
}

// TestLabeledAlreadySuspendedStaysSuspended: a queue-labeled
// InferenceService created with spec.suspend already true stays true
// (idempotent defaulting; the defaulter never unsuspends, but this proves
// it also never trips on an object that's already in the desired state).
func TestLabeledAlreadySuspendedStaysSuspended(t *testing.T) {
	ns := newTestNamespace(t)
	model := newTestModel(ns, "model")
	if err := k8sClient.Create(testCtx, model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	isvc := newTestISVC(ns, "svc", model.Name, map[string]string{queueNameLabel: "inference"}, true)
	if err := k8sClient.Create(testCtx, isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}
	if !isvc.Spec.Suspend {
		t.Fatal("already-suspended create must stay suspended")
	}

	var got llmkubev1alpha1.InferenceService
	if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: isvc.Name}, &got); err != nil {
		t.Fatalf("get isvc: %v", err)
	}
	if !got.Spec.Suspend {
		t.Fatal("stored already-suspended isvc must stay suspended")
	}
}
