package scheme

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestNewRecognizesAllAPIGroups(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "", Version: "v1", Kind: "Pod"},
		{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "Workload"},
		{Group: "inference.llmkube.dev", Version: "v1alpha1", Kind: "InferenceService"},
	} {
		if !s.Recognizes(gvk) {
			t.Errorf("scheme does not recognize %s", gvk)
		}
	}
}
