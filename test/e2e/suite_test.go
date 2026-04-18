// Package e2e contains the wrapper end-to-end test suite.
//
// These tests verify the PackExecution full gate sequence and PackInstance drift
// detection behaviour on a live cluster.
//
// Required environment variables:
//   - MGMT_KUBECONFIG  — path to management cluster kubeconfig
//   - TENANT_KUBECONFIG — path to tenant cluster kubeconfig
//   - REGISTRY_ADDR    — OCI registry address (default: localhost:5000)
//   - MGMT_CLUSTER_NAME — management cluster name (default: ccs-mgmt)
//   - TENANT_CLUSTER_NAME — tenant cluster name (default: ccs-test)
//
// Run with: make e2e
// Skip condition: MGMT_KUBECONFIG absent → all specs skip.
package e2e_test

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	e2ehelpers "github.com/ontai-dev/seam-core/pkg/e2e"
)

// Suite-level cluster clients, initialized in BeforeSuite.
var (
	mgmtClient   *e2ehelpers.ClusterClient
	tenantClient *e2ehelpers.ClusterClient
	registry     *e2ehelpers.RegistryClient

	mgmtKubeconfig    string
	tenantKubeconfig  string
	registryAddr      string
	mgmtClusterName   string
	tenantClusterName string
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "wrapper E2E Suite")
}

var _ = BeforeSuite(func() {
	mgmtKubeconfig = os.Getenv("MGMT_KUBECONFIG")
	if mgmtKubeconfig == "" {
		Skip("MGMT_KUBECONFIG not set — tests require a live cluster")
	}

	tenantKubeconfig = os.Getenv("TENANT_KUBECONFIG")
	registryAddr = os.Getenv("REGISTRY_ADDR")
	if registryAddr == "" {
		registryAddr = e2ehelpers.DefaultRegistryAddr
	}
	mgmtClusterName = os.Getenv("MGMT_CLUSTER_NAME")
	if mgmtClusterName == "" {
		mgmtClusterName = "ccs-mgmt"
	}
	tenantClusterName = os.Getenv("TENANT_CLUSTER_NAME")
	if tenantClusterName == "" {
		tenantClusterName = "ccs-test"
	}

	var err error
	mgmtClient, err = e2ehelpers.NewClusterClient(mgmtClusterName, mgmtKubeconfig)
	Expect(err).NotTo(HaveOccurred(), "failed to build management cluster client")

	if tenantKubeconfig != "" {
		tenantClient, err = e2ehelpers.NewClusterClient(tenantClusterName, tenantKubeconfig)
		Expect(err).NotTo(HaveOccurred(), "failed to build tenant cluster client")
	}

	registry = e2ehelpers.NewRegistryClient(registryAddr)
})

var _ = It("placeholder — lab cluster not yet provisioned", func() {
	Skip("lab cluster not yet provisioned")
})
