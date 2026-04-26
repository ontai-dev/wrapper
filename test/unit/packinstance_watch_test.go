package unit_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientevents "k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seamcorev1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
	"github.com/ontai-dev/wrapper/internal/controller"
)

func newWatchTestReconciler(t *testing.T, objs ...client.Object) *controller.ClusterPackReconciler {
	t.Helper()
	s := newClusterPackScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &controller.ClusterPackReconciler{
		Client:   fc,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}
}

// TestMapPackInstanceToClusterPack_ReturnsRequestAndDeletesPackExecution verifies
// that a PackInstance in a seam-tenant namespace enqueues its ClusterPack and
// deletes the corresponding PackExecution (WS2 cascade).
func TestMapPackInstanceToClusterPack_ReturnsRequestAndDeletesPackExecution(t *testing.T) {
	const (
		ns          = "seam-tenant-ccs-dev"
		cpName      = "nginx"
		peName      = "nginx-ccs-dev"
	)
	pi := newPackInstance("nginx-instance", ns, cpName, "ccs-dev")
	pe := newPackExecution(peName, ns, cpName, "v1.0.0", "ccs-dev", "")

	r := newWatchTestReconciler(t, pi, pe)
	reqs := r.MapPackInstanceToClusterPack(context.Background(), pi)

	if len(reqs) != 1 {
		t.Fatalf("expected 1 reconcile request, got %d", len(reqs))
	}
	if reqs[0].NamespacedName != (types.NamespacedName{Name: cpName, Namespace: ns}) {
		t.Errorf("expected request for %s/%s, got %v", ns, cpName, reqs[0].NamespacedName)
	}

	// PackExecution must have been deleted by the mapping function (WS2).
	fetched := &seamcorev1alpha1.InfrastructurePackExecution{}
	err := r.Client.Get(context.Background(), client.ObjectKey{Name: peName, Namespace: ns}, fetched)
	if err == nil {
		t.Errorf("expected PackExecution %s/%s to be deleted, but it still exists", ns, peName)
	}
}

// TestMapPackInstanceToClusterPack_EmptyClusterPackRefReturnsNil verifies that a
// PackInstance with no ClusterPackRef is silently ignored.
func TestMapPackInstanceToClusterPack_EmptyClusterPackRefReturnsNil(t *testing.T) {
	pi := newPackInstance("orphan-instance", "seam-tenant-ccs-dev", "", "ccs-dev")
	r := newWatchTestReconciler(t, pi)

	reqs := r.MapPackInstanceToClusterPack(context.Background(), pi)
	if len(reqs) != 0 {
		t.Errorf("expected 0 requests for empty ClusterPackRef, got %d", len(reqs))
	}
}

// TestMapPackInstanceToClusterPack_NonTenantNamespaceNoPackExecutionDelete verifies
// that a PackInstance outside a seam-tenant namespace still enqueues the ClusterPack
// but does not attempt to delete a PackExecution.
func TestMapPackInstanceToClusterPack_NonTenantNamespaceNoPackExecutionDelete(t *testing.T) {
	const (
		ns     = "infra-system"
		cpName = "nginx"
	)
	pi := newPackInstance("nginx-instance", ns, cpName, "")
	pe := newPackExecution("nginx-infra-system", ns, cpName, "v1.0.0", "", "")
	r := newWatchTestReconciler(t, pi, pe)

	reqs := r.MapPackInstanceToClusterPack(context.Background(), pi)

	if len(reqs) != 1 {
		t.Fatalf("expected 1 reconcile request, got %d", len(reqs))
	}
	if reqs[0].NamespacedName != (types.NamespacedName{Name: cpName, Namespace: ns}) {
		t.Errorf("expected request for %s/%s, got %v", ns, cpName, reqs[0].NamespacedName)
	}

	// PackExecution must NOT have been deleted -- namespace is not seam-tenant-*.
	fetched := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := r.Client.Get(context.Background(), client.ObjectKey{Name: "nginx-infra-system", Namespace: ns}, fetched); err != nil {
		t.Errorf("PackExecution should survive for non-seam-tenant namespace, got error: %v", err)
	}
}

// TestMapPackInstanceToClusterPack_WrongTypeReturnsNil verifies that passing an
// object that is not an InfrastructurePackInstance results in no enqueue.
func TestMapPackInstanceToClusterPack_WrongTypeReturnsNil(t *testing.T) {
	r := newWatchTestReconciler(t)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "default"}}

	reqs := r.MapPackInstanceToClusterPack(context.Background(), cm)
	if len(reqs) != 0 {
		t.Errorf("expected 0 requests for wrong object type, got %d", len(reqs))
	}
}
