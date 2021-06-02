package common

import (
	"context"
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

var (
	runControllersLocally bool
)

type IntegrationTestFramework struct {
	ControlKubeCluster        *helpers.Cluster
	TargetKubeCluster         *helpers.Cluster
	mcmRepoPath               string
	ctx                       context.Context
	cancelFunc                context.CancelFunc
	mcmLogFileName            string
	mcLogFileName             string
	mcmDeploymentOrigObj      appsV1.Deployment
	controlClusterNamespace   string
	testMachineClassResources []string
	resourcesTracker          helpers.ResourcesTrackerInterface
	targetDir                 string
	crdsPath                  string
	makeCommandMcm            string
	makeCommandMc             string
	mcmsession, mcsession     *gexec.Session
}

func NewIntegrationTestFramework(resourcesTracker helpers.ResourcesTrackerInterface) (c *IntegrationTestFramework) {
	c = &IntegrationTestFramework{
		mcmRepoPath:               "../../../dev/mcm",
		targetDir:                 os.TempDir(),
		mcmLogFileName:            "mcm_process.log",
		mcLogFileName:             "mc_process.log",
		controlClusterNamespace:   os.Getenv("controlClusterNamespace"),
		testMachineClassResources: []string{"test-mc", "test-mc-dummy"},
		//numberOfBgProcesses:       0,
		resourcesTracker: resourcesTracker,
		crdsPath:         "../../../dev/mcm/kubernetes/crds",
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
	c.ctx, c.cancelFunc = context.WithCancel(context.Background())
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
	} else if c.TargetKubeCluster.KubeConfigFilePath != "" {
		return fmt.Errorf("controlKubeconfig path is mandatory if using c.targetKubeConfigPath. Aborting")
	}

	if c.controlClusterNamespace == "" {
		c.controlClusterNamespace = "default"
	}
	if c.ControlKubeCluster.IsSeed(c.TargetKubeCluster) {
		_, err := c.TargetKubeCluster.ClusterName()
		if err != nil {
			log.Println("Failed to determine shoot cluster namespace")
			return err
		}
		c.controlClusterNamespace, _ = c.TargetKubeCluster.ClusterName()
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
		c.ControlKubeCluster.Clientset.CoreV1().Secrets(c.controlClusterNamespace).Create(&coreV1.Secret{
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
				c.controlClusterNamespace)
		if err != nil {
			return err
		}
		// once created, the machine-deployment resource container image tags will be updated by continuing here
	}
	providerSpecificRegexp, _ := regexp.Compile("machine-controller-manager-provider-")
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		result, getErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(c.controlClusterNamespace).Get("machine-controller-manager", metav1.GetOptions{})
		if getErr != nil {
			log.Printf("failed to get latest version of Deployment: %v", getErr)
			return getErr
		}
		c.mcmDeploymentOrigObj = *result
		for i := range result.Spec.Template.Spec.Containers {
			isProviderSpecific := providerSpecificRegexp.Match([]byte(result.Spec.Template.Spec.Containers[i].Name))
			partsOfString := strings.Split(result.Spec.Template.Spec.Containers[i].Image, ":")
			if isProviderSpecific {
				if len(mcContainerImageTag) != 0 {
					result.Spec.Template.Spec.Containers[i].Image = partsOfString[0] + ":" + mcContainerImageTag
					// result.Spec.Template.Spec.Containers[i].Image = "eu.gcr.io/gardener-project/gardener/machine-controller-manager-provider-aws:" + mcContainerImageTag
				}
			} else {
				if len(mcmContainerImageTag) != 0 {
					var optUpdate bool
					for option := range result.Spec.Template.Spec.Containers[i].Command {
						if strings.Contains(result.Spec.Template.Spec.Containers[i].Command[option], "machine-safety-overshooting-period=") {
							result.Spec.Template.Spec.Containers[i].Command[option] = "--machine-safety-overshooting-period=300ms"
							optUpdate = true
						}
					}
					if !optUpdate {
						result.Spec.Template.Spec.Containers[i].Command = append(result.Spec.Template.Spec.Containers[i].Command, "--machine-safety-overshooting-period=300ms")
					}
					result.Spec.Template.Spec.Containers[i].Image = partsOfString[0] + ":" + mcmContainerImageTag
					// result.Spec.Template.Spec.Containers[i].Image = "eu.gcr.io/gardener-project/gardener/machine-controller-manager:" + mcmContainerImageTag
				}
			}
		}
		_, updateErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(c.controlClusterNamespace).Update(result)
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
		result, getErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(c.controlClusterNamespace).Get("machine-controller-manager", metav1.GetOptions{})
		if getErr != nil {
			//panic(fmt.Errorf("failed to get latest version of Deployment: %v", getErr))
			return getErr
		}
		c.mcmDeploymentOrigObj = *result
		*result.Spec.Replicas = replicas
		_, updateErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(c.controlClusterNamespace).Update(result)
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

	machineClasses, err := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(c.controlClusterNamespace).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	var newMachineClass *v1alpha1.MachineClass
	machineClass := machineClasses.Items[0]

	// Create machine-class using yaml and any of existing machineclass resource combined
	for _, resource_name := range c.testMachineClassResources {
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			result, getErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(c.controlClusterNamespace).Get(machineClass.GetName(), metav1.GetOptions{})
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
			_, createErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(c.controlClusterNamespace).Create(newMachineClass)
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
			_, patchErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(c.controlClusterNamespace).Patch(newMachineClass.Name, types.MergePatchType, data)
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
		gomega.Expect(helpers.CloneRepo("https://github.com/gardener/machine-controller-manager.git", c.mcmRepoPath)).To(gomega.BeNil())

		//create the custom resources in the control cluster using yaml files
		//available in kubernetes/crds directory of machine-controller-manager repo
		//resources to be applied are machineclass, machines, machinesets and machinedeployment
		ginkgo.By("Applying kubernetes/crds into control cluster")
		gomega.Expect(c.ControlKubeCluster.ApplyFiles(c.crdsPath, c.controlClusterNamespace)).To(gomega.BeNil())

		//  - if isControlClusterIsShootsSeed is true, then use machineclass from cluster
		// 	 probe for machine-class in the identified namespace and then creae a copy of this machine-class with additional delta available in machineclass-delta.yaml ( eg. tag (providerSpec.tags)  \"mcm-integration-test: "true"\" )
		// 	  --- (Obsolete ?) ---> the namespace of the new machine-class should be default
		ginkgo.By("Applying MachineClass")
		gomega.Expect(c.ControlKubeCluster.ApplyFiles("../../../kubernetes/machine-class.yaml", c.controlClusterNamespace)).To(gomega.BeNil())
	} else {
		// If no tags specified then - applyCrds from the mcm repo by cloning
		if !(len(mcContainerImageTag) != 0 && len(mcmContainerImageTag) != 0) {
			ginkgo.By("Cloning Machine-Controller-Manager github repo")
			gomega.Expect(helpers.CloneRepo("https://github.com/gardener/machine-controller-manager.git", c.mcmRepoPath)).To(gomega.BeNil())

			//create the custom resources in the control cluster using yaml files
			//available in kubernetes/crds directory of machine-controller-manager repo
			//resources to be applied are machineclass, machines, machinesets and machinedeployment
			ginkgo.By("Applying kubernetes/crds into control cluster")
			gomega.Expect(c.ControlKubeCluster.ApplyFiles(c.crdsPath, c.controlClusterNamespace)).To(gomega.BeNil())
		}
		ginkgo.By("Creating dup MachineClass with delta yaml")
		gomega.Expect(c.setupMachineClass()).To(gomega.BeNil())
	}

	// starting controllers
	if len(mcContainerImageTag) != 0 && len(mcmContainerImageTag) != 0 {
		log.Println("length is ", len(mcContainerImageTag))
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
		runControllersLocally = true
		if c.ControlKubeCluster.IsSeed(c.TargetKubeCluster) {
			ginkgo.By("Scaledown existing machine controllers")
			gomega.Expect(c.scaleMcmDeployment(0)).To(gomega.BeNil())
		}

		ginkgo.By("Starting Machine Controller ")
		directory := "../../.."
		c.makeCommandMc = fmt.Sprintf("make --directory=%s start CONTROL_KUBECONFIG=%s TARGET_KUBECONFIG=%s CONTROL_NAMESPACE=%s LEADER_ELECT=false ",
			directory,
			c.ControlKubeCluster.KubeConfigFilePath,
			c.TargetKubeCluster.KubeConfigFilePath,
			c.controlClusterNamespace)
		args := strings.Fields(c.makeCommandMc)
		command := exec.Command(args[0], args[1:]...)
		helpers.Rotate(filepath.Join(c.targetDir, c.mcLogFileName))
		outputFile, _ := os.Create(filepath.Join(c.targetDir, c.mcLogFileName))
		sess, err := gexec.Start(command, outputFile, outputFile)
		c.mcsession = sess
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		gomega.Expect(c.mcsession.ExitCode()).Should(gomega.Equal(-1))

		//gomega.Expect(helpers.StartCommandAsRoutine(c.ctx, c.makeCommandMc, "../../..", filepath.Join(c.targetDir, c.mcLogFileName), &c.wg, exitChannel)).To(gomega.BeNil())
		//gomega.Expect(helpers.StartCommandAsRoutine(c.ctx, c.makeCommandMc, "../../..", filepath.Join(c.targetDir, c.mcLogFileName), &c.wg, exitChannel)).To(gomega.BeNil())

		ginkgo.By("Starting Machine Controller Manager")
		directory = c.mcmRepoPath
		c.makeCommandMcm = fmt.Sprintf("make --directory=%s start CONTROL_KUBECONFIG=%s TARGET_KUBECONFIG=%s CONTROL_NAMESPACE=%s LEADER_ELECT=false MACHINE_SAFETY_OVERSHOOTING_PERIOD=300ms",
			directory,
			c.ControlKubeCluster.KubeConfigFilePath,
			c.TargetKubeCluster.KubeConfigFilePath,
			c.controlClusterNamespace)
		args = strings.Fields(c.makeCommandMcm)
		command = exec.Command(args[0], args[1:]...)
		helpers.Rotate(filepath.Join(c.targetDir, c.mcmLogFileName))
		outputFile, _ = os.Create(filepath.Join(c.targetDir, c.mcmLogFileName))
		c.mcmsession, err = gexec.Start(command, outputFile, outputFile)
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		gomega.Expect(c.mcmsession.ExitCode()).Should(gomega.Equal(-1))

		// gomega.Expect(helpers.StartCommandAsRoutine(c.ctx, c.makeCommandMcm, c.mcmRepoPath, filepath.Join(c.targetDir, c.mcmLogFileName), &c.wg, exitChannel)).To(gomega.BeNil())
		// gomega.Expect(helpers.StartCommandAsRoutine(c.ctx, c.makeCommandMcm, c.mcmRepoPath, filepath.Join(c.targetDir, c.mcmLogFileName), &c.wg, exitChannel)).To(gomega.BeNil())
	}

	// initialize orphan resource tracker
	ginkgo.By("looking for machineclass resource in the control cluster")
	machineClass, err := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(c.controlClusterNamespace).Get(c.testMachineClassResources[0], metav1.GetOptions{})
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
			gomega.Expect(c.mcsession.ExitCode()).Should(gomega.Equal(-1))
			ginkgo.By("Checking machineControllerManager process is running")
			gomega.Expect(c.mcmsession.ExitCode()).Should(gomega.Equal(-1))
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
			ginkgo.It("should not lead to any errors", func() {
				// apply machine resource yaml file
				initialNodes = c.TargetKubeCluster.NumberOfNodes()
				ginkgo.By("checking for errors")
				gomega.Expect(c.ControlKubeCluster.ApplyFiles("../../../kubernetes/machine.yaml", c.controlClusterNamespace)).To(gomega.BeNil())
				//fmt.Println("wait for 30 sec before probing for nodes")
			})
			ginkgo.It("should add 1 more node in target cluster", func() {
				// check whether there is one node more
				ginkgo.By("Waiting until number of ready nodes is 1 more than initial nodes")
				gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 600, 5).Should(gomega.BeNumerically("==", initialNodes+1))
			})
		})

		ginkgo.Context("Deletion", func() {
			ginkgo.Context("When machines available", func() {
				ginkgo.It("should not lead to errors", func() {
					machinesList, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(c.controlClusterNamespace).List(metav1.ListOptions{})
					if len(machinesList.Items) != 0 {
						ginkgo.By("checking for errors")
						gomega.Expect(c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(c.controlClusterNamespace).Delete("test-machine", &metav1.DeleteOptions{})).Should(gomega.BeNil(), "No Errors while deleting machine")
					}
				})
				ginkgo.It("should remove 1 node in target cluster", func() {
					ginkgo.By("Waiting until number of ready nodes is eual to number of initial  nodes")
					gomega.Eventually(c.TargetKubeCluster.NumberOfNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes))
				})
			})
			ginkgo.Context("when machines are not available", func() {
				// delete one machine (non-existent) by random text as name of resource
				// check there are no changes to nodes

				ginkgo.It("should keep nodes intact", func() {
					// Keep count of nodes available
					// delete machine resource
					machinesList, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(c.controlClusterNamespace).List(metav1.ListOptions{})
					if len(machinesList.Items) == 0 {
						err := c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(c.controlClusterNamespace).Delete("test-machine-dummy", &metav1.DeleteOptions{})
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
			ginkgo.It("should not lead to errors", func() {
				//probe initialnodes before continuing
				initialNodes = c.TargetKubeCluster.NumberOfNodes()

				// apply machinedeployment resource yaml file
				ginkgo.By("checking for errors")
				gomega.Expect(c.ControlKubeCluster.ApplyFiles("../../../kubernetes/machine-deployment.yaml", c.controlClusterNamespace)).To(gomega.BeNil())
			})
			ginkgo.It("should add 3 more nodes to target cluster", func() {
				// check whether all the expected nodes are ready
				ginkgo.By("Waiting until number of ready nodes are 3 more than initial")
				gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes+3))
			})
		})
		ginkgo.Context("scale-up with replicas=6", func() {
			ginkgo.It("should not lead to errors", func() {

				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Replicas = 6
					_, updateErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Update(machineDployment)
					return updateErr
				})
				ginkgo.By("checking for errors")
				gomega.Expect(retryErr).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It("should add futher 3 nodes to target cluster", func() {
				// check whether all the expected nodes are ready
				ginkgo.By("checking number of ready nodes are 6 more than initial")
				gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 180, 5).Should(gomega.BeNumerically("==", initialNodes+6))
			})

		})
		ginkgo.Context("scale-down with replicas=2", func() {
			// rapidly scaling back to 2 leading to a freezing and unfreezing
			// check for freezing and unfreezing of machine due to rapid scale up and scale down in the logs of mcm

			ginkgo.It("Should not lead to errors", func() {
				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Replicas = 2
					_, updateErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Update(machineDployment)
					return updateErr
				})
				ginkgo.By("checking for errors")
				gomega.Expect(retryErr).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It("should freeze and unfreeze machineset temporarily", func() {
				if runControllersLocally {
					//ginkgo.By("Searching Froze in the logs")
					// gomega.Expect(c.mcmsession).Should(ContainSubstring(` Froze MachineSet`))
					//gomega.Eventually(c.mcmsession.Out, 300, 5).Should(gbytes.Say(" Froze MachineSet"))
					ginkgo.By("Searching for Froze in mcm log file")
					frozeRegexp, _ := regexp.Compile(` Froze MachineSet`)
					gomega.Eventually(func() bool {
						data, _ := ioutil.ReadFile(filepath.Join(c.targetDir, c.mcmLogFileName))
						return frozeRegexp.Match(data)
					}, 300, 5).Should(gomega.BeTrue())

					//gomega.Expect(c.mcmsession.Out).Should(gbytes.Say()(` Froze MachineSet`))
					ginkgo.By("Searching Unfroze in mcm log file")
					//gomega.Eventually(gbytes.BufferReader(c.mcmsession.Out).Contents(), 300, 5).Should(ContainSubstring(` Unfroze MachineSet`))
					unfrozeRegexp, _ := regexp.Compile(` Unfroze MachineSet`)
					gomega.Eventually(func() bool {
						data, _ := ioutil.ReadFile(filepath.Join(c.targetDir, c.mcmLogFileName))
						return unfrozeRegexp.Match(data)
					}, 300, 5).Should(gomega.BeTrue())
				} else {
					helpers.Rotate(filepath.Join(c.targetDir, c.mcmLogFileName))
					outputFile, _ := os.Create(filepath.Join(c.targetDir, c.mcmLogFileName))
					ginkgo.By("reading containerlog is not erroring")
					mcmPod, err := c.ControlKubeCluster.Clientset.CoreV1().Pods(c.controlClusterNamespace).List(metav1.ListOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					readCloser, err := c.ControlKubeCluster.Clientset.CoreV1().
						Pods(c.controlClusterNamespace).
						GetLogs(mcmPod.Items[0].Name, &coreV1.PodLogOptions{
							Container: "machine-controller-manager",
						}).Stream()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					io.Copy(outputFile, readCloser)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// ginkgo.By("Searching Froze in the logs")
					// gomega.Eventually(gbytes.BufferReader(logs).Contents(), 300, 5).Should(ContainSubstring(` Froze MachineSet`))

					// ginkgo.By("Searching Unfroze in mcm log file")
					// gomega.Eventually(gbytes.BufferReader(logs).Contents(), 300, 5).Should(ContainSubstring(` Unfroze MachineSet`))
					ginkgo.By("Searching for Froze in mcm log file")
					frozeRegexp, _ := regexp.Compile(` Froze MachineSet`)
					gomega.Eventually(func() bool {
						data, _ := ioutil.ReadFile(filepath.Join(c.targetDir, c.mcmLogFileName))
						return frozeRegexp.Match(data)
					}, 300, 5).Should(gomega.BeTrue())

					//gomega.Expect(c.mcmsession.Out).Should(gbytes.Say()(` Froze MachineSet`))
					ginkgo.By("Searching Unfroze in mcm log file")
					//gomega.Eventually(gbytes.BufferReader(c.mcmsession.Out).Contents(), 300, 5).Should(ContainSubstring(` Unfroze MachineSet`))
					unfrozeRegexp, _ := regexp.Compile(` Unfroze MachineSet`)
					gomega.Eventually(func() bool {
						data, _ := ioutil.ReadFile(filepath.Join(c.targetDir, c.mcmLogFileName))
						return unfrozeRegexp.Match(data)
					}, 300, 5).Should(gomega.BeTrue())
				}
			})
			ginkgo.It("should remove 4 nodes from target cluster", func() {
				ginkgo.By("checking number of ready nodes are 2 more than initial")
				gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes+2))
			})
		})
		ginkgo.Context("Updation to v2 machine-class and replicas=4", func() {
			// update machine type -> machineDeployment.spec.template.spec.class.name = "test-mc-dummy"
			// scale up replicas by 4
			// To-Do: Add check for rolling update completion (updatedReplicas check)
			ginkgo.It("should upgrade machines and add more nodes to target", func() {
				// wait for 2400s till machines updates
				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Template.Spec.Class.Name = c.testMachineClassResources[1]
					machineDployment.Spec.Replicas = 4
					_, updateErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Update(machineDployment)
					return updateErr
				})
				//Check there is no error occured
				ginkgo.By("checking for errors")
				gomega.Expect(retryErr).NotTo(gomega.HaveOccurred())
				ginkgo.By("updatedReplicas to be 4")
				gomega.Eventually(c.ControlKubeCluster.GetUpdatedReplicasCount("test-machine-deployment", c.controlClusterNamespace), 1200, 5).Should(gomega.BeNumerically("==", 4))
				ginkgo.By("number of ready nodes be 4 more")
				gomega.Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 1200, 5).Should(gomega.BeNumerically("==", initialNodes+4))
			})
		})
		ginkgo.Context("Deletion", func() {
			ginkgo.Context("When there are machine deployment(s) available in control cluster", func() {
				ginkgo.It("should not lead to errors and list only initial nodes", func() {
					_, err := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					if err == nil {
						//delete machine resource
						ginkgo.By("checking for errors")
						gomega.Expect(c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Delete("test-machine-deployment", &metav1.DeleteOptions{})).Should(gomega.BeNil())
						ginkgo.By("Waiting until number of ready nodes is eual to number of initial  nodes")
						gomega.Eventually(c.TargetKubeCluster.NumberOfNodes, 300, 5).Should(gomega.BeNumerically("==", initialNodes))
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

	if runControllersLocally {
		if c.mcmsession.ExitCode() != -1 {
			ginkgo.By("Restarting Machine Controller ")
			args := strings.Fields(c.makeCommandMc)
			command := exec.Command(args[0], args[1:]...)
			helpers.Rotate(filepath.Join(c.targetDir, c.mcLogFileName))
			outputFile, _ := os.Create(filepath.Join(c.targetDir, c.mcLogFileName))
			gexec.Start(command, outputFile, outputFile)
		}
		if c.mcmsession.ExitCode() != -1 {
			ginkgo.By("Restarting Machine Controller Manager")
			args := strings.Fields(c.makeCommandMcm)
			command := exec.Command(args[0], args[1:]...)
			helpers.Rotate(filepath.Join(c.targetDir, c.mcmLogFileName))
			outputFile, _ := os.Create(filepath.Join(c.targetDir, c.mcmLogFileName))
			gexec.Start(command, outputFile, outputFile)
		}
	}

	if c.ControlKubeCluster.McmClient != nil {
		timeout := int64(900)
		// Check and delete machinedeployment resource
		_, err := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
		if err == nil {
			log.Println("deleting test-machine-deployment")
			watchMachinesDepl, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Watch(metav1.ListOptions{TimeoutSeconds: &timeout}) //ResourceVersion: machineDeploymentObj.ResourceVersion
			for event := range watchMachinesDepl.ResultChan() {
				c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Delete("test-machine-deployment", &metav1.DeleteOptions{})
				if event.Type == watch.Deleted {
					watchMachinesDepl.Stop()
					log.Println("machinedeployment deleted")
				}
			}
		} else {
			log.Println(err.Error())
		}
		// Check and delete machine resource
		_, err = c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(c.controlClusterNamespace).Get("test-machine", metav1.GetOptions{})
		if err == nil {
			log.Println("deleting test-machine")
			watchMachines, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(c.controlClusterNamespace).Watch(metav1.ListOptions{TimeoutSeconds: &timeout}) //ResourceVersion: machineObj.ResourceVersion
			for event := range watchMachines.ResultChan() {
				c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(c.controlClusterNamespace).Delete("test-machine", &metav1.DeleteOptions{})
				if event.Type == watch.Deleted {
					watchMachines.Stop()
					log.Println("machine deleted")
				}
			}
		} else {
			log.Println(err.Error())
		}

		for _, machineClassName := range c.testMachineClassResources {
			// Check and delete machine class resource
			_, err = c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(c.controlClusterNamespace).Get(machineClassName, metav1.GetOptions{})
			if err == nil {
				log.Printf("deleting %s machineclass", machineClassName)
				watchMachineClass, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(c.controlClusterNamespace).Watch(metav1.ListOptions{TimeoutSeconds: &timeout}) //ResourceVersion: machineObj.ResourceVersion
				for event := range watchMachineClass.ResultChan() {
					c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(c.controlClusterNamespace).Delete(machineClassName, &metav1.DeleteOptions{})
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
			_, updateErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(c.mcmDeploymentOrigObj.Namespace).Update(&(c.mcmDeploymentOrigObj))
			return updateErr
		})
	} else {
		if len(os.Getenv("mcContainerImage")) != 0 && len(os.Getenv("mcmContainerImage")) != 0 {
			c.ControlKubeCluster.ClusterRolesAndRoleBindingCleanup()
			c.TargetKubeCluster.ClusterRolesAndRoleBindingCleanup()
			c.ControlKubeCluster.Clientset.CoreV1().Secrets(c.controlClusterNamespace).Delete("machine-controller-manager-target", &metav1.DeleteOptions{})
			c.ControlKubeCluster.Clientset.AppsV1().Deployments(c.controlClusterNamespace).Delete("machine-controller-manager", &metav1.DeleteOptions{})
		}
	}
}
