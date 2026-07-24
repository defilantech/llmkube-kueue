package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/defilantech/llmkube-kueue/internal/scheme"
)

var (
	testEnv    *envtest.Environment
	restCfg    *rest.Config
	k8sClient  client.Client
	testCtx    context.Context
	testCancel context.CancelFunc
)

// moduleDir resolves a dependency's on-disk module directory so envtest can
// load its CRDs without vendoring copies into this repo.
func moduleDir(mod string) (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", mod).Output()
	if err != nil {
		return "", fmt.Errorf("go list -m %s: %w", mod, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func TestMain(m *testing.M) {
	kueueDir, err := moduleDir("sigs.k8s.io/kueue")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	llmkubeDir, err := moduleDir("github.com/defilantech/llmkube")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join(kueueDir, "config", "components", "crd", "bases"),
			filepath.Join(llmkubeDir, "config", "crd", "bases"),
		},
		ErrorIfCRDPathMissing: true,
	}
	restCfg, err = testEnv.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "envtest start:", err)
		os.Exit(1)
	}

	s, err := scheme.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = testEnv.Stop()
		os.Exit(1)
	}
	k8sClient, err = client.New(restCfg, client.Options{Scheme: s})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = testEnv.Stop()
		os.Exit(1)
	}
	testCtx, testCancel = context.WithCancel(ctrl.SetupSignalHandler())

	code := m.Run()

	testCancel()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "envtest stop:", err)
	}
	os.Exit(code)
}
