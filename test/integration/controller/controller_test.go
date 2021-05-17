/**
	Overview
		- Tests the provider specific Machine Controller
	Prerequisites
		- secret yaml file for the hyperscaler/provider passed as input
		- control cluster and target clusters kube-config passed as input (optional)
	BeforeSuite
		- Check and create control cluster and target clusters if required
		- Check and create crds ( machineclass, machines, machinesets and machinedeployment ) if required
		  using file available in kubernetes/crds directory of machine-controller-manager repo
		- Start the Machine Controller manager ( as goroutine )
		- apply secret resource for accesing the cloud provider service in the control cluster
		- Create machineclass resource from file available in kubernetes directory of provider specific repo in control cluster
	AfterSuite
		- Delete the control and target clusters // As of now we are reusing the cluster so this is not required

	Test: differentRegion Scheduling Strategy Test
        1) Create machine in region other than where the target cluster exists. (e.g machine in eu-west-1 and target cluster exists in us-east-1)
           Expected Output
			 - should fail because no cluster in same region exists)

    Test: sameRegion Scheduling Strategy Test
        1) Create machine in same region/zone as target cluster and attach it to the cluster
           Expected Output
			 - should successfully attach the machine to the target cluster (new node added)
		2) Delete machine
			Expected Output
			 - should successfully delete the machine from the target cluster (less one node)
 **/

package controller_test

import (
	"log"
	"os"

	"github.com/gardener/machine-controller-manager-provider-aws/test/integration/provider"
	"github.com/gardener/machine-controller-manager/pkg/integrationtest/common"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	controlKubeConfigPath    = os.Getenv("controlKubeconfig")
	targetKubeConfigPath     = os.Getenv("targetKubeconfig")
	providerResourcesTracker *provider.ResourcesTracker
	mcmRepoPath              = "../../../dev/mcm"
	logPath                  = "../../../dev/"
	// mc_logFile             = filepath.Join("../../../dev/", "integration-test-mc.log")
	// mcm_logFile            = ioutil.TempFile()
	mcContainerImageTag       = os.Getenv("mcContainerImage")
	mcmContainerImageTag      = os.Getenv("mcmContainerImage")
	controlClusterNamespace   = os.Getenv("controlClusterNamespace")
	testMachineClassResources = []string{"test-mc", "test-mc-dummy"}
)

var commons = common.NewContollerTest(controlKubeConfigPath,
	targetKubeConfigPath,
	mcmRepoPath,
	logPath,
	mcContainerImageTag,
	mcmContainerImageTag,
	controlClusterNamespace)

var _ = Describe("Integration test", func() {
	BeforeSuite(func() {
		commons.SetupBeforeSuite()
		// initialize orphan resource tracker
		ControlKubeCluster := commons.ControlKubeCluster
		machineClass, err := ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Get(testMachineClassResources[0], metav1.GetOptions{})
		if err == nil {
			secret, err := ControlKubeCluster.Clientset.CoreV1().Secrets(machineClass.SecretRef.Namespace).Get(machineClass.SecretRef.Name, metav1.GetOptions{})
			if err == nil {
				clusterName, err := ControlKubeCluster.ClusterName()
				log.Println("control cluster nmae is :", clusterName)
				Expect(err).NotTo(HaveOccurred())
				providerResourcesTracker, err = provider.NewResourcesTracker(machineClass, secret, clusterName)
				//Check there is no error occured
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(err).NotTo(HaveOccurred())
		log.Println("Orphan resource tracker initialized")
	})
	BeforeEach(func() {
		commons.BeforeEachCheck()
	})

	commons.ControllerTests()
	// ---------------------------------------------------------------------------------------
	// Testcase #03 | Orphaned Resources
	Describe("Check for orphaned resources", func() {
		Context("In target cluster", func() {
			Context("Check if there are any resources matching the tag exists", func() {
				It("Should list any orphaned resources if available", func() {
					// if available should delete orphaned resources in cloud provider
					Expect(providerResourcesTracker.IsOrphanedResourcesAvailable()).To(BeFalse())
				})
			})
		})
	})

	commons.Cleanup()
})
