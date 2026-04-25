// Package controller_test -- label-based PackOperationResult lookup tests (T-16).
package controller_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientevents "k8s.io/client-go/tools/events"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	seamv1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
	"github.com/ontai-dev/wrapper/internal/controller"
)

func makePOR(name, namespace, packExecutionRef string, revision int64, status seamv1alpha1.PackResultStatus) *seamv1alpha1.PackOperationResult {
	return &seamv1alpha1.PackOperationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"ontai.dev/pack-execution": packExecutionRef,
			},
		},
		Spec: seamv1alpha1.PackOperationResultSpec{
			Revision:   revision,
			Capability: "pack-deploy",
			Status:     status,
		},
	}
}

// TestFindLatestPOR_SingleResult verifies that the reconciler finds a labelled
// POR and advances PackExecution to Succeeded.
func TestFindLatestPOR_SingleResult(t *testing.T) {
	const (
		peName  = "pe-lookup-single"
		cpName  = "my-pack-single"
		version = "v1.0.0"
		cluster = "cluster-lookup"
		profile = "profile-lookup"
	)

	fakeClient, pe := allGatesSetup(t, peName, cpName, version, cluster, profile)
	ctx := context.Background()

	succeededJob := newJob(packDeployJobName(peName), "infra-system", 1, 0, pe)
	por := makePOR("pack-deploy-result-"+peName+"-r1", "infra-system", peName, 1, seamv1alpha1.PackResultSucceeded)
	if err := fakeClient.Create(ctx, succeededJob); err != nil {
		t.Fatalf("create Job: %v", err)
	}
	if err := fakeClient.Create(ctx, por); err != nil {
		t.Fatalf("create POR: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   buildTestScheme(t),
		Recorder: clientevents.NewFakeRecorder(32),
		RBACChecker: rbacAllowedStub,
	}
	reconcilePackExecution(t, r, peName, "infra-system")

	updated := &seamv1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(ctx, ctrlclient.ObjectKey{Name: peName, Namespace: "infra-system"}, updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}
	if updated.Status.OperationResultRef != "pack-deploy-result-"+peName+"-r1" {
		t.Errorf("OperationResultRef=%q, want pack-deploy-result-%s-r1", updated.Status.OperationResultRef, peName)
	}
}

// TestFindLatestPOR_MultipleRevisions verifies that when multiple revisions
// exist (e.g., due to a race), the reconciler picks the one with the highest
// Revision value.
func TestFindLatestPOR_MultipleRevisions(t *testing.T) {
	const (
		peName  = "pe-lookup-multi"
		cpName  = "my-pack-multi"
		version = "v1.0.0"
		cluster = "cluster-multi"
		profile = "profile-multi"
	)

	fakeClient, pe := allGatesSetup(t, peName, cpName, version, cluster, profile)
	ctx := context.Background()

	succeededJob := newJob(packDeployJobName(peName), "infra-system", 1, 0, pe)
	por1 := makePOR("pack-deploy-result-"+peName+"-r1", "infra-system", peName, 1, seamv1alpha1.PackResultFailed)
	por2 := makePOR("pack-deploy-result-"+peName+"-r2", "infra-system", peName, 2, seamv1alpha1.PackResultSucceeded)
	if err := fakeClient.Create(ctx, succeededJob); err != nil {
		t.Fatalf("create Job: %v", err)
	}
	for _, p := range []*seamv1alpha1.PackOperationResult{por1, por2} {
		if err := fakeClient.Create(ctx, p); err != nil {
			t.Fatalf("create POR %q: %v", p.Name, err)
		}
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   buildTestScheme(t),
		Recorder: clientevents.NewFakeRecorder(32),
		RBACChecker: rbacAllowedStub,
	}
	reconcilePackExecution(t, r, peName, "infra-system")

	updated := &seamv1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(ctx, ctrlclient.ObjectKey{Name: peName, Namespace: "infra-system"}, updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}
	if updated.Status.OperationResultRef != "pack-deploy-result-"+peName+"-r2" {
		t.Errorf("OperationResultRef=%q, want highest revision r2", updated.Status.OperationResultRef)
	}
}

// TestFindLatestPOR_NoPOR verifies that when no POR exists yet, the reconciler
// requeues and does not advance to Succeeded.
func TestFindLatestPOR_NoPOR(t *testing.T) {
	const (
		peName  = "pe-lookup-nopor"
		cpName  = "my-pack-nopor"
		version = "v1.0.0"
		cluster = "cluster-nopor"
		profile = "profile-nopor"
	)

	fakeClient, pe := allGatesSetup(t, peName, cpName, version, cluster, profile)
	ctx := context.Background()

	succeededJob := newJob(packDeployJobName(peName), "infra-system", 1, 0, pe)
	if err := fakeClient.Create(ctx, succeededJob); err != nil {
		t.Fatalf("create Job: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   buildTestScheme(t),
		Recorder: clientevents.NewFakeRecorder(32),
		RBACChecker: rbacAllowedStub,
	}
	result := reconcilePackExecution(t, r, peName, "infra-system")

	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter when POR not yet written; got no requeue")
	}

	updated := &seamv1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(ctx, ctrlclient.ObjectKey{Name: peName, Namespace: "infra-system"}, updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}
	if updated.Status.OperationResultRef != "" {
		t.Errorf("OperationResultRef=%q, want empty when POR not yet written", updated.Status.OperationResultRef)
	}
}
