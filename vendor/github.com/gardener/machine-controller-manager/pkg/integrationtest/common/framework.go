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
	"github.com/gardener/machine-controller-manager/pkg/integrationtest/common/helpers"
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

const (
	mcmLogFileName = "mcm_process.log" // Name of machine-controller-manager log file
	mcLogFileName  = "mc_process.log"  // Name of machine-controller log file
	// relative path to clone machine-controller-manager repo.
	// Only used if no mcm or mc image tag is available and thus running make command locally
	mcmRepoPath = "../../../dev/mcm"
)

var (
	// names of machineclass resource. the second one for upgrade machines test
	testMachineClassResources = []string{"test-mc", "test-mc-dummy"}

	// control cluster namespace to create resources.
	// ignored if the target cluster is a shoot of control cluster
	controlClusterNamespace = os.Getenv("controlClusterNamespace")

	// path for storing log files (mcm_process.log & mc_process.log)
	targetDir = os.TempDir()

	// make processes/sessions started by gexec. available only if the controllers are running in local setup. updated during runtime
	mcmsession, mcsession *gexec.Session
	// mcmDeploymentOrigObj a placeholder for mcm deployment object running in seed cluster.
	// it will be scaled down to 0 before test starts.
	// also used in cleanup to restore the controllers to its original state.
	// used only if control cluster is seed
	mcmDeploymentOrigObj appsV1.Deployment
)

type IntegrationTestFramework struct {
	resourcesTracker   helpers.ResourcesTrackerInterface
	ControlKubeCluster *helpers.Cluster
	TargetKubeCluster  *helpers.Cluster
}

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
		c.ControlKubeCluster, err = helpers.NewCluster(controlKubeConfigPath)
		if err != nil {
			return err
		}
		c.TargetKubeCluster, err = helpers.NewCluster(targetKubeConfigPath)
		if err != nil {
			return err
		}

		// update clientset and check whether the cluster is accessible
		err = c.ControlKubeCluster.FillClientSets()
		if err != nil {
			log.Println("Failed to check nodes in the cluster")
			return err
		}

		err = c.TargetKubeCluster.FillClientSets()
		if err != nil {
			log.Println("Failed to check nodes in the cluster")
			return err
		}
	}

	// Update namespace to use
	if c.ControlKubeCluster.IsSeed(c.TargetKubeCluster) {
		_, err := c.TargetKubeCluster.ClusterName()
		if err != nil {
			log.Println("Failed to determine shoot cluster namespace")
			return err
		}
		controlClusterNamespace, _ = c.TargetKubeCluster.ClusterName()
	} else if controlClusterNamespace == "" {
		controlClusterNamespace = "default"
	}
	return nil
}

func (c *IntegrationTestFramework) prepareMcmDeployment(mcContainerImageTag string, mcmContainerImageTag string, byCreating bool) error {
	/*
		 - if any of mcmContainerImage  or mcContainerImageTag flag is non-empty then,
			 update machinecontrollermanager deployment in the control-cluster with specified image
		 -
	*/
	if byCreating {
		// Create clusterroles and clusterrolebindings for control and target cluster
		// Create secret containing target kubeconfig file
		// Create machine-deployment using the yaml file
		c.ControlKubeCluster.ControlClusterRolesAndRoleBindingSetup()
		c.TargetKubeCluster.TargetClusterRolesAndRoleBindingSetup()
		configFile, _ := os.ReadFile(c.TargetKubeCluster.KubeConfigFilePath)
		c.ControlKubeCluster.Clientset.CoreV1().Secrets(controlClusterNamespace).Create(&coreV1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "machine-controller-manager-target",
			},
			Data: map[string][]byte{
				"kubeconfig": configFile,
			},
			Type: coreV1.SecretTypeOpaque,
		})

		err := c.ControlKubeCluster.
			ApplyFiles("../../../kubernetes/controllers/deployment.yaml",
				controlClusterNamespace)
		if err != nil {
			return err
		}
		// once created, the machine-deployment resource container image tags will be updated by continuing here
	}
	providerSpecificRegexp, _ := regexp.Compile("machine-controller-manager-provider-")
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		result, getErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(controlClusterNamespace).Get("machine-controller-manager", metav1.GetOptions{})
		if getErr != nil {
			log.Printf("failed to get latest version of Deployment: %v", getErr)
			return getErr
		}
		mcmDeploymentOrigObj = *result
		for i := range result.Spec.Template.Spec.Containers {
			isProviderSpecific := providerSpecificRegexp.Match([]byte(result.Spec.Template.Spec.Containers[i].Name))
			var partsOfString []string // to hold eu.gcr.io/gardener-project/gardener/machine-controller-manager in partsOfString[0]
			if strings.Contains(result.Spec.Template.Spec.Containers[i].Image, "@") {
				partsOfString = strings.Split(result.Spec.Template.Spec.Containers[i].Image, "@")
			} else {
				partsOfString = strings.Split(result.Spec.Template.Spec.Containers[i].Image, ":")
			}
			if isProviderSpecific {
				if len(mcContainerImageTag) != 0 {
					if strings.Contains(mcContainerImageTag, "sha256") {
						result.Spec.Template.Spec.Containers[i].Image = partsOfString[0] + "@" + mcContainerImageTag
					} else {
						result.Spec.Template.Spec.Containers[i].Image = partsOfString[0] + ":" + mcContainerImageTag
					}
				}
			} else {
				if len(mcmContainerImageTag) != 0 {
					// Add/reduce machine-safety-overshooting-period for freeze check to succeed
					var isOptionAvailable bool
					for option := range result.Spec.Template.Spec.Containers[i].Command {
						if strings.Contains(result.Spec.Template.Spec.Containers[i].Command[option], "machine-safety-overshooting-period=") {
							isOptionAvailable = true
							result.Spec.Template.Spec.Containers[i].Command[option] = "--machine-safety-overshooting-period=300ms"
						}
					}
					if !isOptionAvailable {
						result.Spec.Template.Spec.Containers[i].Command = append(result.Spec.Template.Spec.Containers[i].Command, "--machine-safety-overshooting-period=300ms")
					}
					if strings.Contains(mcContainerImageTag, "sha256") {
						result.Spec.Template.Spec.Containers[i].Image = partsOfString[0] + "@" + mcmContainerImageTag
					} else {
						result.Spec.Template.Spec.Containers[i].Image = partsOfString[0] + ":" + mcmContainerImageTag
					}
				}
			}
		}
		_, updateErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(controlClusterNamespace).Update(result)
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
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		result, getErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(controlClusterNamespace).Get("machine-controller-manager", metav1.GetOptions{})
		if getErr != nil {
			//panic(fmt.Errorf("failed to get latest version of Deployment: %v", getErr))
			return getErr
		}
		mcmDeploymentOrigObj = *result
		*result.Spec.Replicas = replicas
		_, updateErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(controlClusterNamespace).Update(result)
		return updateErr
	})
	return retryErr
}

func (c *IntegrationTestFramework) setupMachineClass() error {
	/* TO-DO: createDummyMachineClass
	 This will read the control cluster machineclass resource and creates a duplicate of it
	 it will additionally add the delta part found in machineclass yaml file

	 - (if not use machine-class.yaml file)
			 look for a file available in kubernetes directory of provider specific repo and then use it instead for creating machine class

	*/

	machineClasses, err := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	var newMachineClass *v1alpha1.MachineClass
	machineClass := machineClasses.Items[0]

	// Create machine-class using yaml and any of existing machineclass resource combined
	for _, resource_name := range testMachineClassResources {
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			result, getErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Get(machineClass.GetName(), metav1.GetOptions{})
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
			_, createErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Create(newMachineClass)
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
			_, patchErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Patch(newMachineClass.Name, types.MergePatchType, data)
			return patchErr
		})
		if retryErr != nil {
			return retryErr
		}
	}
	return nil
}

func (c *IntegrationTestFramework) SetupBeforeSuite() {
	/*Check control cluster and target clusters are accessible
	- Check and create crds ( machineclass, machines, machinesets and machinedeployment ) if required
	using file available in kubernetes/crds directory of machine-controller-manager repo
	- Start the Machine Controller manager and machine controller (provider-specific)
	- Assume secret resource for accesing the cloud provider service in already in the control cluster
	- Create machineclass resource from file available in kubernetes directory of provider specific repo in control cluster
	*/
	log.SetOutput(ginkgo.GinkgoWriter)
	mcContainerImageTag := os.Getenv("mcContainerImage")
	mcmContainerImageTag := os.Getenv("mcmContainerImage")
	ginkgo.By("Checking for the clusters if provided are available")
	gomega.Expect(c.initalizeClusters()).To(gomega.BeNil())

	// preparing resources
	if !c.ControlKubeCluster.IsSeed(c.TargetKubeCluster) {

		ginkgo.By("Cloning Machine-Controller-Manager github repo")
		gomega.Expect(helpers.CloneRepo("https://github.com/gardener/machine-controller-manager.git", mcmRepoPath)).To(gomega.BeNil())

		//create the custom resources in the control cluster using yaml files
		//available in kubernetes/crds directory of machine-controller-manager repo
		//resources to be applied are machineclass, machines, machinesets and machinedeployment
		ginkgo.By("Applying kubernetes/crds into control cluster")
		gomega.Expect(c.ControlKubeCluster.ApplyFiles(filepath.Join(mcmRepoPath, "kubernetes/crds"), controlClusterNamespace)).To(gomega.BeNil())

		//  - if isControlClusterIsShootsSeed is true, then use machineclass from cluster
		// 	 probe for machine-class in the identified namespace and then creae a copy of this machine-class with additional delta available in machineclass-delta.yaml ( eg. tag (providerSpec.tags)  \"mcm-integration-test: "true"\" )
		// 	  --- (Obsolete ?) ---> the namespace of the new machine-class should be default
		ginkgo.By("Applying MachineClass")
		gomega.Expect(c.ControlKubeCluster.ApplyFiles("../../../kubernetes/machine-class.yaml", controlClusterNamespace)).To(gomega.BeNil())
	} else {
		// If no tags specified then - applyCrds from the mcm repo by cloning
		if !(len(mcContainerImageTag) != 0 && len(mcmContainerImageTag) != 0) {
			ginkgo.By("Cloning Machine-Controller-Manager github repo")
			gomega.Expect(helpers.CloneRepo("https://github.com/gardener/machine-controller-manager.git", mcmRepoPath)).To(gomega.BeNil())

			//create the custom resources in the control cluster using yaml files
			//available in kubernetes/crds directory of machine-controller-manager repo
			//resources to be applied are machineclass, machines, machinesets and machinedeployment
			ginkgo.By("Applying kubernetes/crds into control cluster")
			gomega.Expect(c.ControlKubeCluster.ApplyFiles(filepath.Join(mcmRepoPath, "kubernetes/crds"), controlClusterNamespace)).To(gomega.BeNil())
		}
		ginkgo.By("Creating dup MachineClass with delta yaml")
		gomega.Expect(c.setupMachineClass()).To(gomega.BeNil())
	}

	// starting controllers
	if len(mcContainerImageTag) != 0 && len(mcmContainerImageTag) != 0 {

		/* - if any of mcmContainerImage  or mcContainerImageTag flag is non-empty then,
		create/update machinecontrollermanager deployment in the control-cluster with specified image
		- crds already exist in the cluster.
		TO-DO: try to look for crds in local kubernetes directory and apply them. this validates changes in crd structures (if any)
		*/
		if c.ControlKubeCluster.IsSeed(c.TargetKubeCluster) {
			ginkgo.By("Updating MCM Deployemnt")
			gomega.Expect(c.prepareMcmDeployment(mcContainerImageTag, mcmContainerImageTag, false)).To(gomega.BeNil())
		} else {
			ginkgo.By("Creating MCM Deployemnt")
			gomega.Expect(c.prepareMcmDeployment(mcContainerImageTag, mcmContainerImageTag, true)).To(gomega.BeNil())
		}

	} else {
		/*
			- as mcmContainerImage is empty, run mc and mcm locally
		*/
		if c.ControlKubeCluster.IsSeed(c.TargetKubeCluster) {
			ginkgo.By("Scaledown existing machine controllers")
			gomega.Expect(c.scaleMcmDeployment(0)).To(gomega.BeNil())
		}

		ginkgo.By("Starting Machine Controller ")
		directory := "../../.."
		makeCommandMc := fmt.Sprintf("make --directory=%s start CONTROL_KUBECONFIG=%s TARGET_KUBECONFIG=%s CONTROL_NAMESPACE=%s LEADER_ELECT=false ",
			directory,
			c.ControlKubeCluster.KubeConfigFilePath,
			c.TargetKubeCluster.KubeConfigFilePath,
			controlClusterNamespace)
		args := strings.Fields(makeCommandMc)
		command := exec.Command(args[0], args[1:]...)
		helpers.Rotate(filepath.Join(targetDir, mcLogFileName))
		outputFile, _ := os.Create(filepath.Join(targetDir, mcLogFileName))
		sess, err := gexec.Start(command, outputFile, outputFile)
		mcsession = sess
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		gomega.Expect(mcsession.ExitCode()).Should(gomega.Equal(-1))

		ginkgo.By("Starting Machine Controller Manager")
		directory = mcmRepoPath
		makeCommandMcm := fmt.Sprintf("make --directory=%s start CONTROL_KUBECONFIG=%s TARGET_KUBECONFIG=%s CONTROL_NAMESPACE=%s LEADER_ELECT=false MACHINE_SAFETY_OVERSHOOTING_PERIOD=300ms",
			directory,
			c.ControlKubeCluster.KubeConfigFilePath,
			c.TargetKubeCluster.KubeConfigFilePath,
			controlClusterNamespace)
		args = strings.Fields(makeCommandMcm)
		command = exec.Command(args[0], args[1:]...)
		helpers.Rotate(filepath.Join(targetDir, mcmLogFileName))
		outputFile, _ = os.Create(filepath.Join(targetDir, mcmLogFileName))
		mcmsession, err = gexec.Start(command, outputFile, outputFile)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		gomega.Expect(mcmsession.ExitCode()).Should(gomega.Equal(-1))
	}

	// initialize orphan resource tracker
	ginkgo.By("looking for machineclass resource in the control cluster")
	machineClass, err := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Get(testMachineClassResources[0], metav1.GetOptions{})
	if err == nil {
		secret, err := c.ControlKubeCluster.Clientset.CoreV1().Secrets(machineClass.SecretRef.Namespace).Get(machineClass.SecretRef.Name, metav1.GetOptions{})
		ginkgo.By("looking for secret resource refered in machineclass in the control cluster")
		if err == nil {
			ginkgo.By("determining control cluster name")
			clusterName, err := c.ControlKubeCluster.ClusterName()
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
		gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 180, 5).Should(gomega.BeNumerically("==", c.TargetKubeCluster.NumberOfNodes()))
	})
}

func (c *IntegrationTestFramework) ControllerTests() {
	// Testcase #01 | Machine
	ginkgo.Describe("Machine Resource", func() {
		var initialNodes int16
		ginkgo.Context("Creation", func() {
			// Probe nodes currently available in target cluster
			ginkgo.It("should not lead to any errors and add 1 more node in target cluste", func() {
				// apply machine resource yaml file
				initialNodes = c.TargetKubeCluster.NumberOfNodes()
				ginkgo.By("checking for errors")
				gomega.Expect(c.ControlKubeCluster.ApplyFiles("../../../kubernetes/machine.yaml", controlClusterNamespace)).To(gomega.BeNil())
				//fmt.Println("wait for 30 sec before probing for nodes")

				// check whether there is one node more
				ginkgo.By("Waiting until number of ready nodes is 1 more than initial nodes")
				gomega.Eventually(c.TargetKubeCluster.NumberOfNodes, 600, 5).Should(gomega.BeNumerically("==", initialNodes+1))
				gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 600, 5).Should(gomega.BeNumerically("==", initialNodes+1))
			})
		})

		ginkgo.Context("Deletion", func() {
			ginkgo.Context("When machines available", func() {
				ginkgo.It("should not lead to errors and remove 1 node in target cluster", func() {
					machinesList, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).List(metav1.ListOptions{})
					if len(machinesList.Items) != 0 {
						ginkgo.By("checking for errors")
						gomega.Expect(c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).Delete("test-machine", &metav1.DeleteOptions{})).Should(gomega.BeNil(), "No Errors while deleting machine")

						ginkgo.By("Waiting until number of ready nodes is eual to number of initial  nodes")
						gomega.Eventually(c.TargetKubeCluster.NumberOfNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes))
						gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes))
					}

				})
			})
			ginkgo.Context("when machines are not available", func() {
				// delete one machine (non-existent) by random text as name of resource
				// check there are no changes to nodes

				ginkgo.It("should keep nodes intact", func() {
					// Keep count of nodes available
					// delete machine resource
					machinesList, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).List(metav1.ListOptions{})
					if len(machinesList.Items) == 0 {
						err := c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).Delete("test-machine-dummy", &metav1.DeleteOptions{})
						ginkgo.By("checking for errors")
						gomega.Expect(err).To(gomega.HaveOccurred())
						time.Sleep(30 * time.Second)
						ginkgo.By("Checking number of ready nodes is eual to number of initial nodes")
						gomega.Expect(c.TargetKubeCluster.NumberOfNodes()).To(gomega.BeEquivalentTo(initialNodes))
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
				initialNodes = c.TargetKubeCluster.NumberOfNodes()

				// apply machinedeployment resource yaml file
				ginkgo.By("checking for errors")
				gomega.Expect(c.ControlKubeCluster.ApplyFiles("../../../kubernetes/machine-deployment.yaml", controlClusterNamespace)).To(gomega.BeNil())

				// check whether all the expected nodes are ready
				ginkgo.By("Waiting until number of ready nodes are 3 more than initial")
				gomega.Eventually(c.TargetKubeCluster.NumberOfNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes+3))
				gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes+3))
			})
		})
		ginkgo.Context("scale-up with replicas=6", func() {
			ginkgo.It("should not lead to errors and add futher 3 nodes to target cluster", func() {

				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Replicas = 6
					_, updateErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Update(machineDployment)
					return updateErr
				})
				ginkgo.By("checking for errors")
				gomega.Expect(retryErr).NotTo(gomega.HaveOccurred())
				// check whether all the expected nodes are ready
				ginkgo.By("checking number of ready nodes are 6 more than initial")
				gomega.Eventually(c.TargetKubeCluster.NumberOfNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes+6))
				gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes+6))
			})

		})
		ginkgo.Context("scale-down with replicas=2", func() {
			// rapidly scaling back to 2 leading to a freezing and unfreezing
			// check for freezing and unfreezing of machine due to rapid scale up and scale down in the logs of mcm

			ginkgo.It("Should not lead to errors and remove 4 nodes from target cluster", func() {
				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Replicas = 2
					_, updateErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Update(machineDployment)
					return updateErr
				})
				ginkgo.By("checking for errors")
				gomega.Expect(retryErr).NotTo(gomega.HaveOccurred())

				ginkgo.By("checking number of ready nodes are 2 more than initial")
				gomega.Eventually(c.TargetKubeCluster.NumberOfNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes+2))
				gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes+2))
			})
			ginkgo.It("should freeze and unfreeze machineset temporarily", func() {
				if mcsession == nil {
					// controllers running in pod
					// Create log file from container log
					helpers.Rotate(filepath.Join(targetDir, mcmLogFileName))
					outputFile, _ := os.Create(filepath.Join(targetDir, mcmLogFileName))
					ginkgo.By("reading containerlog is not erroring")
					mcmPod, err := c.ControlKubeCluster.Clientset.CoreV1().Pods(controlClusterNamespace).List(metav1.ListOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					readCloser, err := c.ControlKubeCluster.Clientset.CoreV1().
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
					data, _ := ioutil.ReadFile(filepath.Join(targetDir, mcmLogFileName))
					return frozeRegexp.Match(data)
				}, 300, 5).Should(gomega.BeTrue())

				ginkgo.By("Searching Unfroze in mcm log file")
				unfrozeRegexp, _ := regexp.Compile(` Unfroze MachineSet`)
				gomega.Eventually(func() bool {
					data, _ := ioutil.ReadFile(filepath.Join(targetDir, mcmLogFileName))
					return unfrozeRegexp.Match(data)
				}, 300, 5).Should(gomega.BeTrue())
			})
		})
		ginkgo.Context("Updation to v2 machine-class and replicas=4", func() {
			// update machine type -> machineDeployment.spec.template.spec.class.name = "test-mc-dummy"
			// scale up replicas by 4
			// To-Do: Add check for rolling update completion (updatedReplicas check)
			ginkgo.It("should upgrade machines and add more nodes to target", func() {
				// wait for 2400s till machines updates
				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Template.Spec.Class.Name = testMachineClassResources[1]
					machineDployment.Spec.Replicas = 4
					_, updateErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Update(machineDployment)
					return updateErr
				})
				//Check there is no error occured
				ginkgo.By("checking for errors")
				gomega.Expect(retryErr).NotTo(gomega.HaveOccurred())
				ginkgo.By("updatedReplicas to be 4")
				gomega.Eventually(c.ControlKubeCluster.GetUpdatedReplicasCount("test-machine-deployment", controlClusterNamespace), 900, 5).Should(gomega.BeNumerically("==", 4))
				ginkgo.By("number of ready nodes be 4 more")
				gomega.Eventually(c.TargetKubeCluster.NumberOfNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes+4))
				gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes+4))

			})
		})
		ginkgo.Context("Deletion", func() {
			ginkgo.Context("When there are machine deployment(s) available in control cluster", func() {
				ginkgo.It("should not lead to errors and list only initial nodes", func() {
					_, err := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					if err == nil {
						//delete machine resource
						ginkgo.By("checking for errors")
						gomega.Expect(c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Delete("test-machine-deployment", &metav1.DeleteOptions{})).Should(gomega.BeNil())
						ginkgo.By("Waiting until number of ready nodes is eual to number of initial  nodes")
						gomega.Eventually(c.TargetKubeCluster.NumberOfNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes))
						gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes))
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

func (c *IntegrationTestFramework) Cleanup() {

	//running locally
	if mcsession != nil {
		if mcsession.ExitCode() != -1 {
			ginkgo.By("Restarting Machine Controller ")
			helpers.Rotate(filepath.Join(targetDir, mcLogFileName))
			outputFile, _ := os.Create(filepath.Join(targetDir, mcLogFileName))
			gexec.Start(mcsession.Command, outputFile, outputFile)
		}
		if mcmsession.ExitCode() != -1 {
			ginkgo.By("Restarting Machine Controller Manager")
			helpers.Rotate(filepath.Join(targetDir, mcmLogFileName))
			outputFile, _ := os.Create(filepath.Join(targetDir, mcmLogFileName))
			gexec.Start(mcmsession.Command, outputFile, outputFile)
		}
	}

	if c.ControlKubeCluster.McmClient != nil {
		timeout := int64(900)
		// Check and delete machinedeployment resource
		_, err := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
		if err == nil {
			log.Println("deleting test-machine-deployment")
			watchMachinesDepl, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Watch(metav1.ListOptions{TimeoutSeconds: &timeout}) //ResourceVersion: machineDeploymentObj.ResourceVersion
			for event := range watchMachinesDepl.ResultChan() {
				c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).Delete("test-machine-deployment", &metav1.DeleteOptions{})
				if event.Type == watch.Deleted {
					watchMachinesDepl.Stop()
					log.Println("machinedeployment deleted")
				}
			}
		} else {
			log.Println(err.Error())
		}
		// Check and delete machine resource
		_, err = c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).Get("test-machine", metav1.GetOptions{})
		if err == nil {
			log.Println("deleting test-machine")
			watchMachines, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).Watch(metav1.ListOptions{TimeoutSeconds: &timeout}) //ResourceVersion: machineObj.ResourceVersion
			for event := range watchMachines.ResultChan() {
				c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(controlClusterNamespace).Delete("test-machine", &metav1.DeleteOptions{})
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
			_, err = c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Get(machineClassName, metav1.GetOptions{})
			if err == nil {
				log.Printf("deleting %s machineclass", machineClassName)
				watchMachineClass, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Watch(metav1.ListOptions{TimeoutSeconds: &timeout}) //ResourceVersion: machineObj.ResourceVersion
				for event := range watchMachineClass.ResultChan() {
					c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(controlClusterNamespace).Delete(machineClassName, &metav1.DeleteOptions{})
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
	if c.ControlKubeCluster.IsSeed(c.TargetKubeCluster) {
		retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Retrieve the latest version of Deployment before attempting update
			// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
			_, updateErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(mcmDeploymentOrigObj.Namespace).Update(&(mcmDeploymentOrigObj))
			return updateErr
		})
	} else {
		if len(os.Getenv("mcContainerImage")) != 0 && len(os.Getenv("mcmContainerImage")) != 0 {
			c.ControlKubeCluster.ClusterRolesAndRoleBindingCleanup()
			c.TargetKubeCluster.ClusterRolesAndRoleBindingCleanup()
			c.ControlKubeCluster.Clientset.CoreV1().Secrets(controlClusterNamespace).Delete("machine-controller-manager-target", &metav1.DeleteOptions{})
			c.ControlKubeCluster.Clientset.AppsV1().Deployments(controlClusterNamespace).Delete("machine-controller-manager", &metav1.DeleteOptions{})
		}
	}
}
