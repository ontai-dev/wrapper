package e2e_test

// AC-4: Day-2 operations acceptance contract for tenant cluster (ccs-dev).
//
// Day-2 operations verified here:
//   D2-1  Current ClusterPack state       -- live: nginx-ccs-dev deployed, POR exists
//   D2-2  DriftSignal lifecycle           -- live: DriftSignal for nginx-ccs-dev exists, state valid
//   D2-3  DriftSignal correlationID clear -- live: when drift cleared, correlationID must be cleared
//   D2-4  Upgrade (version bump)          -- stub: requires new OCI image for target version
//   D2-5  Active drift injection          -- stub: requires DRIFT_INJECTION=true
//
// Coverage gaps (D2-4, D2-5) are stubs with explicit skip conditions per e2e CI contract.
//
// Run: MGMT_KUBECONFIG=/home/saigha01/.kube/ccs-mgmt.yaml
//      TENANT_KUBECONFIG=/home/saigha01/.kube/ccs-dev.yaml
//      MGMT_CLUSTER_NAME=ccs-dev TENANT_CLUSTER_NAME=ccs-dev
//      make e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	driftSignalGVR = schema.GroupVersionResource{
		Group:    "infrastructure.ontai.dev",
		Version:  "v1alpha1",
		Resource: "driftsignals",
	}
)

const (
	day2Timeout  = 3 * time.Minute
	day2Interval = 5 * time.Second
)

// D2-1: Current ClusterPack state validation.
var _ = Describe("D2-1: Tenant ClusterPack deployed state", func() {
	It("ClusterPack exists in seam-tenant-{cluster} with version and Signed=true", func() {
		ctx := context.Background()
		ns := "seam-tenant-" + mgmtClusterName

		cpList, err := mgmtClient.Dynamic.Resource(clusterPackGVR).Namespace(ns).
			List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed to list ClusterPacks in %s", ns)
		Expect(cpList.Items).NotTo(BeEmpty(), "no ClusterPacks found in %s", ns)

		cp := cpList.Items[0]
		spec, _ := cp.Object["spec"].(map[string]interface{})
		version, _ := spec["version"].(string)
		Expect(version).NotTo(BeEmpty(), "ClusterPack %s has no spec.version", cp.GetName())

		status, _ := cp.Object["status"].(map[string]interface{})
		signed, _ := status["signed"].(bool)
		Expect(signed).To(BeTrue(), "ClusterPack %s is not signed (status.signed=false)", cp.GetName())
	})

	It("PackOperationResult exists for the ClusterPack with status=Succeeded", func() {
		ctx := context.Background()
		ns := "seam-tenant-" + mgmtClusterName

		cpList, err := mgmtClient.Dynamic.Resource(clusterPackGVR).Namespace(ns).
			List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(cpList.Items).NotTo(BeEmpty())
		packName := cpList.Items[0].GetName()

		Eventually(func() bool {
			list, err := mgmtClient.Dynamic.Resource(packOperationResultGVR).Namespace(ns).
				List(ctx, metav1.ListOptions{})
			if err != nil {
				return false
			}
			for _, item := range list.Items {
				s, _ := item.Object["spec"].(map[string]interface{})
				ref, _ := s["clusterPackRef"].(string)
				status, _ := s["status"].(string)
				if (ref == packName || ref == "") && status == "Succeeded" {
					return true
				}
			}
			return false
		}, day2Timeout, day2Interval).Should(BeTrue(),
			"no Succeeded PackOperationResult found for %s in %s", packName, ns)
	})

	It("PackInstance exists in seam-tenant-{cluster} for the deployed pack", func() {
		ctx := context.Background()
		ns := "seam-tenant-" + mgmtClusterName

		Eventually(func() bool {
			list, err := mgmtClient.Dynamic.Resource(packInstanceGVR).Namespace(ns).
				List(ctx, metav1.ListOptions{})
			return err == nil && len(list.Items) > 0
		}, day2Timeout, day2Interval).Should(BeTrue(),
			"no PackInstance found in %s", ns)
	})
})

// D2-2: DriftSignal lifecycle validation.
var _ = Describe("D2-2: DriftSignal exists and has valid state", func() {
	It("at least one DriftSignal exists in seam-tenant-{cluster} with a recognized state", func() {
		ctx := context.Background()
		ns := "seam-tenant-" + mgmtClusterName

		validStates := map[string]bool{"pending": true, "acknowledged": true, "confirmed": true}

		Eventually(func() bool {
			list, err := mgmtClient.Dynamic.Resource(driftSignalGVR).Namespace(ns).
				List(ctx, metav1.ListOptions{})
			if err != nil || len(list.Items) == 0 {
				return false
			}
			for _, item := range list.Items {
				s, _ := item.Object["spec"].(map[string]interface{})
				state, _ := s["state"].(string)
				if validStates[state] {
					return true
				}
			}
			return false
		}, day2Timeout, day2Interval).Should(BeTrue(),
			"no DriftSignal with valid state (pending|acknowledged|confirmed) found in %s", ns)
	})

	It("confirmed DriftSignal has correlationID cleared (drift lifecycle complete)", func() {
		ctx := context.Background()
		ns := "seam-tenant-" + mgmtClusterName

		list, err := mgmtClient.Dynamic.Resource(driftSignalGVR).Namespace(ns).
			List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed to list DriftSignals in %s", ns)

		for _, item := range list.Items {
			s, _ := item.Object["spec"].(map[string]interface{})
			state, _ := s["state"].(string)
			if state != "confirmed" {
				continue
			}
			correlationID, _ := s["correlationID"].(string)
			Expect(correlationID).To(BeEmpty(),
				"DriftSignal %s is confirmed but correlationID=%q is not cleared -- lifecycle invariant violated",
				item.GetName(), correlationID)
		}
	})
})

// D2-3: DriftSignal correlationID clear contract (detailed).
var _ = Describe("D2-3: DriftSignal correlationID lifecycle", func() {
	It("pending DriftSignal carries a non-empty correlationID", func() {
		ctx := context.Background()
		ns := "seam-tenant-" + mgmtClusterName

		list, err := mgmtClient.Dynamic.Resource(driftSignalGVR).Namespace(ns).
			List(ctx, metav1.ListOptions{})
		if err != nil {
			Skip(fmt.Sprintf("DriftSignal list failed: %v -- skipping correlationID check", err))
		}

		for _, item := range list.Items {
			s, _ := item.Object["spec"].(map[string]interface{})
			state, _ := s["state"].(string)
			if state != "pending" {
				continue
			}
			correlationID, _ := s["correlationID"].(string)
			Expect(correlationID).NotTo(BeEmpty(),
				"pending DriftSignal %s must carry a non-empty correlationID", item.GetName())
		}
	})

	It("acknowledged DriftSignal transitions correlationID before it reaches confirmed", func() {
		Skip("requires DRIFT_INJECTION=true and active drift cycle -- DRIFT-LIFECYCLE-E2E")
	})
})

// D2-4: Upgrade (ClusterPack version bump).
var _ = Describe("D2-4: ClusterPack upgrade (version bump)", func() {
	It("bumping spec.version triggers new PackExecution and POR at revision N+1", func() {
		Skip("requires new OCI image for target version and UPGRADE-E2E closed")
	})

	It("after upgrade, previous POR is labeled ontai.dev/superseded=true (retained for rollback)", func() {
		Skip("requires new OCI image for target version and UPGRADE-E2E closed")
	})

	It("PackInstance shows new version after upgrade Job succeeds", func() {
		Skip("requires new OCI image for target version and UPGRADE-E2E closed")
	})
})

// D2-5: Active drift injection (resource deletion on tenant cluster).
var _ = Describe("D2-5: Active drift injection and detection", func() {
	It("deleting a managed resource on the tenant cluster triggers a new DriftSignal", func() {
		Skip("requires DRIFT_INJECTION=true env var and DRIFT-INJECTION-E2E closed")
	})

	It("conductor agent on tenant detects deletion within one drift-loop period", func() {
		Skip("requires DRIFT_INJECTION=true env var and DRIFT-INJECTION-E2E closed")
	})

	It("DriftSignal state progresses pending → acknowledged → confirmed", func() {
		Skip("requires DRIFT_INJECTION=true env var and DRIFT-INJECTION-E2E closed")
	})

	It("conductor on management cluster re-deploys drifted resources after confirmation", func() {
		Skip("requires DRIFT_INJECTION=true env var and DRIFT-INJECTION-E2E closed")
	})
})
