// Package samples server-creates every reference manifest under
// config/samples against a real envtest API server. docs/queue-topology.md
// (Task 2) walks a reader through these exact files, so a real Create (not
// dry-run) forces the Kueue and LLMKube CRDs' structural schemas and CEL
// rules to validate here: schema drift fails this suite instead of quietly
// breaking the doc walkthrough.
package samples

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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

// samplesGlob is relative to this package's directory (test/samples), which
// is what `go test` uses as its working directory.
const samplesGlob = "../../config/samples/*.yaml"

// TestSamplesApplyCleanly walks every config/samples/*.yaml fixture, splits
// it into its constituent YAML documents, and server-creates each one as an
// unstructured.Unstructured. Every document must apply cleanly: batch/v1
// (the Job sample) ships built into envtest and the Kueue + LLMKube CRDs are
// loaded explicitly above, so no kind here is ever expected to be
// unrecognized — any Create error, including an unknown kind, fails the
// test loudly with the offending document's kind and name rather than
// silently skipping it.
func TestSamplesApplyCleanly(t *testing.T) {
	files, err := filepath.Glob(samplesGlob)
	if err != nil {
		t.Fatalf("glob %s: %v", samplesGlob, err)
	}
	if len(files) == 0 {
		t.Fatalf("no sample files matched %s", samplesGlob)
	}
	sort.Strings(files)

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}

			dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
			for {
				obj := &unstructured.Unstructured{}
				if err := dec.Decode(obj); err != nil {
					if err == io.EOF { //nolint:errorlint // apimachinery's decoder returns the bare sentinel
						break
					}
					t.Fatalf("%s: decode document: %v", path, err)
				}
				if len(obj.Object) == 0 {
					continue // blank or comment-only document
				}

				if err := k8sClient.Create(testCtx, obj); err != nil {
					t.Fatalf("%s: create %s %q: %v", path, obj.GetKind(), obj.GetName(), err)
				}
			}
		})
	}
}
