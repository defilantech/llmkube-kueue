package webhook

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

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
	llmkubeDir, err := moduleDir("github.com/defilantech/llmkube")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(llmkubeDir, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		// Loading the REAL manifest (not a hand-rolled test fixture) means
		// this suite fails the moment the served path, objectSelector, or
		// rules in config/webhook/manifests.yaml diverge from what
		// SetupWithManager actually registers below (the #416 class of
		// bug: manifest and code silently drift, admission fail-closes).
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("..", "..", "config", "webhook", "manifests.yaml")},
		},
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

	// One shared manager serves the defaulter for every spec in this
	// package, exactly the way cmd/main.go's "webhook" subcommand wires it:
	// a webhook-only manager whose server listens on envtest's allocated
	// host/port/cert-dir, with SetupWithManager registering the same
	// CustomDefaulter at controller-runtime's default path.
	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme:  s,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    testEnv.WebhookInstallOptions.LocalServingHost,
			Port:    testEnv.WebhookInstallOptions.LocalServingPort,
			CertDir: testEnv.WebhookInstallOptions.LocalServingCertDir,
		}),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "new manager:", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}
	d := &InferenceServiceDefaulter{Client: mgr.GetClient()}
	if err := d.SetupWithManager(mgr); err != nil {
		fmt.Fprintln(os.Stderr, "SetupWithManager:", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	mgrDone := make(chan struct{})
	go func() {
		defer close(mgrDone)
		_ = mgr.Start(testCtx)
	}()

	if err := waitForWebhookTLS(testEnv.WebhookInstallOptions.LocalServingHost, testEnv.WebhookInstallOptions.LocalServingPort); err != nil {
		fmt.Fprintln(os.Stderr, "waiting for webhook TLS readiness:", err)
		testCancel()
		<-mgrDone
		_ = testEnv.Stop()
		os.Exit(1)
	}

	code := m.Run()

	testCancel()
	<-mgrDone
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "envtest stop:", err)
	}
	os.Exit(code)
}

// waitForWebhookTLS polls the webhook server's port until it accepts a TLS
// handshake. The manager's Start (above) begins listening asynchronously,
// and envtest's own webhook installation only waits for the
// MutatingWebhookConfiguration to appear in API server discovery - not for
// our process to actually be up - so specs would otherwise race a cold
// server and see connection-refused on the first create.
func waitForWebhookTLS(host string, port int) error {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test-only: envtest's self-signed serving cert
	return wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return false, nil
		}
		_ = conn.Close()
		return true, nil
	})
}
