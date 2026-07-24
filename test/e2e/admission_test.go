//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kueuequeuelabel "sigs.k8s.io/kueue/pkg/controller/constants"

	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	llmkubev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestAdmissionContract drives the issue #5 scenarios as ordered subtests
// sharing one namespace and one 1-GPU ClusterQueue, because the quota
// narrative is inherently sequential: each subtest leaves the state the
// next one depends on (which InferenceService currently holds the single
// GPU slot).
//
// Field verification against $(go env GOMODCACHE)/sigs.k8s.io/kueue@v0.19.0
// /apis/kueue/v1beta2/workload_types.go before writing these assertions:
//   - WorkloadSpec.Active is `*bool` with json tag "active" -> spec.active.
//   - WorkloadAdmitted = "Admitted" is the condition type set once a
//     Workload has reserved quota and satisfied admission checks.
//   - WorkloadEvicted = "Evicted" / WorkloadDeactivated = "Deactivated" are
//     the condition types that accompany a deactivated (spec.active=false)
//     Workload.
const (
	clusterQueueName   = "e2e-gpu"
	resourceFlavorName = "e2e-default-flavor"
	localQueueName     = "e2e-queue"

	gpuResourceName = corev1.ResourceName("nvidia.com/gpu")
	modelRefName    = "e2e-model"
)

// newISVC builds an InferenceService requesting 1 GPU / 1 replica against a
// Model that does not exist (tolerated: the llmkube operator is not running
// in this e2e cluster, only the Kueue integration is, so nothing ever
// resolves modelRef). Labeled services omit spec.suspend entirely: the
// llmkube-kueue mutating webhook is what sets it true at admission time,
// and asserting on that default is the point of these scenarios.
func newISVC(name, namespace string, labeled bool) *llmkubev1alpha1.InferenceService {
	isvc := &llmkubev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: llmkubev1alpha1.InferenceServiceSpec{
			ModelRef: modelRefName,
			Replicas: int32Ptr(1),
			Resources: &llmkubev1alpha1.InferenceResourceRequirements{
				GPU: 1,
			},
		},
	}
	if labeled {
		isvc.Labels = map[string]string{
			kueuequeuelabel.QueueLabel: localQueueName,
		}
	}
	return isvc
}

func int32Ptr(v int32) *int32 { return &v }

// workloadsOwnedBy lists every Workload in ns whose OwnerReferences include
// ownerUID. Lookup is by owner UID rather than by guessing the Workload name
// jobframework generates, per the brief.
func workloadsOwnedBy(ns string, ownerUID types.UID) ([]kueuev1beta2.Workload, error) {
	var list kueuev1beta2.WorkloadList
	if err := k8sClient.List(testCtx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	var owned []kueuev1beta2.Workload
	for _, w := range list.Items {
		for _, ref := range w.OwnerReferences {
			if ref.UID == ownerUID {
				owned = append(owned, w)
				break
			}
		}
	}
	return owned, nil
}

// singleWorkloadOwnedBy returns the one Workload owned by ownerUID, nil if
// none exists yet, or an error if more than one does (a real bug, not a
// transient waiting state).
func singleWorkloadOwnedBy(ns string, ownerUID types.UID) (*kueuev1beta2.Workload, error) {
	owned, err := workloadsOwnedBy(ns, ownerUID)
	if err != nil {
		return nil, err
	}
	switch len(owned) {
	case 0:
		return nil, nil
	case 1:
		return &owned[0], nil
	default:
		names := make([]string, len(owned))
		for i, w := range owned {
			names[i] = w.Name
		}
		return nil, fmt.Errorf("expected at most one workload owned by %s, found %d: %v", ownerUID, len(owned), names)
	}
}

// getISVC fetches a fresh copy of the named InferenceService.
func getISVC(key client.ObjectKey) (*llmkubev1alpha1.InferenceService, error) {
	cur := &llmkubev1alpha1.InferenceService{}
	if err := k8sClient.Get(testCtx, key, cur); err != nil {
		return nil, err
	}
	return cur, nil
}

// scaleISVC patches spec.replicas via retry.RetryOnConflict, always
// re-Getting immediately before the Update so a stale local copy never
// clobbers a concurrent webhook/controller write.
func scaleISVC(key client.ObjectKey, replicas int32) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := getISVC(key)
		if err != nil {
			return err
		}
		cur.Spec.Replicas = int32Ptr(replicas)
		return k8sClient.Update(testCtx, cur)
	})
}

// describeISVC and describeWorkload render enough state for a failing
// assertion message to be debuggable without a follow-up kubectl.
func describeISVC(isvc *llmkubev1alpha1.InferenceService) string {
	replicas := "nil"
	if isvc.Spec.Replicas != nil {
		replicas = fmt.Sprintf("%d", *isvc.Spec.Replicas)
	}
	return fmt.Sprintf("ISVC %s/%s: suspend=%v replicas=%s labels=%v resourceVersion=%s",
		isvc.Namespace, isvc.Name, isvc.Spec.Suspend, replicas, isvc.Labels, isvc.ResourceVersion)
}

func describeWorkload(w *kueuev1beta2.Workload) string {
	active := "nil"
	if w.Spec.Active != nil {
		active = fmt.Sprintf("%v", *w.Spec.Active)
	}
	return fmt.Sprintf("Workload %s/%s: active=%s conditions=%v",
		w.Namespace, w.Name, active, w.Status.Conditions)
}

// eventually polls cond until it reports (true, nil) or eventualLimit
// elapses. cond may return (false, err) on a "not yet" iteration to attach a
// descriptive error to a still-pending state (e.g. "workload not admitted
// yet: <dump>"); eventually treats any non-true result as "keep polling" and
// surfaces the most recent such error if the deadline is reached, so a
// timeout failure always prints the object's current state rather than a
// bare "timed out".
func eventually(t *testing.T, msg string, cond func() (bool, error)) {
	t.Helper()
	var lastErr error
	pollErr := wait.PollUntilContextTimeout(testCtx, pollInterval, eventualLimit, true, func(context.Context) (bool, error) {
		ok, err := cond()
		if err != nil {
			lastErr = err
		}
		if ok {
			return true, nil
		}
		return false, nil
	})
	if pollErr != nil {
		if lastErr != nil {
			t.Fatalf("eventually %s: timed out after %s: %v", msg, eventualLimit, lastErr)
		}
		t.Fatalf("eventually %s: timed out after %s: %v", msg, eventualLimit, pollErr)
	}
}

// consistently polls cond for the entire steadyWindow and fails the instant
// cond reports a violation (false result or non-nil error), rather than
// waiting out the window before reporting. Surviving to the deadline without
// a violation (context.DeadlineExceeded) is success.
func consistently(t *testing.T, msg string, cond func() (bool, error)) {
	t.Helper()
	err := wait.PollUntilContextTimeout(testCtx, pollInterval, steadyWindow, true, func(context.Context) (bool, error) {
		ok, err := cond()
		if err != nil {
			return false, err
		}
		if !ok {
			return false, fmt.Errorf("%s: condition became false", msg)
		}
		return false, nil
	})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("consistently %s: %v", msg, err)
	}
}

// waitForGone polls until key is not found, for cleanup: the CQ and
// ResourceFlavor carry Kueue's resource-in-use finalizer, which only clears
// once their usage/references drain to zero, so a plain Delete call is not
// enough to guarantee the next run's Create of the same static name will
// succeed.
func waitForGone[T client.Object](t *testing.T, desc string, key client.ObjectKey, newObj func() T) {
	t.Helper()
	err := wait.PollUntilContextTimeout(testCtx, pollInterval, eventualLimit, true, func(context.Context) (bool, error) {
		err := k8sClient.Get(testCtx, key, newObj())
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		t.Errorf("waiting for %s to be deleted: %v", desc, err)
	}
}

func TestAdmissionContract(t *testing.T) {
	// setup: unique namespace, a 1-GPU ClusterQueue/ResourceFlavor pair,
	// and a LocalQueue in the namespace pointing at it.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-"}}
	if err := k8sClient.Create(testCtx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	flavor := &kueuev1beta2.ResourceFlavor{ObjectMeta: metav1.ObjectMeta{Name: resourceFlavorName}}
	if err := k8sClient.Create(testCtx, flavor); err != nil {
		t.Fatalf("create resourceflavor: %v", err)
	}

	cq := &kueuev1beta2.ClusterQueue{
		ObjectMeta: metav1.ObjectMeta{Name: clusterQueueName},
		Spec: kueuev1beta2.ClusterQueueSpec{
			// Empty (non-nil) selector: match every namespace, since our
			// e2e namespace name is GenerateName'd and unpredictable.
			NamespaceSelector: &metav1.LabelSelector{},
			ResourceGroups: []kueuev1beta2.ResourceGroup{{
				CoveredResources: []corev1.ResourceName{gpuResourceName},
				Flavors: []kueuev1beta2.FlavorQuotas{{
					Name: kueuev1beta2.ResourceFlavorReference(resourceFlavorName),
					Resources: []kueuev1beta2.ResourceQuota{{
						Name:         gpuResourceName,
						NominalQuota: resource.MustParse("1"),
					}},
				}},
			}},
		},
	}
	if err := k8sClient.Create(testCtx, cq); err != nil {
		t.Fatalf("create clusterqueue: %v", err)
	}

	lq := &kueuev1beta2.LocalQueue{
		ObjectMeta: metav1.ObjectMeta{Name: localQueueName, Namespace: ns.Name},
		Spec:       kueuev1beta2.LocalQueueSpec{ClusterQueue: kueuev1beta2.ClusterQueueReference(clusterQueueName)},
	}
	if err := k8sClient.Create(testCtx, lq); err != nil {
		t.Fatalf("create localqueue: %v", err)
	}

	t.Cleanup(func() {
		// Namespace first: cascades ISVCs and their owned Workloads,
		// draining ClusterQueue usage to zero so the CQ/flavor
		// resource-in-use finalizers can clear and the static-named
		// cluster-scoped objects are actually gone before this run ends
		// (required for a second run to re-Create them cleanly).
		if err := k8sClient.Delete(testCtx, ns); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete namespace %s: %v", ns.Name, err)
		}
		waitForGone(t, "namespace "+ns.Name, client.ObjectKeyFromObject(ns), func() *corev1.Namespace { return &corev1.Namespace{} })

		if err := k8sClient.Delete(testCtx, cq); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete clusterqueue %s: %v", cq.Name, err)
		}
		waitForGone(t, "clusterqueue "+cq.Name, client.ObjectKeyFromObject(cq), func() *kueuev1beta2.ClusterQueue { return &kueuev1beta2.ClusterQueue{} })

		if err := k8sClient.Delete(testCtx, flavor); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete resourceflavor %s: %v", flavor.Name, err)
		}
		waitForGone(t, "resourceflavor "+flavor.Name, client.ObjectKeyFromObject(flavor), func() *kueuev1beta2.ResourceFlavor { return &kueuev1beta2.ResourceFlavor{} })
	})

	isvcAKey := client.ObjectKey{Namespace: ns.Name, Name: "isvc-a"}
	isvcBKey := client.ObjectKey{Namespace: ns.Name, Name: "isvc-b"}
	isvcCKey := client.ObjectKey{Namespace: ns.Name, Name: "isvc-c"}

	t.Run("admit-when-free", func(t *testing.T) {
		isvcA := newISVC(isvcAKey.Name, ns.Name, true)
		if err := k8sClient.Create(testCtx, isvcA); err != nil {
			t.Fatalf("create ISVC A: %v", err)
		}

		eventually(t, "ISVC A unsuspended and its Workload Admitted", func() (bool, error) {
			cur, err := getISVC(isvcAKey)
			if err != nil {
				return false, err
			}
			w, err := singleWorkloadOwnedBy(ns.Name, cur.UID)
			if err != nil {
				return false, err
			}
			if cur.Spec.Suspend {
				if w == nil {
					return false, fmt.Errorf("still suspended, no workload yet: %s", describeISVC(cur))
				}
				return false, fmt.Errorf("still suspended: %s / %s", describeISVC(cur), describeWorkload(w))
			}
			if w == nil {
				return false, fmt.Errorf("unsuspended but no workload found yet: %s", describeISVC(cur))
			}
			if !meta.IsStatusConditionTrue(w.Status.Conditions, kueuev1beta2.WorkloadAdmitted) {
				return false, fmt.Errorf("workload not yet Admitted: %s", describeWorkload(w))
			}
			return true, nil
		})
	})

	var workloadBKey client.ObjectKey
	t.Run("second-waits-suspended", func(t *testing.T) {
		isvcB := newISVC(isvcBKey.Name, ns.Name, true)
		if err := k8sClient.Create(testCtx, isvcB); err != nil {
			t.Fatalf("create ISVC B: %v", err)
		}

		eventually(t, "ISVC B has a Workload", func() (bool, error) {
			cur, err := getISVC(isvcBKey)
			if err != nil {
				return false, err
			}
			w, err := singleWorkloadOwnedBy(ns.Name, cur.UID)
			if err != nil {
				return false, err
			}
			if w == nil {
				return false, fmt.Errorf("no workload yet: %s", describeISVC(cur))
			}
			workloadBKey = client.ObjectKeyFromObject(w)
			return true, nil
		})

		consistently(t, "ISVC B stays suspended, its Workload stays unadmitted", func() (bool, error) {
			cur, err := getISVC(isvcBKey)
			if err != nil {
				return false, err
			}
			w := &kueuev1beta2.Workload{}
			if err := k8sClient.Get(testCtx, workloadBKey, w); err != nil {
				return false, err
			}
			if !cur.Spec.Suspend {
				return false, fmt.Errorf("ISVC B unexpectedly unsuspended: %s", describeISVC(cur))
			}
			if meta.IsStatusConditionTrue(w.Status.Conditions, kueuev1beta2.WorkloadAdmitted) {
				return false, fmt.Errorf("ISVC B's workload unexpectedly admitted: %s", describeWorkload(w))
			}
			return true, nil
		})
	})

	t.Run("scale-to-zero-releases", func(t *testing.T) {
		if err := scaleISVC(isvcAKey, 0); err != nil {
			t.Fatalf("scale ISVC A to zero: %v", err)
		}

		eventually(t, "ISVC A's workload deactivated and ISVC B admitted", func() (bool, error) {
			curA, err := getISVC(isvcAKey)
			if err != nil {
				return false, err
			}
			wa, err := singleWorkloadOwnedBy(ns.Name, curA.UID)
			if err != nil {
				return false, err
			}
			if wa == nil {
				return false, fmt.Errorf("ISVC A's workload disappeared: %s", describeISVC(curA))
			}
			deactivatedA := (wa.Spec.Active != nil && !*wa.Spec.Active) ||
				meta.IsStatusConditionTrue(wa.Status.Conditions, kueuev1beta2.WorkloadEvicted) ||
				meta.IsStatusConditionTrue(wa.Status.Conditions, kueuev1beta2.WorkloadDeactivated)
			if !deactivatedA {
				return false, fmt.Errorf("ISVC A's workload not yet deactivated: %s", describeWorkload(wa))
			}

			curB, err := getISVC(isvcBKey)
			if err != nil {
				return false, err
			}
			wb, err := singleWorkloadOwnedBy(ns.Name, curB.UID)
			if err != nil {
				return false, err
			}
			if curB.Spec.Suspend || wb == nil || !meta.IsStatusConditionTrue(wb.Status.Conditions, kueuev1beta2.WorkloadAdmitted) {
				wbDesc := "no workload"
				if wb != nil {
					wbDesc = describeWorkload(wb)
				}
				return false, fmt.Errorf("ISVC B not yet admitted: %s / %s", describeISVC(curB), wbDesc)
			}
			return true, nil
		})
	})

	t.Run("scale-up-requeues-then-readmits", func(t *testing.T) {
		if err := scaleISVC(isvcAKey, 1); err != nil {
			t.Fatalf("scale ISVC A to one: %v", err)
		}

		consistently(t, "ISVC A stays suspended while B holds the quota", func() (bool, error) {
			cur, err := getISVC(isvcAKey)
			if err != nil {
				return false, err
			}
			if !cur.Spec.Suspend {
				return false, fmt.Errorf("ISVC A unexpectedly unsuspended while B holds quota: %s", describeISVC(cur))
			}
			return true, nil
		})

		if err := scaleISVC(isvcBKey, 0); err != nil {
			t.Fatalf("scale ISVC B to zero: %v", err)
		}

		eventually(t, "ISVC A unsuspended and (re)admitted after B releases quota", func() (bool, error) {
			cur, err := getISVC(isvcAKey)
			if err != nil {
				return false, err
			}
			// Assert admission, not workload identity: the re-queue may
			// admit a fresh Workload or reactivate the existing one.
			w, err := singleWorkloadOwnedBy(ns.Name, cur.UID)
			if err != nil {
				return false, err
			}
			if cur.Spec.Suspend || w == nil || !meta.IsStatusConditionTrue(w.Status.Conditions, kueuev1beta2.WorkloadAdmitted) {
				wDesc := "no workload"
				if w != nil {
					wDesc = describeWorkload(w)
				}
				return false, fmt.Errorf("ISVC A not yet re-admitted: %s / %s", describeISVC(cur), wDesc)
			}
			return true, nil
		})
	})

	t.Run("unlabeled-untouched", func(t *testing.T) {
		isvcC := newISVC(isvcCKey.Name, ns.Name, false)
		if err := k8sClient.Create(testCtx, isvcC); err != nil {
			t.Fatalf("create ISVC C: %v", err)
		}

		consistently(t, "unlabeled ISVC C stays unsuspended with no Workload", func() (bool, error) {
			cur, err := getISVC(isvcCKey)
			if err != nil {
				return false, err
			}
			if cur.Spec.Suspend {
				return false, fmt.Errorf("unlabeled ISVC C unexpectedly suspended: %s", describeISVC(cur))
			}
			owned, err := workloadsOwnedBy(ns.Name, cur.UID)
			if err != nil {
				return false, err
			}
			if len(owned) != 0 {
				return false, fmt.Errorf("unlabeled ISVC C unexpectedly owns %d workload(s): %s", len(owned), describeISVC(cur))
			}
			return true, nil
		})
	})
}
