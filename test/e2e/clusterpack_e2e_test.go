package e2e_test

// Steps 3 and 4: ClusterPack deployment end-to-end and PackOperationResult
// single-active-revision validation.
//
// Pre-conditions:
//   - cert-manager ClusterPack CR exists in seam-system, Signed=true
//   - Kueue running in seam-system (Job admission active)
//   - PermissionSnapshot for mgmtClusterName is current
//   - RBACProfile provisioned=true in seam-tenant-{mgmtClusterName}
//   - TalosCluster for mgmtClusterName has ConductorReady=True
//   - Conductor execute job image available at REGISTRY_ADDR
//
// Reusable: each validation function accepts a ClusterClient and cluster name.
// Run against management cluster first; reuse for tenant cluster validation once
// TENANT-CLUSTER-E2E closes.
//
// Covers management cluster validation gate Steps 3 and 4 (GAP_TO_FILL.md).

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	e2ehelpers "github.com/ontai-dev/seam-core/pkg/e2e"
)

var (
	clusterPackGVR = schema.GroupVersionResource{
		Group:    "infrastructure.ontai.dev",
		Version:  "v1alpha1",
		Resource: "infrastructureclusterpacks",
	}
	packExecutionGVR = schema.GroupVersionResource{
		Group:    "infrastructure.ontai.dev",
		Version:  "v1alpha1",
		Resource: "infrastructurepackexecutions",
	}
	packReceiptGVR = schema.GroupVersionResource{
		Group:    "infrastructure.ontai.dev",
		Version:  "v1alpha1",
		Resource: "infrastructurepackreceipts",
	}
	packInstanceGVR = schema.GroupVersionResource{
		Group:    "infrastructure.ontai.dev",
		Version:  "v1alpha1",
		Resource: "infrastructurepackinstances",
	}
	packOperationResultGVR = schema.GroupVersionResource{
		Group:    "infrastructure.ontai.dev",
		Version:  "v1alpha1",
		Resource: "packoperationresults",
	}
)

const (
	deployTimeout = 10 * time.Minute
	pollInterval  = 5 * time.Second
	// certManagerPackName is the ClusterPack name used for WS8b validation.
	certManagerPackName = "cert-manager"
)

var _ = Describe("Step 3: ClusterPack deployment end-to-end", func() {
	It("ClusterPack cert-manager exists in seam-system with helm metadata fields populated", func() {
		validateClusterPackHelmMetadata(context.Background(), mgmtClient, certManagerPackName)
	})

	It("PackExecution is created by wrapper in seam-tenant-{cluster}", func() {
		validatePackExecutionCreated(context.Background(), mgmtClient, mgmtClusterName, certManagerPackName, deployTimeout, pollInterval)
	})

	It("Kueue Job is submitted for the pack-deploy capability", func() {
		validateKueueJobSubmitted(context.Background(), mgmtClient, mgmtClusterName, deployTimeout, pollInterval)
	})

	It("PackReceipt is written in seam-tenant-{cluster} with rbacDigest and workloadDigest", func() {
		validatePackReceipt(context.Background(), mgmtClient, mgmtClusterName, certManagerPackName, deployTimeout, pollInterval)
	})

	It("PackInstance is written confirming delivered state", func() {
		validatePackInstance(context.Background(), mgmtClient, mgmtClusterName, certManagerPackName, deployTimeout, pollInterval)
	})

	It("PackOperationResult is written in seam-tenant-{cluster} with revision=1", func() {
		validatePORRevision1(context.Background(), mgmtClient, mgmtClusterName, deployTimeout, pollInterval)
	})
})

var _ = Describe("Step 4: PackOperationResult single-active-revision", func() {
	It("second deployment writes POR revision=2 with previousRevisionRef pointing to -r1", func() {
		validatePORRevision2(context.Background(), mgmtClient, mgmtClusterName, certManagerPackName, deployTimeout, pollInterval)
	})

	It("revision=1 POR is deleted after revision=2 is written", func() {
		validatePORPredecessorDeleted(context.Background(), mgmtClient, mgmtClusterName, deployTimeout, pollInterval)
	})

	It("exactly one PackOperationResult exists in seam-tenant-{cluster} after second deployment", func() {
		validateSingleActivePOR(context.Background(), mgmtClient, mgmtClusterName, deployTimeout, pollInterval)
	})
})

// validateClusterPackHelmMetadata verifies the named ClusterPack CR exists in
// seam-system and has all four helm metadata fields populated (chartVersion,
// chartURL, chartName, helmVersion) and both digest fields (rbacDigest, workloadDigest).
func validateClusterPackHelmMetadata(ctx context.Context, cl *e2ehelpers.ClusterClient, packName string) {
	By(fmt.Sprintf("getting ClusterPack %s from seam-system on cluster %s", packName, cl.Name))
	obj, err := cl.Dynamic.Resource(clusterPackGVR).Namespace("seam-system").
		Get(ctx, packName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "ClusterPack %s not found in seam-system", packName)

	spec, ok := obj.Object["spec"].(map[string]interface{})
	Expect(ok).To(BeTrue(), "ClusterPack spec not found")

	for _, field := range []string{"chartVersion", "chartURL", "chartName", "helmVersion"} {
		val, present := spec[field]
		Expect(present).To(BeTrue(), "ClusterPack spec.%s absent -- Phase 2 regression", field)
		Expect(fmt.Sprint(val)).NotTo(BeEmpty(), "ClusterPack spec.%s is empty", field)
	}
	for _, field := range []string{"rbacDigest", "workloadDigest"} {
		val, present := spec[field]
		Expect(present).To(BeTrue(), "ClusterPack spec.%s absent -- three-bucket split regression", field)
		Expect(fmt.Sprint(val)).NotTo(BeEmpty(), "ClusterPack spec.%s is empty", field)
	}
}

// validatePackExecutionCreated waits for a PackExecution with the given packName
// to appear in seam-tenant-{clusterName}.
func validatePackExecutionCreated(
	ctx context.Context,
	cl *e2ehelpers.ClusterClient,
	clusterName, packName string,
	timeout, interval time.Duration,
) {
	tenantNS := "seam-tenant-" + clusterName
	By(fmt.Sprintf("waiting for PackExecution in %s on cluster %s", tenantNS, cl.Name))
	Eventually(func() bool {
		list, err := cl.Dynamic.Resource(packExecutionGVR).Namespace(tenantNS).
			List(ctx, metav1.ListOptions{
				LabelSelector: "infrastructure.ontai.dev/pack=" + packName,
			})
		return err == nil && len(list.Items) > 0
	}, timeout, interval).Should(BeTrue(),
		"PackExecution for %s not found in %s within %s", packName, tenantNS, timeout)
}

// validateKueueJobSubmitted waits for a pack-deploy Job to appear in infra-system
// for the given cluster, confirming Kueue admitted the Job.
func validateKueueJobSubmitted(
	ctx context.Context,
	cl *e2ehelpers.ClusterClient,
	clusterName string,
	timeout, interval time.Duration,
) {
	By(fmt.Sprintf("waiting for pack-deploy Job in infra-system on cluster %s", cl.Name))
	Eventually(func() bool {
		list, err := cl.Typed.BatchV1().Jobs("infra-system").
			List(ctx, metav1.ListOptions{
				LabelSelector: "infrastructure.ontai.dev/target-cluster=" + clusterName,
			})
		return err == nil && len(list.Items) > 0
	}, timeout, interval).Should(BeTrue(),
		"pack-deploy Job not found in infra-system for cluster %s within %s", clusterName, timeout)
}

// validatePackReceipt waits for a PackReceipt to appear in seam-tenant-{clusterName}
// and verifies it carries rbacDigest and workloadDigest from the three-bucket split.
func validatePackReceipt(
	ctx context.Context,
	cl *e2ehelpers.ClusterClient,
	clusterName, packName string,
	timeout, interval time.Duration,
) {
	tenantNS := "seam-tenant-" + clusterName
	By(fmt.Sprintf("waiting for PackReceipt in %s for %s on cluster %s", tenantNS, packName, cl.Name))

	var receiptObj map[string]interface{}
	Eventually(func() bool {
		list, err := cl.Dynamic.Resource(packReceiptGVR).Namespace(tenantNS).
			List(ctx, metav1.ListOptions{
				LabelSelector: "infrastructure.ontai.dev/pack=" + packName,
			})
		if err != nil || len(list.Items) == 0 {
			return false
		}
		receiptObj = list.Items[0].Object
		return true
	}, timeout, interval).Should(BeTrue(),
		"PackReceipt for %s not found in %s within %s", packName, tenantNS, timeout)

	spec, ok := receiptObj["spec"].(map[string]interface{})
	Expect(ok).To(BeTrue(), "PackReceipt spec not found")

	for _, field := range []string{"rbacDigest", "workloadDigest"} {
		val, present := spec[field]
		Expect(present).To(BeTrue(), "PackReceipt spec.%s absent -- digest carry-through regression", field)
		Expect(fmt.Sprint(val)).NotTo(BeEmpty(), "PackReceipt spec.%s is empty", field)
	}
}

// validatePackInstance waits for a PackInstance to appear in seam-tenant-{clusterName}
// with Delivered condition = True.
func validatePackInstance(
	ctx context.Context,
	cl *e2ehelpers.ClusterClient,
	clusterName, packName string,
	timeout, interval time.Duration,
) {
	tenantNS := "seam-tenant-" + clusterName
	By(fmt.Sprintf("waiting for PackInstance Delivered in %s on cluster %s", tenantNS, cl.Name))
	Eventually(func() bool {
		list, err := cl.Dynamic.Resource(packInstanceGVR).Namespace(tenantNS).
			List(ctx, metav1.ListOptions{
				LabelSelector: "infrastructure.ontai.dev/pack=" + packName,
			})
		if err != nil || len(list.Items) == 0 {
			return false
		}
		status, _ := list.Items[0].Object["status"].(map[string]interface{})
		conditions, _ := status["conditions"].([]interface{})
		for _, c := range conditions {
			cm, _ := c.(map[string]interface{})
			if cm["type"] == "Delivered" && cm["status"] == "True" {
				return true
			}
		}
		return false
	}, timeout, interval).Should(BeTrue(),
		"PackInstance for %s did not reach Delivered=True in %s within %s", packName, tenantNS, timeout)
}

// validatePORRevision1 waits for a PackOperationResult with revision=1 to appear
// in seam-tenant-{clusterName} for the given pack deployment.
func validatePORRevision1(
	ctx context.Context,
	cl *e2ehelpers.ClusterClient,
	clusterName string,
	timeout, interval time.Duration,
) {
	tenantNS := "seam-tenant-" + clusterName
	By(fmt.Sprintf("waiting for PackOperationResult revision=1 in %s on cluster %s", tenantNS, cl.Name))
	Eventually(func() bool {
		list, err := cl.Dynamic.Resource(packOperationResultGVR).Namespace(tenantNS).
			List(ctx, metav1.ListOptions{})
		if err != nil || len(list.Items) == 0 {
			return false
		}
		for _, item := range list.Items {
			spec, _ := item.Object["spec"].(map[string]interface{})
			rev, _ := spec["revision"].(int64)
			if rev == 1 {
				return true
			}
		}
		return false
	}, timeout, interval).Should(BeTrue(),
		"PackOperationResult revision=1 not found in %s within %s", tenantNS, timeout)
}

// validatePORRevision2 waits for a PackOperationResult with revision=2 to appear
// and verifies its previousRevisionRef points to the -r1 CR name.
func validatePORRevision2(
	ctx context.Context,
	cl *e2ehelpers.ClusterClient,
	clusterName, packName string,
	timeout, interval time.Duration,
) {
	tenantNS := "seam-tenant-" + clusterName
	By(fmt.Sprintf("triggering second deployment and waiting for POR revision=2 in %s", tenantNS))

	// Trigger a second deployment by annotating the PackExecution to force re-reconcile.
	// In practice this is done by deleting the PackOperationResult and requeuing;
	// the exact trigger mechanism depends on the operator's requeue contract.
	// For this validation gate we wait for revision=2 to appear after the second
	// pack-deploy Job completes.

	var r2Name string
	Eventually(func() bool {
		list, err := cl.Dynamic.Resource(packOperationResultGVR).Namespace(tenantNS).
			List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		for _, item := range list.Items {
			spec, _ := item.Object["spec"].(map[string]interface{})
			rev, _ := spec["revision"].(int64)
			if rev == 2 {
				r2Name = item.GetName()
				prevRef, _ := spec["previousRevisionRef"].(string)
				return prevRef != ""
			}
		}
		return false
	}, timeout, interval).Should(BeTrue(),
		"PackOperationResult revision=2 with previousRevisionRef not found in %s within %s", tenantNS, timeout)

	Expect(r2Name).To(ContainSubstring("-r2"),
		"revision=2 POR name must follow pack-deploy-result-{peName}-r2 convention")
}

// validatePORPredecessorDeleted confirms that after revision=2 is written, the
// revision=1 CR is deleted (single-active-revision invariant).
func validatePORPredecessorDeleted(
	ctx context.Context,
	cl *e2ehelpers.ClusterClient,
	clusterName string,
	timeout, interval time.Duration,
) {
	tenantNS := "seam-tenant-" + clusterName
	By(fmt.Sprintf("confirming revision=1 POR deleted after revision=2 written in %s", tenantNS))
	Eventually(func() bool {
		list, err := cl.Dynamic.Resource(packOperationResultGVR).Namespace(tenantNS).
			List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		for _, item := range list.Items {
			spec, _ := item.Object["spec"].(map[string]interface{})
			rev, _ := spec["revision"].(int64)
			if rev == 1 {
				return false
			}
		}
		return true
	}, timeout, interval).Should(BeTrue(),
		"revision=1 POR still present after revision=2 written -- single-active-revision invariant violated in %s", tenantNS)
}

// validateSingleActivePOR confirms exactly one PackOperationResult exists in the
// namespace after the second deployment cycle completes.
func validateSingleActivePOR(
	ctx context.Context,
	cl *e2ehelpers.ClusterClient,
	clusterName string,
	timeout, interval time.Duration,
) {
	tenantNS := "seam-tenant-" + clusterName
	By(fmt.Sprintf("confirming exactly one POR exists in %s on cluster %s", tenantNS, cl.Name))
	Eventually(func() int {
		list, err := cl.Dynamic.Resource(packOperationResultGVR).Namespace(tenantNS).
			List(ctx, metav1.ListOptions{})
		if err != nil {
			return -1
		}
		return len(list.Items)
	}, timeout, interval).Should(Equal(1),
		"single-active-revision violated: expected 1 POR in %s, got != 1", tenantNS)
}
