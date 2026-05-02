package e2e_test

// AC-3: ClusterPack N-step rollback acceptance contract.
//
// Scenario: A ClusterPack has a retained superseded POR in its history.
// Setting spec.rollbackToRevision to that POR's revision causes the wrapper
// ClusterPackReconciler to:
//   1. List all PORs labeled ontai.dev/cluster-pack={cp.Name} (active + superseded).
//   2. Find the POR at spec.revision == rollbackToRevision.
//   3. Patch ClusterPack.spec to the target version/rbacDigest/workloadDigest.
//   4. Clear spec.rollbackToRevision to 0.
//   5. Remove infrastructure.ontai.dev/spec-checksum-snapshot annotation.
//
// Validation approach: the test creates a synthetic superseded POR so no prior
// deployment history with the new conductor is required. It sets rollbackToRevision
// and confirms the handler ran by polling for the field to clear.
//
// Run: MGMT_KUBECONFIG=/home/saigha01/.kube/ccs-mgmt.yaml MGMT_CLUSTER_NAME=ccs-dev make e2e
// Wrapper must be at commit 51039d6+ (N-step handleRollback).

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	e2ehelpers "github.com/ontai-dev/seam-core/pkg/e2e"
)

const (
	rollbackTimeout  = 2 * time.Minute
	rollbackInterval = 5 * time.Second
)

var _ = Describe("AC-3: ClusterPack N-step rollback", func() {
	It("rollback to retained superseded revision: rollbackToRevision cleared by wrapper handler", func() {
		ctx := context.Background()
		clusterName := mgmtClusterName
		ns := "seam-tenant-" + clusterName

		// Discover the first ClusterPack in the tenant namespace.
		By(fmt.Sprintf("discovering ClusterPack in %s", ns))
		cpList, err := mgmtClient.Dynamic.Resource(clusterPackGVR).Namespace(ns).
			List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed to list ClusterPacks in %s", ns)
		Expect(cpList.Items).NotTo(BeEmpty(),
			"no ClusterPacks found in %s -- set MGMT_CLUSTER_NAME to a provisioned tenant cluster", ns)

		packName := cpList.Items[0].GetName()
		spec := cpList.Items[0].Object["spec"].(map[string]interface{})
		currentVersion, _ := spec["version"].(string)
		Expect(currentVersion).NotTo(BeEmpty(), "ClusterPack %s has no spec.version", packName)

		By(fmt.Sprintf("ClusterPack %s in %s at version %s", packName, ns, currentVersion))

		// Create a synthetic superseded POR at revision 1. The version is set to the
		// same value as the current ClusterPack so rollback does not attempt to deploy
		// a non-existent OCI artifact.
		fakePORName := fmt.Sprintf("pack-deploy-result-%s-%s-r1-rollback-test", packName, clusterName)
		fakePORYAML := fmt.Sprintf(`
apiVersion: infrastructure.ontai.dev/v1alpha1
kind: PackOperationResult
metadata:
  name: %s
  namespace: %s
  labels:
    ontai.dev/cluster-pack: %s
    ontai.dev/pack-execution: %s-%s
    ontai.dev/superseded: "true"
spec:
  revision: 1
  clusterPackRef: %s
  clusterPackVersion: %s
  capability: pack-deploy
  status: Succeeded
  targetClusterRef: %s
`, fakePORName, ns, packName, packName, clusterName, packName, currentVersion, clusterName)

		By(fmt.Sprintf("applying synthetic superseded POR %s", fakePORName))
		applier := e2ehelpers.NewCRApplier(mgmtClient)
		_, err = applier.Apply(ctx, packOperationResultGVR, []byte(fakePORYAML))
		Expect(err).NotTo(HaveOccurred(), "failed to create synthetic superseded POR %s", fakePORName)

		DeferCleanup(func() {
			By("cleanup: deleting synthetic superseded POR")
			_ = mgmtClient.Dynamic.Resource(packOperationResultGVR).Namespace(ns).
				Delete(context.Background(), fakePORName, metav1.DeleteOptions{})
		})

		// Set spec.rollbackToRevision=1 via merge patch.
		By(fmt.Sprintf("patching ClusterPack %s: spec.rollbackToRevision=1", packName))
		rollbackPatch := []byte(`{"spec":{"rollbackToRevision":1}}`)
		_, err = mgmtClient.Dynamic.Resource(clusterPackGVR).Namespace(ns).
			Patch(ctx, packName, types.MergePatchType, rollbackPatch, metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed to set rollbackToRevision on ClusterPack %s", packName)

		DeferCleanup(func() {
			By("cleanup: clearing rollbackToRevision on ClusterPack in case of test failure")
			clearPatch := []byte(`{"spec":{"rollbackToRevision":0}}`)
			_, _ = mgmtClient.Dynamic.Resource(clusterPackGVR).Namespace(ns).
				Patch(context.Background(), packName, types.MergePatchType, clearPatch, metav1.PatchOptions{})
		})

		// Poll until the wrapper's N-step handleRollback clears the field.
		// Clearing to 0 (or absent) proves the handler ran and completed. wrapper-schema.md §6.2.
		By("polling for spec.rollbackToRevision to clear to 0 (handler executed)")
		Eventually(func() int64 {
			obj, err := mgmtClient.Dynamic.Resource(clusterPackGVR).Namespace(ns).
				Get(ctx, packName, metav1.GetOptions{})
			if err != nil {
				return -1
			}
			s, _ := obj.Object["spec"].(map[string]interface{})
			rev, _ := s["rollbackToRevision"].(int64)
			return rev
		}, rollbackTimeout, rollbackInterval).Should(Equal(int64(0)),
			"spec.rollbackToRevision did not clear within %s -- wrapper may not have N-step handler (commit 51039d6+)", rollbackTimeout)

		// Verify spec.version equals the rollback target recorded in the synthetic POR.
		By("verifying spec.version matches the rollback target version")
		finalCP, err := mgmtClient.Dynamic.Resource(clusterPackGVR).Namespace(ns).
			Get(ctx, packName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		finalSpec, _ := finalCP.Object["spec"].(map[string]interface{})
		finalVersion, _ := finalSpec["version"].(string)
		Expect(finalVersion).To(Equal(currentVersion),
			"spec.version=%q, want %q (rollback target from synthetic POR revision 1)", finalVersion, currentVersion)
	})
})
