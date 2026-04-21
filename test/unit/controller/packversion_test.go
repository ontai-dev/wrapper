// Package controller_test tests for the upgradeDirection logic in
// PackExecutionReconciler. Tests are driven through full reconcile cycles
// to verify that PackInstance.Status.UpgradeDirection is set correctly
// based on the version comparison between existing and new ClusterPack versions.
package controller_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientevents "k8s.io/client-go/tools/events"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	seamv1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
	infrav1alpha1 "github.com/ontai-dev/wrapper/api/v1alpha1"
	controller "github.com/ontai-dev/wrapper/internal/controller"
)

// deployPack runs a reconcile cycle with the given ClusterPack version and
// an optional pre-existing PackInstance version. Returns the resulting
// PackInstance.Status.UpgradeDirection.
func deployPack(t *testing.T, cpVersion, existingPIVersion string) string {
	t.Helper()
	ctx := context.Background()
	const (
		peName     = "pe-ver-test"
		cpName     = "mypack"
		clusterRef = "cver"
		profileRef = "profile-ver"
		peNS       = "infra-system"
	)

	fakeClient, _ := allGatesSetup(t, peName, cpName, cpVersion, clusterRef, profileRef)

	// If a previous PackInstance version is given, pre-create one to simulate
	// an existing deployment (basePackName supersession path).
	if existingPIVersion != "" {
		piName := cpName + "-" + clusterRef
		piNS := "seam-tenant-" + clusterRef
		existing := &infrav1alpha1.PackInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      piName,
				Namespace: piNS,
				UID:       types.UID("uid-pi-existing"),
			},
			Spec: infrav1alpha1.PackInstanceSpec{
				ClusterPackRef:   cpName,
				Version:          existingPIVersion,
				TargetClusterRef: clusterRef,
			},
		}
		if err := fakeClient.Create(ctx, existing); err != nil {
			t.Fatalf("pre-create PackInstance: %v", err)
		}
	}

	succeededJob := newJob(packDeployJobName(peName), peNS, 1, 0)
	por := newOperationResultPOR(peName, peNS)
	if err := fakeClient.Create(ctx, succeededJob); err != nil {
		t.Fatalf("create Job: %v", err)
	}
	if err := fakeClient.Create(ctx, por); err != nil {
		t.Fatalf("create PackOperationResult: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   buildTestScheme(t),
		Recorder: clientevents.NewFakeRecorder(32),
	}
	reconcilePackExecution(t, r, peName, peNS)

	pi := &infrav1alpha1.PackInstance{}
	piKey := ctrlclient.ObjectKey{
		Name:      cpName + "-" + clusterRef,
		Namespace: "seam-tenant-" + clusterRef,
	}
	if err := fakeClient.Get(ctx, piKey, pi); err != nil {
		t.Fatalf("get PackInstance after reconcile: %v", err)
	}
	return pi.Status.UpgradeDirection
}

// TestUpgradeDirection_Initial verifies first deployment → Initial.
func TestUpgradeDirection_Initial(t *testing.T) {
	got := deployPack(t, "v4.10.0-r1", "")
	if got != string(seamv1alpha1.PackUpgradeDirectionInitial) {
		t.Errorf("upgradeDirection=%q, want Initial", got)
	}
}

// TestUpgradeDirection_Upgrade verifies newer version → Upgrade.
func TestUpgradeDirection_Upgrade(t *testing.T) {
	got := deployPack(t, "v4.10.0-r1", "v4.9.0-r1")
	if got != string(seamv1alpha1.PackUpgradeDirectionUpgrade) {
		t.Errorf("upgradeDirection=%q, want Upgrade", got)
	}
}

// TestUpgradeDirection_Rollback verifies older version → Rollback.
func TestUpgradeDirection_Rollback(t *testing.T) {
	got := deployPack(t, "v4.9.0-r1", "v4.10.0-r1")
	if got != string(seamv1alpha1.PackUpgradeDirectionRollback) {
		t.Errorf("upgradeDirection=%q, want Rollback", got)
	}
}

// TestUpgradeDirection_Redeploy verifies same version → Redeploy.
func TestUpgradeDirection_Redeploy(t *testing.T) {
	got := deployPack(t, "v4.10.0-r1", "v4.10.0-r1")
	if got != string(seamv1alpha1.PackUpgradeDirectionRedeploy) {
		t.Errorf("upgradeDirection=%q, want Redeploy", got)
	}
}

// TestUpgradeDirection_RevisionBump verifies revision bump (same semver) → Upgrade.
func TestUpgradeDirection_RevisionBump(t *testing.T) {
	got := deployPack(t, "v4.9.0-r2", "v4.9.0-r1")
	if got != string(seamv1alpha1.PackUpgradeDirectionUpgrade) {
		t.Errorf("upgradeDirection=%q, want Upgrade (revision bump)", got)
	}
}
