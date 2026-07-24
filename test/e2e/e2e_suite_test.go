//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/defilantech/llmkube-kueue/internal/scheme"
)

var (
	k8sClient client.Client
	testCtx   context.Context
)

const (
	pollInterval  = 250 * time.Millisecond
	eventualLimit = 3 * time.Minute
	steadyWindow  = 15 * time.Second
)

func TestMain(m *testing.M) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubeconfig:", err)
		os.Exit(1)
	}
	s, err := scheme.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testCtx = context.Background()
	os.Exit(m.Run())
}
