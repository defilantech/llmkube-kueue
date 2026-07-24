package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	controllerconstants "sigs.k8s.io/kueue/pkg/controller/constants"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"

	"github.com/defilantech/llmkube-kueue/internal/scheme"
	llmkubev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	pollInterval         = 250 * time.Millisecond
	eventuallyTimeout    = 30 * time.Second
	consistentlyDuration = 2 * time.Second
)

// newTestNamespace creates a uniquely named namespace for a spec and
// schedules its deletion for test cleanup.
func newTestNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "kueue-envtest-"}}
	if err := k8sClient.Create(testCtx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})
	return ns.Name
}

// startTestReconciler boots a controller-runtime manager running the Kueue
// jobframework GenericJob reconciler for InferenceService against the shared
// envtest API server. This mirrors the wiring issue #3's cmd/main.go
// "controller" subcommand will use for real: SetupWorkloadOwnerIndex before
// starting the manager, the factory-built reconciler registered against it,
// metrics disabled since nothing here scrapes them. The manager (and its
// reconciler) is stopped when the test completes.
func startTestReconciler(t *testing.T) {
	t.Helper()

	s, err := scheme.New()
	if err != nil {
		t.Fatalf("scheme.New: %v", err)
	}

	// Each spec boots its own manager, and every one of them registers a
	// controller named "inferenceservice" (derived from the GVK kind); that
	// repeats within a single test binary process, which controller-runtime
	// rejects by default as a likely metrics/log collision. It is not one
	// here (each manager and its cache are independent), so opt out.
	skipNameValidation := true
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:     s,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: &skipNameValidation},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	// Required before Start: the reconciler looks up a job's existing
	// Workload via this index.
	if err := jobframework.SetupWorkloadOwnerIndex(testCtx, mgr.GetFieldIndexer(), gvk); err != nil {
		t.Fatalf("SetupWorkloadOwnerIndex: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("kubernetes.NewForConfig: %v", err)
	}

	ctx, cancel := context.WithCancel(testCtx)

	broadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: kubeClient.EventsV1()})
	broadcaster.StartRecordingToSink(ctx.Done())
	recorder := broadcaster.NewRecorder(s, "llmkube-kueue-test")

	// NewGenericReconcilerFactory defaults ManageJobsWithoutQueueName to
	// false (see kueue's jobframework.defaultOptions), so unlabeled
	// InferenceServices are left alone without any extra Option here.
	factory := jobframework.NewGenericReconcilerFactory(NewInferenceServiceJob)
	rec, err := factory(ctx, mgr.GetClient(), mgr.GetFieldIndexer(), recorder)
	if err != nil {
		cancel()
		t.Fatalf("reconciler factory: %v", err)
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		cancel()
		t.Fatalf("SetupWithManager: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mgr.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func newTestModel(ns, name string) *llmkubev1alpha1.Model {
	return &llmkubev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		// A bare source with no spec.hardware resolves to the NVIDIA default
		// (see pkg/apiutil.GPUResourceName), which is what these specs check.
		Spec: llmkubev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
	}
}

func newTestISVC(ns, name, modelRef string, labels map[string]string, suspend bool, replicas int32) *llmkubev1alpha1.InferenceService {
	r := replicas
	return &llmkubev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: llmkubev1alpha1.InferenceServiceSpec{
			ModelRef:  modelRef,
			Suspend:   suspend,
			Replicas:  &r,
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

// consistently polls check for consistentlyDuration, failing the test the
// moment check returns a non-nil error. A natural timeout (check held true
// for the whole window) is success.
func consistently(t *testing.T, check func(ctx context.Context) error) {
	t.Helper()
	err := wait.PollUntilContextTimeout(testCtx, pollInterval, consistentlyDuration, true, func(ctx context.Context) (bool, error) {
		if err := check(ctx); err != nil {
			return false, err
		}
		return false, nil // never "done": keep polling until the window elapses
	})
	if err != nil && !wait.Interrupted(err) {
		t.Fatalf("consistently check violated: %v", err)
	}
}

// TestReconcilerCreatesWorkloadForQueueLabeledISVC: a queue-labeled,
// suspended InferenceService gets exactly one Kueue Workload, owned by it,
// with PodSets carrying the desired replica count and GPU request.
func TestReconcilerCreatesWorkloadForQueueLabeledISVC(t *testing.T) {
	ns := newTestNamespace(t)
	model := newTestModel(ns, "model")
	if err := k8sClient.Create(testCtx, model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	isvc := newTestISVC(ns, "svc", model.Name, map[string]string{controllerconstants.QueueLabel: "q"}, true, 2)
	if err := k8sClient.Create(testCtx, isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}

	startTestReconciler(t)

	var wl kueue.Workload
	eventually(t, func(ctx context.Context) (bool, error) {
		var wls kueue.WorkloadList
		if err := k8sClient.List(ctx, &wls, client.InNamespace(ns)); err != nil {
			return false, err
		}
		if len(wls.Items) != 1 {
			return false, nil
		}
		wl = wls.Items[0]
		return true, nil
	})

	if !metav1.IsControlledBy(&wl, isvc) {
		t.Fatalf("workload %s is not controlled by isvc %s: ownerReferences=%+v", wl.Name, isvc.Name, wl.OwnerReferences)
	}
	if len(wl.Spec.PodSets) != 1 {
		t.Fatalf("want exactly 1 PodSet, got %d", len(wl.Spec.PodSets))
	}
	ps := wl.Spec.PodSets[0]
	if ps.Count != 2 {
		t.Errorf("PodSets[0].Count = %d, want 2", ps.Count)
	}
	if len(ps.Template.Spec.Containers) != 1 {
		t.Fatalf("want exactly 1 container in the PodSet template, got %d", len(ps.Template.Spec.Containers))
	}
	qty, ok := ps.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]
	if !ok {
		t.Fatalf("PodSets[0] requests missing nvidia.com/gpu: %+v", ps.Template.Spec.Containers[0].Resources.Requests)
	}
	if qty.Value() != 1 {
		t.Errorf("nvidia.com/gpu request = %d, want 1", qty.Value())
	}
}

// TestUnlabeledISVCGetsNoWorkload: an InferenceService with no queue-name
// label is invisible to the reconciler (ManageJobsWithoutQueueName defaults
// to false), so no Workload ever appears and the ISVC's spec is left alone.
func TestUnlabeledISVCGetsNoWorkload(t *testing.T) {
	ns := newTestNamespace(t)
	model := newTestModel(ns, "model")
	if err := k8sClient.Create(testCtx, model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	isvc := newTestISVC(ns, "svc", model.Name, nil, false, 1)
	if err := k8sClient.Create(testCtx, isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}

	startTestReconciler(t)

	consistently(t, func(ctx context.Context) error {
		var wls kueue.WorkloadList
		if err := k8sClient.List(ctx, &wls, client.InNamespace(ns)); err != nil {
			return err
		}
		if len(wls.Items) != 0 {
			return fmt.Errorf("workload unexpectedly created: %s", wls.Items[0].Name)
		}
		var got llmkubev1alpha1.InferenceService
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: isvc.Name}, &got); err != nil {
			return err
		}
		if got.Spec.Suspend {
			return fmt.Errorf("isvc.spec.suspend unexpectedly flipped to true")
		}
		return nil
	})
}

// TestScaleToZeroDeactivatesWorkload: once a Workload exists for a
// queue-labeled ISVC, scaling spec.replicas to 0 flips the Workload's
// spec.active to false (LLMKube-kueue's scale-to-zero quota release; see
// InferenceService.IsWorkloadActive). Kueue's scheduler is not running, so
// this never reaches admission/unsuspend territory.
func TestScaleToZeroDeactivatesWorkload(t *testing.T) {
	ns := newTestNamespace(t)
	model := newTestModel(ns, "model")
	if err := k8sClient.Create(testCtx, model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	isvc := newTestISVC(ns, "svc", model.Name, map[string]string{controllerconstants.QueueLabel: "q"}, true, 2)
	if err := k8sClient.Create(testCtx, isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}

	startTestReconciler(t)

	var wlName string
	eventually(t, func(ctx context.Context) (bool, error) {
		var wls kueue.WorkloadList
		if err := k8sClient.List(ctx, &wls, client.InNamespace(ns)); err != nil {
			return false, err
		}
		if len(wls.Items) != 1 {
			return false, nil
		}
		wlName = wls.Items[0].Name
		return true, nil
	})

	var fresh llmkubev1alpha1.InferenceService
	if err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: isvc.Name}, &fresh); err != nil {
		t.Fatalf("get isvc: %v", err)
	}
	zero := int32(0)
	fresh.Spec.Replicas = &zero
	if err := k8sClient.Update(testCtx, &fresh); err != nil {
		t.Fatalf("scale isvc to zero: %v", err)
	}

	eventually(t, func(ctx context.Context) (bool, error) {
		var wl kueue.Workload
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: wlName}, &wl); err != nil {
			return false, err
		}
		return wl.Spec.Active != nil && !*wl.Spec.Active, nil
	})
}
