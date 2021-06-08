package common

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	v1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	"github.com/gardener/machine-controller-manager/pkg/test/integration/common/helpers"
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	appsV1 "k8s.io/api/apps/v1"
	coreV1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/retry"
)

var (
	// path for storing log files (mcm & mc processes)
	targetDir = os.TempDir()
	// machine-controller-manager log file
	mcmLogFile = filepath.Join(targetDir, "mcm_process.log")
	// machine-controller log file
	mcLogFile = filepath.Join(targetDir, "mc_process.log")

	// relative path to clone machine-controller-manager repo.
	// Only used if no mcm or mc image tag is available and thus running make command locally
	mcmRepoPath = filepath.Join("..", "..", "..", "dev", "mcm")

	// names of machineclass resource. the second one for upgrade machines test
	testMachineClassResources = []string{"test-mc", "test-mc-dummy"}

	// control cluster namespace to create resources.
	// ignored if the target cluster is a shoot of control cluster
	controlClusterNamespace = os.Getenv("controlClusterNamespace")

	// make processes/sessions started by gexec. available only if the controllers are running in local setup. updated during runtime
	mcmsession, mcsession *gexec.Session
	// mcmDeploymentOrigObj a placeholder for mcm deployment object running in seed cluster.
	// it will be scaled down to 0 before test starts.
	// also used in cleanup to restore the controllers to its original state.
	// used only if control cluster is seed
	mcmDeploymentOrigObj *appsV1.Deployment
)

type IntegrationTestFramework struct {
	// Must be rti implementation for the hyperscaler provider.
	// It is used for checking orphan resources.
	resourcesTracker helpers.ResourcesTrackerInterface

	// Control cluster resource containing ClientSets for accessing kubernetes resources
	// And kubeconfig file path for the cluster
	// initialization is done by SetupBeforeSuite
	ControlCluster *helpers.Cluster

	// Target cluster resource containing ClientSets for accessing kubernetes resources
	// And kubeconfig file path for the cluster
	// initialization is done by SetupBeforeSuite
	TargetCluster *helpers.Cluster
}

// NewIntegrationTestFramework creates a new IntegrationTestFramework
// initializing resource tracker implementation.
// and containing placeholder for ControlCluster and TargetCluster
func NewIntegrationTestFramework(resourcesTracker helpers.ResourcesTrackerInterface) (c *IntegrationTestFramework) {
	c = &IntegrationTestFramework{
		resourcesTracker: resourcesTracker,
	}
	return c
}

func (c *IntegrationTestFramework) initalizeClusters() error {
	/* prepareClusters checks for
	- the validity of controlKubeConfig and targetKubeConfig flags
	- It should return an error if thre is a error
	*/
	controlKubeConfigPath := os.Getenv("controlKubeconfig")
	targetKubeConfigPath := os.Getenv("targetKubeconfig")
	log.Printf("Control cluster kube-config - %s\n", controlKubeConfigPath)
	log.Printf("Target cluster kube-config  - %s\n", targetKubeConfigPath)
	if controlKubeConfigPath != "" {
		controlKubeConfigPath, _ = filepath.Abs(controlKubeConfigPath)
		// if control cluster config is available but not the target, then set control and target clusters as same
		if targetKubeConfigPath == "" {
			targetKubeConfigPath = controlKubeConfigPath
			log.Println("Missing targetKubeConfig. control cluster will be set as target too")
		}
		targetKubeConfigPath, _ = filepath.Abs(targetKubeConfigPath)
		// use the current context in controlkubeconfig
		var err error
		c.ControlCluster, err = helpers.NewCluster(controlKubeConfigPath)
		if err != nil {
			return err
		}
		c.TargetCluster, err = helpers.NewCluster(targetKubeConfigPath)
		if err != nil {
			return err
		}

		// update clientset and check whether the cluster is accessible
		err = c.ControlCluster.FillClientSets()
		if err != nil {
			log.Println("Failed to check nodes in the cluster")
			return err
		}

		err = c.TargetCluster.FillClientSets()
		if err != nil {
			log.Println("Failed to check nodes in the cluster")
			return err
		}
	}

	// Update namespace to use
	if c.ControlCluster.IsSeed(c.TargetCluster) {
		_, err := c.TargetCluster.ClusterName()
		if err != nil {
			log.Println("Failed to determine shoot cluster namespace")
			return err
		}
		controlClusterNamespace, _ = c.TargetCluster.ClusterName()
	} else if controlClusterNamespace == "" {
		controlClusterNamespace = "default"
	}
	return nil
}

func (c *IntegrationTestFramework) prepareMcmDeployment(mcContainerImageTag string, mcmContainerImageTag string, byCreating bool) error {
	/*
		if any of mcmContainerImage  or mcContainerImageTag flag is non-empty then,
		update machinecontrollermanager deployment in the control-cluster with specified image
		 -
	*/
	if byCreating {
		// Create clusterroles and clusterrolebindings for control and target cluster
		// Create secret containing target kubeconfig file
		// Create machine-deployment using the yaml file
		c.ControlCluster.ControlClusterRolesAndRoleBindingSetup()
		c.TargetCluster.TargetClusterRolesAndRoleBindingSetup()
		configFile, _ := os.ReadFile(c.TargetCluster.KubeConfigFilePath)
		c.ControlCluster.Clientset.CoreV1().Secrets(controlClusterNamespace).Create(&coreV1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "machine-controller-manager-target",
			},
			Data: map[string][]byte{
				"kubeconfig": configFile,
			},
			Type: coreV1.SecretTypeOpaque,
		})

		err := c.ControlCluster.
			ApplyFiles("../../../kubernetes/controllers/deployment.yaml",
				controlClusterNamespace)
		if err != nil {
			return err
		}
		// after creation, the machine-deployment resource container image tags will be updated now
	}

	// mcmDeploymentOrigObj holds a copy of original mcm deployment
	result, getErr := c.ControlCluster.Clientset.AppsV1().Deployments(controlClusterNamespace).Get("machine-controller-manager", metav1.GetOptions{})
	if getErr != nil {
		log.Printf("failed to get latest version of Deployment: %v", getErr)
		return getErr
	}
	mcmDeploymentOrigObj = result

	// update containers spec
	providerSpecificRegexp, _ := regexp.Compile("machine-controller-manager-provider-")
	containers := mcmDeploymentOrigObj.Spec.Template.Spec.Containers
	for i, container := range containers {

		// splitedString holds image name in splitedString[0]
		var splitedString []string
		if strings.Contains(container.Image, "@") {
			splitedString = strings.Split(container.Image, "@")
		} else {
			splitedString = strings.Split(container.Image, ":")
		}

		if providerSpecificRegexp.Match([]byte(container.Name)) {
			// set container image to mcContainerImageTag as the name of container contains provider
			if len(mcContainerImageTag) != 0 {
				container.Image = splitedString[0] + ":" + mcContainerImageTag
			}
		} else {
			// set container image to mcmContainerImageTag as the name of container contains provider
			if len(mcmContainerImageTag) != 0 {
				container.Image = splitedString[0] + ":" + mcmContainerImageTag
			}

			// set machine-safety-overshooting-period to 300ms for freeze check to succeed
			var isOptionAvailable bool
			for option := range container.Command {
				if strings.Contains(container.Command[option], "machine-safety-overshooting-period=") {
					isOptionAvailable = true
					container.Command[option] = "--machine-safety-overshooting-period=300ms"
				}
			}
			if !isOptionAvailable {
				container.Command = append(container.Command, "--machine-safety-overshooting-period=300ms")
			}
		}
		containers[i] = container
	}

	// apply updated containers spec to mcmDeploymentObj in kubernetes cluster
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		mcmDeployment, getErr := c.ControlCluster.Clientset.AppsV1().Deployments(controlClusterNamespace).Get("machine-controller-manager", metav1.GetOptions{})
		if getErr != nil {
			log.Printf("failed to get latest version of Deployment: %v", getErr)
			return getErr
		}
		mcmDeployment.Spec.Template.Spec.Containers = containers
		_, updateErr := c.ControlCluster.Clientset.AppsV1().Deployments(controlClusterNamespace).Update(mcmDeployment)
		return updateErr
	})
	return retryErr
}

func (c *IntegrationTestFramework) scaleMcmDeployment(replicas int32) error {
	/*
		 - if any of mcmContainerImage  or mcContainerImageTag flag is non-empty then,
			 update machinecontrollermanager deployment in the control-cluster with specified image
		 -
	*/
	result, getErr := c.ControlCluster.Clientset.AppsV1().Deployments(controlClusterNamespace).Get("machine-controller-manager", metav1.GetOptions{})
	if getErr != nil {
		return getErr
	}
	mcmDeploymentOrigObj = result

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		result, getErr := c.ControlCluster.Clientset.AppsV1().Deployments(controlClusterNamespace).Get("machine-controller-manager", metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		*result.Spec.Replicas = replicas
		_, updateErr := c.ControlCluster.Clientset.AppsV1().Deployments(controlClusterNamespace).Update(result)
		return updateErr
	})
	return retryErr
}

// setupMachineClass reads the control cluster machineclass resource and creates a duplicate of it.
// Additionally it adds the delta part found in machine-class-patch.yaml file inside kubernetes directory of provider specific repo
// OR
// use machine-class.yaml file instead for creating machine class from scratch
func (c *IntegrationTestFramework) setupMachineClass() error {

	machineClasses, err := c.ControlCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	var newMachineClass *v1alpha1.MachineClass
	machineClass := machineClasses.Items[0]

	// Create machine-class using yaml and any of existing machineclass resource combined
	for _, resource_name := range testMachineClassResources {
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			result, getErr := c.ControlCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Get(machineClass.GetName(), metav1.GetOptions{})
			if getErr != nil {
				log.Println("Failed to get latest version of machineclass")
				return getErr
			}
			//machineClassOrigObj = *result
			metaData := metav1.ObjectMeta{
				Name:        resource_name,
				Labels:      result.ObjectMeta.Labels,
				Annotations: result.ObjectMeta.Annotations,
			}
			newMachineClass = &v1alpha1.MachineClass{
				ObjectMeta:           metaData,
				ProviderSpec:         result.ProviderSpec,
				SecretRef:            result.SecretRef,
				CredentialsSecretRef: result.CredentialsSecretRef,
				Provider:             result.Provider,
			}
			// c.applyFiles(machineClass)
			// remove dynamic fileds. eg uid, creation time e.t.c.,
			// create result (or machineClassOrigObj) with "../../../kubernetes/machine-class.yaml" content
			_, createErr := c.ControlCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Create(newMachineClass)
			return createErr
		})
		if retryErr != nil {
			return retryErr
		}

		// patch

		retryErr = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// read machineClass patch yaml file ("../../../kubernetes/machine-class-patch.yaml" ) and update machine class(machineClass)
			data, err := os.ReadFile("../../../kubernetes/machine-class-patch.json")
			if err != nil {
				// Error reading file. So skipping it
				return nil
			}
			_, patchErr := c.ControlCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Patch(newMachineClass.Name, types.MergePatchType, data)
			return patchErr
		})
		if retryErr != nil {
			return retryErr
		}
	}
	return nil
}

// SetupBeforeSuite performs intial setup for the test by
// - Checks control cluster and target clusters are accessible and initializes ControlCluster and TargetCluster.
// - Check and optionally create crds (machineclass, machines, machinesets and machinedeployment) using kubernetes/crds directory of mcm repo.
// - Setup controller processes either as a pod in control cluster or running locally.
// - Setup machineclass to use either by copying existing machineclass in seed cluster or by applying file kubernetes/machine-class.yaml file in provider repo.
// - invokes InitializeResourcesTracker or rti for orphan resource check.
func (c *IntegrationTestFramework) SetupBeforeSuite() {
	log.SetOutput(ginkgo.GinkgoWriter)
	mcContainerImageTag := os.Getenv("mcContainerImage")
	mcmContainerImageTag := os.Getenv("mcmContainerImage")
	ginkgo.By("Checking for the clusters if provided are available")
	gomega.Expect(c.initalizeClusters()).To(gomega.BeNil())

	// preparing resources
	if !c.ControlCluster.IsSeed(c.TargetCluster) {

		ginkgo.By("Cloning Machine-Controller-Manager github repo")
		gomega.Expect(helpers.CloneRepo("https://github.com/gardener/machine-controller-manager.git", mcmRepoPath)).To(gomega.BeNil())

		//create the custom resources in the control cluster using yaml files
		//available in kubernetes/crds directory of machine-controller-manager repo
		//resources to be applied are machineclass, machines, machinesets and machinedeployment
		ginkgo.By("Applying kubernetes/crds into control cluster")
		gomega.Expect(c.ControlCluster.ApplyFiles(filepath.Join(mcmRepoPath, "kubernetes/crds"), controlClusterNamespace)).To(gomega.BeNil())

		// if isControlClusterIsShootsSeed is true, then use machineclass from cluster
		// probe for machine-class in the identified namespace and then creae a copy of this machine-class with
		// additional delta available in machineclass-delta.yaml
		// eg. tag (providerSpec.tags)  \"mcm-integration-test: "true"\"
		ginkgo.By("Applying MachineClass")
		gomega.Expect(c.ControlCluster.ApplyFiles("../../../kubernetes/machine-class.yaml", controlClusterNamespace)).To(gomega.BeNil())
	} else {
		// If no tags specified then - applyCrds from the mcm repo by cloning
		if !(len(mcContainerImageTag) != 0 && len(mcmContainerImageTag) != 0) {
			ginkgo.By("Cloning Machine-Controller-Manager github repo")
			gomega.Expect(helpers.CloneRepo("https://github.com/gardener/machine-controller-manager.git", mcmRepoPath)).To(gomega.BeNil())

			//create the custom resources in the control cluster using yaml files
			//available in kubernetes/crds directory of machine-controller-manager repo
			//resources to be applied are machineclass, machines, machinesets and machinedeployment
			ginkgo.By("Applying kubernetes/crds into control cluster")
			gomega.Expect(c.ControlCluster.ApplyFiles(filepath.Join(mcmRepoPath, "kubernetes/crds"), controlClusterNamespace)).To(gomega.BeNil())
		}
		ginkgo.By("Creating dup MachineClass with delta yaml")
		gomega.Expect(c.setupMachineClass()).To(gomega.BeNil())
	}

	// starting controllers
	if len(mcContainerImageTag) != 0 && len(mcmContainerImageTag) != 0 {

		/* if any of mcmContainerImage  or mcContainerImageTag flag is non-empty then,
		create/update machinecontrollermanager deployment in the control-cluster with specified image
		*/
		if c.ControlCluster.IsSeed(c.TargetCluster) {
			ginkgo.By("Updating MCM Deployemnt")
			gomega.Expect(c.prepareMcmDeployment(mcContainerImageTag, mcmContainerImageTag, false)).To(gomega.BeNil())
		} else {
			ginkgo.By("Creating MCM Deployemnt")
			gomega.Expect(c.prepareMcmDeployment(mcContainerImageTag, mcmContainerImageTag, true)).To(gomega.BeNil())
		}

	} else {
		/*
		 run mc and mcm locally
		*/
		if c.ControlCluster.IsSeed(c.TargetCluster) {
			ginkgo.By("Scaledown existing machine controllers")
			gomega.Expect(c.scaleMcmDeployment(0)).To(gomega.BeNil())
		}

		ginkgo.By("Starting Machine Controller ")
		args := strings.Fields(fmt.Sprintf("make --directory=%s start CONTROL_KUBECONFIG=%s TARGET_KUBECONFIG=%s CONTROL_NAMESPACE=%s LEADER_ELECT=false ",
			"../../..",
			c.ControlCluster.KubeConfigFilePath,
			c.TargetCluster.KubeConfigFilePath,
			controlClusterNamespace))
		outputFile, err := helpers.RotateLogFile(mcLogFile)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		sess, err := gexec.Start(exec.Command(args[0], args[1:]...), outputFile, outputFile)
		mcsession = sess
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		gomega.Expect(mcsession.ExitCode()).Should(gomega.Equal(-1))

		ginkgo.By("Starting Machine Controller Manager")
		args = strings.Fields(fmt.Sprintf("make --directory=%s start CONTROL_KUBECONFIG=%s TARGET_KUBECONFIG=%s CONTROL_NAMESPACE=%s LEADER_ELECT=false MACHINE_SAFETY_OVERSHOOTING_PERIOD=300ms",
			mcmRepoPath,
			c.ControlCluster.KubeConfigFilePath,
			c.TargetCluster.KubeConfigFilePath,
			controlClusterNamespace))
		outputFile, err = helpers.RotateLogFile(mcmLogFile)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		mcmsession, err = gexec.Start(exec.Command(args[0], args[1:]...), outputFile, outputFile)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		gomega.Expect(mcmsession.ExitCode()).Should(gomega.Equal(-1))
	}

	// initialize orphan resource tracker
	ginkgo.By("looking for machineclass resource in the control cluster")
	machineClass, err := c.ControlCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Get(testMachineClassResources[0], metav1.GetOptions{})
	if err == nil {
		secret, err := c.ControlCluster.Clientset.CoreV1().Secrets(machineClass.SecretRef.Namespace).Get(machineClass.SecretRef.Name, metav1.GetOptions{})
		ginkgo.By("looking for secret resource refered in machineclass in the control cluster")
		if err == nil {
			ginkgo.By("determining control cluster name")
			clusterName, err := c.ControlCluster.ClusterName()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.By("initializing orphan resource tracker")
			err = c.resourcesTracker.InitializeResourcesTracker(machineClass, secret, clusterName)
			//Check there is no error occured
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	log.Println("orphan resource tracker initialized")
}

// BeforeEachCheck checks if all the nodes are ready.
// And the controllers are runnings
func (c *IntegrationTestFramework) BeforeEachCheck() {
	ginkgo.BeforeEach(func() {
		if len(os.Getenv("mcContainerImage")) == 0 && len(os.Getenv("mcmContainerImage")) == 0 {
			ginkgo.By("Checking machineController process is running")
			gomega.Expect(mcsession.ExitCode()).Should(gomega.Equal(-1))
			ginkgo.By("Checking machineControllerManager process is running")
			gomega.Expect(mcmsession.ExitCode()).Should(gomega.Equal(-1))
		}
		// Nodes are healthy
		ginkgo.By("Checking nodes in target cluster are healthy")
		gomega.Eventually(c.TargetCluster.GetNumberOfReadyNodes, 180, 5).Should(gomega.BeNumerically("==", c.TargetCluster.GetNumberOfNodes()))
	})
}

// ControllerTests runs common tests by using yaml files in kubernetes directory inside provider specific repo.
// Common tests are ...
// machine resource creation and deletion,
// machine deployment resource creation, scale-up, scale-down, update and deletion. And
// orphan resource check by invoking IsOrphanedResourcesAvailable from rti
func (c *IntegrationTestFramework) ControllerTests() {
	// Testcase #01 | Machine
	ginkgo.Describe("Machine Resource", func() {
		var initialNodes int16
		ginkgo.Context("Creation", func() {
			// Probe nodes currently available in target cluster
			ginkgo.It("should not lead to any errors and add 1 more node in target cluste", func() {
				// apply machine resource yaml file
				initialNodes = c.TargetCluster.GetNumberOfNodes()
				ginkgo.By("checking for errors")
				gomega.Expect(c.ControlCluster.ApplyFiles("../../../kubernetes/machine.yaml", controlClusterNamespace)).To(gomega.BeNil())
				//fmt.Println("wait for 30 sec before probing for nodes")

				// check whether there is one node more
				ginkgo.By("Waiting until number of ready nodes is 1 more than initial nodes")
				gomega.Eventually(c.TargetCluster.GetNumberOfNodes, 600, 5).Should(gomega.BeNumerically("==", initialNodes+1))
				gomega.Eventually(c.TargetCluster.GetNumberOfReadyNodes, 600, 5).Should(gomega.BeNumerically("==", initialNodes+1))
			})
		})

		ginkgo.Context("Deletion", func() {
			ginkgo.Context("When machines available", func() {
				ginkgo.It("should not lead to errors and remove 1 node in target cluster", func() {
					machinesList, _ := c.ControlCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).List(metav1.ListOptions{})
					if len(machinesList.Items) != 0 {
						ginkgo.By("checking for errors")
						gomega.Expect(c.ControlCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).Delete("test-machine", &metav1.DeleteOptions{})).Should(gomega.BeNil(), "No Errors while deleting machine")

						ginkgo.By("Waiting until number of ready nodes is eual to number of initial  nodes")
						gomega.Eventually(c.TargetCluster.GetNumberOfNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes))
						gomega.Eventually(c.TargetCluster.GetNumberOfReadyNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes))
					}

				})
			})
			ginkgo.Context("when machines are not available", func() {
				// delete one machine (non-existent) by random text as name of resource
				// check there are no changes to nodes

				ginkgo.It("should keep nodes intact", func() {
					// Keep count of nodes available
					// delete machine resource
					machinesList, _ := c.ControlCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).List(metav1.ListOptions{})
					if len(machinesList.Items) == 0 {
						err := c.ControlCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).Delete("test-machine-dummy", &metav1.DeleteOptions{})
						ginkgo.By("checking for errors")
						gomega.Expect(err).To(gomega.HaveOccurred())
						time.Sleep(30 * time.Second)
						ginkgo.By("Checking number of ready nodes is eual to number of initial nodes")
						gomega.Expect(c.TargetCluster.GetNumberOfNodes()).To(gomega.BeEquivalentTo(initialNodes))
					} else {
						ginkgo.By("Skipping as there are machines available and this check can't be performed")
					}
				})
			})
		})
	})

	// Testcase #02 | Machine Deployment
	ginkgo.Describe("Machine Deployment resource", func() {
		var initialNodes int16 // initialization should be part of creation test logic
		ginkgo.Context("creation with replicas=3", func() {
			ginkgo.It("should not lead to errors and add 3 more nodes to target cluster", func() {
				//probe initialnodes before continuing
				initialNodes = c.TargetCluster.GetNumberOfNodes()

				// apply machinedeployment resource yaml file
				ginkgo.By("checking for errors")
				gomega.Expect(c.ControlCluster.ApplyFiles("../../../kubernetes/machine-deployment.yaml", controlClusterNamespace)).To(gomega.BeNil())

				// check whether all the expected nodes are ready
				ginkgo.By("Waiting until number of ready nodes are 3 more than initial")
				gomega.Eventually(c.TargetCluster.GetNumberOfNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes+3))
				gomega.Eventually(c.TargetCluster.GetNumberOfReadyNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes+3))
			})
		})
		ginkgo.Context("scale-up with replicas=6", func() {
			ginkgo.It("should not lead to errors and add futher 3 nodes to target cluster", func() {

				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := c.ControlCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Replicas = 6
					_, updateErr := c.ControlCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Update(machineDployment)
					return updateErr
				})
				ginkgo.By("checking for errors")
				gomega.Expect(retryErr).NotTo(gomega.HaveOccurred())
				// check whether all the expected nodes are ready
				ginkgo.By("checking number of ready nodes are 6 more than initial")
				gomega.Eventually(c.TargetCluster.GetNumberOfNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes+6))
				gomega.Eventually(c.TargetCluster.GetNumberOfReadyNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes+6))
			})

		})
		ginkgo.Context("scale-down with replicas=2", func() {
			// rapidly scaling back to 2 leading to a freezing and unfreezing
			// check for freezing and unfreezing of machine due to rapid scale up and scale down in the logs of mcm

			ginkgo.It("Should not lead to errors and remove 4 nodes from target cluster", func() {
				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := c.ControlCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Replicas = 2
					_, updateErr := c.ControlCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Update(machineDployment)
					return updateErr
				})
				ginkgo.By("checking for errors")
				gomega.Expect(retryErr).NotTo(gomega.HaveOccurred())

				ginkgo.By("checking number of ready nodes are 2 more than initial")
				gomega.Eventually(c.TargetCluster.GetNumberOfNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes+2))
				gomega.Eventually(c.TargetCluster.GetNumberOfReadyNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes+2))
			})
			ginkgo.It("should freeze and unfreeze machineset temporarily", func() {
				if mcsession == nil {
					// controllers running in pod
					// Create log file from container log
					outputFile, err := helpers.RotateLogFile(mcmLogFile)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					ginkgo.By("reading containerlog is not erroring")
					mcmPod, err := c.ControlCluster.Clientset.CoreV1().Pods(controlClusterNamespace).List(metav1.ListOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					readCloser, err := c.ControlCluster.Clientset.CoreV1().
						Pods(controlClusterNamespace).
						GetLogs(mcmPod.Items[0].Name, &coreV1.PodLogOptions{
							Container: "machine-controller-manager",
						}).Stream()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					io.Copy(outputFile, readCloser)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}

				ginkgo.By("Searching for Froze in mcm log file")
				frozeRegexp, _ := regexp.Compile(` Froze MachineSet`)
				gomega.Eventually(func() bool {
					data, _ := ioutil.ReadFile(mcmLogFile)
					return frozeRegexp.Match(data)
				}, 300, 5).Should(gomega.BeTrue())

				ginkgo.By("Searching Unfroze in mcm log file")
				unfrozeRegexp, _ := regexp.Compile(` Unfroze MachineSet`)
				gomega.Eventually(func() bool {
					data, _ := ioutil.ReadFile(mcmLogFile)
					return unfrozeRegexp.Match(data)
				}, 300, 5).Should(gomega.BeTrue())
			})
		})
		ginkgo.XContext("Updation to v2 machine-class and replicas=4", func() {
			// update machine type -> machineDeployment.spec.template.spec.class.name = "test-mc-dummy"
			// scale up replicas by 4
			// To-Do: Add check for rolling update completion (updatedReplicas check)
			ginkgo.It("should upgrade machines and add more nodes to target", func() {
				// wait for 2400s till machines updates
				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := c.ControlCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Template.Spec.Class.Name = testMachineClassResources[1]
					machineDployment.Spec.Replicas = 4
					_, updateErr := c.ControlCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Update(machineDployment)
					return updateErr
				})
				//Check there is no error occured
				ginkgo.By("checking for errors")
				gomega.Expect(retryErr).NotTo(gomega.HaveOccurred())
				ginkgo.By("updatedReplicas to be 4")
				gomega.Eventually(c.ControlCluster.GetUpdatedReplicasCount("test-machine-deployment", controlClusterNamespace), 900, 5).Should(gomega.BeNumerically("==", 4))
				ginkgo.By("number of ready nodes be 4 more")
				gomega.Eventually(c.TargetCluster.GetNumberOfNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes+4))
				gomega.Eventually(c.TargetCluster.GetNumberOfReadyNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes+4))

			})
		})
		ginkgo.Context("Deletion", func() {
			ginkgo.Context("When there are machine deployment(s) available in control cluster", func() {
				ginkgo.It("should not lead to errors and list only initial nodes", func() {
					_, err := c.ControlCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					if err == nil {
						//delete machine resource
						ginkgo.By("checking for errors")
						gomega.Expect(c.ControlCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Delete("test-machine-deployment", &metav1.DeleteOptions{})).Should(gomega.BeNil())
						ginkgo.By("Waiting until number of ready nodes is eual to number of initial  nodes")
						gomega.Eventually(c.TargetCluster.GetNumberOfNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes))
						gomega.Eventually(c.TargetCluster.GetNumberOfReadyNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes))
					}
				})
			})
		})
	})

	// Testcase #03 | Orphaned Resources
	ginkgo.Describe("Zero Orphaned resource", func() {
		ginkgo.Context("when the hyperscaler resources are querried", func() {
			ginkgo.It("should match with inital resources", func() {
				// if available should delete orphaned resources in cloud provider
				ginkgo.By("Querrying and comparing")
				gomega.Expect(c.resourcesTracker.IsOrphanedResourcesAvailable()).To(gomega.BeFalse())
			})
		})
	})
}

//Cleanup performs rollback of original resources and removes any machines created by the test
func (c *IntegrationTestFramework) Cleanup() {

	//running locally
	if mcsession != nil {
		if mcsession.ExitCode() != -1 {
			ginkgo.By("Restarting Machine Controller ")
			outputFile, err := helpers.RotateLogFile(mcLogFile)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gexec.Start(mcsession.Command, outputFile, outputFile)
		}
		if mcmsession.ExitCode() != -1 {
			ginkgo.By("Restarting Machine Controller Manager")
			outputFile, err := helpers.RotateLogFile(mcmLogFile)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gexec.Start(mcmsession.Command, outputFile, outputFile)
		}
	}

	if c.ControlCluster.McmClient != nil {
		timeout := int64(900)
		// Check and delete machinedeployment resource
		_, err := c.ControlCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
		if err == nil {
			log.Println("deleting test-machine-deployment")
			watchMachinesDepl, _ := c.ControlCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Watch(metav1.ListOptions{TimeoutSeconds: &timeout}) //ResourceVersion: machineDeploymentObj.ResourceVersion
			for event := range watchMachinesDepl.ResultChan() {
				c.ControlCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Delete("test-machine-deployment", &metav1.DeleteOptions{})
				if event.Type == watch.Deleted {
					watchMachinesDepl.Stop()
					log.Println("machinedeployment deleted")
				}
			}
		} else {
			log.Println(err.Error())
		}
		// Check and delete machine resource
		_, err = c.ControlCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).Get("test-machine", metav1.GetOptions{})
		if err == nil {
			log.Println("deleting test-machine")
			watchMachines, _ := c.ControlCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).Watch(metav1.ListOptions{TimeoutSeconds: &timeout}) //ResourceVersion: machineObj.ResourceVersion
			for event := range watchMachines.ResultChan() {
				c.ControlCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).Delete("test-machine", &metav1.DeleteOptions{})
				if event.Type == watch.Deleted {
					watchMachines.Stop()
					log.Println("machine deleted")
				}
			}
		} else {
			log.Println(err.Error())
		}

		for _, machineClassName := range testMachineClassResources {
			// Check and delete machine class resource
			_, err = c.ControlCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Get(machineClassName, metav1.GetOptions{})
			if err == nil {
				log.Printf("deleting %s machineclass", machineClassName)
				watchMachineClass, _ := c.ControlCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Watch(metav1.ListOptions{TimeoutSeconds: &timeout}) //ResourceVersion: machineObj.ResourceVersion
				for event := range watchMachineClass.ResultChan() {
					c.ControlCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Delete(machineClassName, &metav1.DeleteOptions{})
					if event.Type == watch.Deleted {
						watchMachineClass.Stop()
						log.Println("machineclass deleted")
					}
				}
			} else {
				log.Println(err.Error())
			}
		}
	}
	if c.ControlCluster.IsSeed(c.TargetCluster) {
		retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Retrieve the latest version of Deployment before attempting update
			// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
			_, updateErr := c.ControlCluster.Clientset.AppsV1().Deployments(mcmDeploymentOrigObj.Namespace).Update(mcmDeploymentOrigObj)
			return updateErr
		})
	} else {
		if len(os.Getenv("mcContainerImage")) != 0 && len(os.Getenv("mcmContainerImage")) != 0 {
			c.ControlCluster.ClusterRolesAndRoleBindingCleanup()
			c.TargetCluster.ClusterRolesAndRoleBindingCleanup()
			c.ControlCluster.Clientset.CoreV1().Secrets(controlClusterNamespace).Delete("machine-controller-manager-target", &metav1.DeleteOptions{})
			c.ControlCluster.Clientset.AppsV1().Deployments(controlClusterNamespace).Delete("machine-controller-manager", &metav1.DeleteOptions{})
		}
	}
}
