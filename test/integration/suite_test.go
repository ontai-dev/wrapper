// Package integration_test contains envtest integration tests for the wrapper
// operator reconcilers.
//
// These tests use envtest to spin up a real API server and etcd, verifying
// reconciler behavior and Kubernetes GC cascade delete semantics that fake
// clients cannot validate.
//
// envtest binaries are required:
//
//	setup-envtest use --bin-dir /tmp/envtest-bins
//	export KUBEBUILDER_ASSETS=/tmp/envtest-bins/k8s/1.35.0-linux-amd64
package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	seamcorev1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
	"github.com/ontai-dev/wrapper/internal/controller"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	scheme    = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS not set; skipping integration suite (requires KUBEBUILDER_ASSETS and WRAPPER-ENVTEST-BINARIES closed)")
		os.Exit(0)
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(seamcorev1alpha1.AddToScheme(scheme))

	crdPath := filepath.Join("..", "..", "config", "crd")

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		panic("failed to start envtest: " + err.Error())
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic("failed to create k8s client: " + err.Error())
	}

	// Ensure seam-system namespace exists for any CR that uses it.
	seamNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "seam-system"}}
	if err := k8sClient.Create(context.Background(), seamNS); err != nil {
		panic("create seam-system namespace: " + err.Error())
	}

	// Start the controller manager with all wrapper reconcilers.
	// Metrics server disabled to avoid port conflicts in parallel test runs.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		panic("failed to create manager: " + err.Error())
	}
	if err := (&controller.ClusterPackReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("clusterpack-controller"),
	}).SetupWithManager(mgr); err != nil {
		panic("failed to register ClusterPackReconciler: " + err.Error())
	}
	if err := (&controller.PackInstanceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("packinstance-controller"),
	}).SetupWithManager(mgr); err != nil {
		panic("failed to register PackInstanceReconciler: " + err.Error())
	}
	// Note: PackExecution reconciler is intentionally not registered here.
	// PackExecution requires ConductorReady from a platform TalosCluster CR
	// via unstructured read, which is not available in wrapper envtest. Gate
	// behavior is covered by wrapper unit tests that use fake clients.

	goCtx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(goCtx); err != nil {
			panic("manager exited: " + err.Error())
		}
	}()

	code := m.Run()
	cancel()
	_ = testEnv.Stop()
	os.Exit(code)
}

// poll waits up to timeout for condition to return true, checking every 200ms.
func poll(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}
