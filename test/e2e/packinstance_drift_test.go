package e2e_test

// Scenario: PackInstance drift detection
//
// Pre-conditions required for this test to run:
//   - ccs-mgmt and ccs-test both provisioned and running
//   - Wrapper operator running in seam-system on ccs-mgmt
//   - A PackInstance CR exists in infra-system on ccs-mgmt for a deployed pack
//   - Conductor agent running in ont-system on ccs-test (drift detection loop active)
//   - At least one resource from the deployed ClusterPack exists on ccs-test
//
// What this test verifies (wrapper-schema.md §8):
//   - Deleting a managed resource on ccs-test is detected by conductor drift loop
//   - PackReceipt on ccs-test is updated with driftStatus=Drifted
//   - PackInstance on ccs-mgmt reflects Drifted condition
//   - PackInstance with DependencyBlocked condition when a dependency PackInstance
//     is in a non-Ready state and dependencyPolicy.onDrift=Block

import (
	. "github.com/onsi/ginkgo/v2"
)

var _ = Describe("PackInstance drift detection", func() {
	It("deleting a managed resource on the target cluster is detected as drift", func() {
		Skip("lab cluster not yet provisioned")
	})

	It("PackReceipt on ccs-test shows driftStatus=Drifted after resource deletion", func() {
		Skip("lab cluster not yet provisioned")
	})

	It("PackInstance on ccs-mgmt reflects Drifted condition from PackReceipt", func() {
		Skip("lab cluster not yet provisioned")
	})

	It("PackInstance shows DependencyBlocked when upstream dependency is Drifted with Block policy", func() {
		Skip("lab cluster not yet provisioned")
	})
})
