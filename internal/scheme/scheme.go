// Package scheme builds the runtime.Scheme shared by every llmkube-kueue
// entrypoint: client-go core types, the Kueue API (v1beta2), and the
// LLMKube inference API (v1alpha1).
package scheme

import (
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	llmkubev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

// New returns a Scheme with all API groups llmkube-kueue works with.
func New() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		kueuev1beta2.AddToScheme,
		llmkubev1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}
