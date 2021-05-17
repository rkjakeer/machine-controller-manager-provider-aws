package common

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	v1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	"github.com/gardener/machine-controller-manager/pkg/integrationtest/common/helpers"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

type ControllerTest struct {
	controlKubeConfigPath     string
	targetKubeConfigPath      string
	ControlKubeCluster        *helpers.Cluster
	TargetKubeCluster         *helpers.Cluster
	numberOfBgProcesses       int16
	mcmRepoPath               string
	ctx                       context.Context
	cancelFunc                context.CancelFunc
	wg                        sync.WaitGroup // prevents race condition between main and other goroutines exit
	mcm_logFile               string
	mc_logFile                string
	mcContainerImageTag       string
	mcmContainerImageTag      string
	mcmDeploymentOrigObj      v1.Deployment
	controlClusterNamespace   string
	testMachineClassResources []string
}

func NewContollerTest(controlKubeConfigPath string,
	targetKubeConfigPath string,
	mcmRepoPath string,
	logPath string,
	mcContainerImageTag string,
	mcmContainerImageTag string,
	controlClusterNamespace string) (c *ControllerTest) {
	c = &ControllerTest{
		controlKubeConfigPath:     controlKubeConfigPath,
		targetKubeConfigPath:      targetKubeConfigPath,
		mcmRepoPath:               mcmRepoPath,
		mcm_logFile:               filepath.Join(logPath, "integration-test-mcm.log"),
		mc_logFile:                filepath.Join(logPath, "integration-test-mc.log"),
		mcContainerImageTag:       mcContainerImageTag,
		mcmContainerImageTag:      mcmContainerImageTag,
		controlClusterNamespace:   controlClusterNamespace,
		testMachineClassResources: []string{"test-mc", "test-mc-dummy"},
		numberOfBgProcesses:       0,
	}
	return c
}

func (c *ControllerTest) prepareClusters() error {
	/* prepareClusters checks for
	- the validity of controlKubeConfig and targetKubeConfig flags
	- It should return an error if thre is a error
	*/
	c.ctx, c.cancelFunc = context.WithCancel(context.Background())
	log.Printf("Control path is %s\n", c.controlKubeConfigPath)
	log.Printf("Target path is %s\n", c.targetKubeConfigPath)
	if c.controlKubeConfigPath != "" {
		c.controlKubeConfigPath, _ = filepath.Abs(c.controlKubeConfigPath)
		// if control cluster config is available but not the target, then set control and target clusters as same
		if c.targetKubeConfigPath == "" {
			c.targetKubeConfigPath = c.controlKubeConfigPath
			log.Println("Missing targetKubeConfig. control cluster will be set as target too")
		}
		c.targetKubeConfigPath, _ = filepath.Abs(c.targetKubeConfigPath)
		// use the current context in controlkubeconfig
		var err error
		c.ControlKubeCluster, err = helpers.NewCluster(c.controlKubeConfigPath)
		if err != nil {
			return err
		}
		c.TargetKubeCluster, err = helpers.NewCluster(c.targetKubeConfigPath)
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
	} else if c.targetKubeConfigPath != "" {
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

func (c *ControllerTest) cloneMcmRepo() error {
	/* clones mcm repo locally.
	This is required if there is no mcm container image tag supplied or
	the clusters are not seed (control) and shoot (target) clusters
	*/
	// src := "https://github.com/gardener/machine-controller-manager.git"
	// helpers.CheckDst(c.mcmRepoPath)
	// err := helpers.CloningRepo(c.mcmRepoPath, src)
	// if err != nil {
	// 	return err
	// }
	return nil
}

func (c *ControllerTest) applyCrds() error {
	/* TO-DO: applyCrds will
	- create the custom resources in the controlKubeConfig
	- yaml files are available in kubernetes/crds directory of machine-controller-manager repo
	- resources to be applied are machineclass, machines, machinesets and machinedeployment
	*/

	err := c.cloneMcmRepo()
	if err != nil {
		return err
	}

	applyCrdsDirectory := fmt.Sprintf("%s/kubernetes/crds", c.mcmRepoPath)

	err = c.applyFiles(applyCrdsDirectory)
	if err != nil {
		return err
	}
	return nil
}

func (c *ControllerTest) startMachineControllerManager(ctx context.Context) error {
	/*
			 startMachineControllerManager starts the machine controller manager
					  clone the required repo and then use make


		TO-DO: Below error is appearing occasionally - We should avoid it

			 I0129 10:51:48.140615   33699 controller.go:508] Starting machine-controller-manager
			 I0129 10:57:19.893033   33699 leaderelection.go:287] failed to renew lease default/machine-controller-manager: failed to tryAcquireOrRenew context deadline exceeded
			 F0129 10:57:19.893084   33699 controllermanager.go:190] leaderelection lost
			 exit status 255
			 make: *** [start] Error 1
	*/
	command := fmt.Sprintf("make start CONTROL_KUBECONFIG=%s TARGET_KUBECONFIG=%s CONTROL_NAMESPACE=%s", c.controlKubeConfigPath, c.targetKubeConfigPath, c.controlClusterNamespace)
	log.Println("starting MachineControllerManager with command: ", command)
	c.wg.Add(1)
	go c.execCommandAsRoutine(ctx, command, c.mcmRepoPath, c.mcm_logFile)
	return nil
}

func (c *ControllerTest) startMachineController(ctx context.Context) error {
	/*
		  startMachineController starts the machine controller
			  - if mcContainerImage flag is non-empty then, start a pod in the control-cluster with specified image
			  - if mcContainerImage is empty, runs machine controller locally
	*/
	command := fmt.Sprintf("make start CONTROL_KUBECONFIG=%s TARGET_KUBECONFIG=%s CONTROL_NAMESPACE=%s", c.controlKubeConfigPath, c.targetKubeConfigPath, c.controlClusterNamespace)
	log.Println("starting MachineController with command: ", command)
	c.wg.Add(1)
	go c.execCommandAsRoutine(ctx, command, "../../..", c.mc_logFile)
	return nil
}

func (c *ControllerTest) initMcmDeployment() error {
	/*
		 - if any of mcmContainerImage  or mcContainerImageTag flag is non-empty then,
			 update machinecontrollermanager deployment in the control-cluster with specified image
		 -
	*/
	regObj, _ := regexp.Compile("machine-controller-manager-provider-")
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		result, getErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(c.controlClusterNamespace).Get("machine-controller-manager", metav1.GetOptions{})
		if getErr != nil {
			panic(fmt.Errorf("failed to get latest version of Deployment: %v", getErr))
		}
		c.mcmDeploymentOrigObj = *result
		for i := range result.Spec.Template.Spec.Containers {
			isProviderSpecific := regObj.Match([]byte(result.Spec.Template.Spec.Containers[i].Name))
			if isProviderSpecific {
				if len(c.mcContainerImageTag) != 0 {
					result.Spec.Template.Spec.Containers[i].Image = "eu.gcr.io/gardener-project/gardener/machine-controller-manager-provider-aws:" + c.mcContainerImageTag
				}
			} else {
				if len(c.mcmContainerImageTag) != 0 {
					result.Spec.Template.Spec.Containers[i].Image = "eu.gcr.io/gardener-project/gardener/machine-controller-manager:" + c.mcmContainerImageTag
				}
			}
		}
		_, updateErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(c.controlClusterNamespace).Update(result)
		return updateErr
	})
	if retryErr != nil {
		return retryErr
	} else {
		return nil
	}
}

func (c *ControllerTest) scaleMcmDeployment(replicas int32) error {
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
			panic(fmt.Errorf("failed to get latest version of Deployment: %v", getErr))
		}
		c.mcmDeploymentOrigObj = *result
		*result.Spec.Replicas = replicas
		_, updateErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(c.controlClusterNamespace).Update(result)
		return updateErr
	})
	if retryErr != nil {
		return retryErr
	} else {
		return nil
	}
}

func (c *ControllerTest) applyMachineClass() error {
	/*
		 - if isControlClusterIsShootsSeed is true, then use machineclass from cluster
			 probe for machine-class in the identified namespace and then creae a copy of this machine-class with additional delta available in machineclass-delta.yaml ( eg. tag (providerSpec.tags)  \"mcm-integration-test: "true"\" )
			  --- (Obsolete ?) ---> the namespace of the new machine-class should be default
	*/

	applyMC := "../../../kubernetes/machine-class.yaml"

	err := c.applyFiles(applyMC)
	if err != nil {
		return err
	}
	return nil
}

func (c *ControllerTest) createDummyMachineClass() error {
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

func (c *ControllerTest) applyFiles(filePath string) error {
	var files []string
	err := filepath.Walk(filePath, func(path string, info os.FileInfo, err error) error {
		files = append(files, path)
		return nil
	})
	if err != nil {
		panic(err)
	}

	for _, file := range files {
		log.Println(file)
		fi, err := os.Stat(file)
		if err != nil {
			log.Println("\nError file does not exist!")
			return err
		}

		switch mode := fi.Mode(); {
		case mode.IsDir():
			// do directory stuff
			log.Printf("\n%s is a directory. Therefore nothing will happen!\n", file)
		case mode.IsRegular():
			// do file stuff
			log.Printf("\n%s is a file. Therefore applying yaml ...", file)
			err := c.ControlKubeCluster.ApplyYamlFile(file, c.controlClusterNamespace)
			if err != nil {
				if strings.Contains(err.Error(), "already exists") {
					log.Printf("\n%s already exists, so skipping ...\n", file)
				} else {
					log.Printf("\nFailed to create machine class %s, in the cluster.\n", file)
					return err
				}

			}
		}
	}
	err = c.ControlKubeCluster.CheckEstablished()
	if err != nil {
		return err
	}
	return nil
}

func (c *ControllerTest) execCommandAsRoutine(ctx context.Context, cmd string, dir string, logFile string) {
	c.numberOfBgProcesses++
	args := strings.Fields(cmd)

	command := exec.CommandContext(ctx, args[0], args[1:]...)
	outputFile, err := os.Create(logFile)

	if err != nil {
		log.Printf("Error occured while creating log file %s. Error is %s", logFile, err)
	}

	defer func() {
		c.numberOfBgProcesses = c.numberOfBgProcesses - 1
		outputFile.Close()

		err := command.Process.Kill()
		log.Printf("process has been terminated. Check %s\n%s", logFile, err)
		//command.Process.Signal(os.Interrupt)
		c.wg.Done()
	}()

	command.Dir = dir
	command.Stdout = outputFile
	command.Stderr = outputFile
	log.Println("Goroutine started")

	err = command.Run()

	if err != nil {
		log.Println("make command terminated")
	}
	log.Println("For more details check:", logFile)

}

func (c *ControllerTest) SetupBeforeSuite() {
	/*Check control cluster and target clusters are accessible
	- Check and create crds ( machineclass, machines, machinesets and machinedeployment ) if required
	using file available in kubernetes/crds directory of machine-controller-manager repo
	- Start the Machine Controller manager and machine controller (provider-specific)
	- Assume secret resource for accesing the cloud provider service in already in the control cluster
	- Create machineclass resource from file available in kubernetes directory of provider specific repo in control cluster
	*/
	log.SetOutput(GinkgoWriter)

	By("Checking for the clusters if provided are available")
	Expect(c.prepareClusters()).To(BeNil())

	if !c.ControlKubeCluster.IsSeed(c.TargetKubeCluster) {
		By("Fetching kubernetes/crds and applying them into control cluster")
		Expect(c.applyCrds()).To(BeNil())

		By("Applying MachineClass")
		Expect(c.applyMachineClass()).To(BeNil())
	} else {
		By("Creating dup MachineClass with delta yaml")
		Expect(c.createDummyMachineClass()).To(BeNil())
	}

	if len(c.mcContainerImageTag) != 0 && len(c.mcmContainerImageTag) != 0 {
		log.Println("length is ", len(c.mcContainerImageTag))
		/* - if any of mcmContainerImage  or mcContainerImageTag flag is non-empty then,
		create/update machinecontrollermanager deployment in the control-cluster with specified image
		- crds already exist in the cluster.
		TO-DO: try to look for crds in local kubernetes directory and apply them. this validates changes in crd structures (if any)
		*/
		By("Starting MCM Deployemnt")
		Expect(c.initMcmDeployment()).To(BeNil())
	} else {
		/* 	- applyCrds from the mcm repo by cloning it and then
		- as mcmContainerImage is empty, run mc and mcm locally
		*/
		By("Cloning Machine-Controller-Manager github repo")
		Expect(c.cloneMcmRepo()).To(BeNil())

		if c.ControlKubeCluster.IsSeed(c.TargetKubeCluster) {
			By("Scaledown existing machine controllers")
			Expect(c.scaleMcmDeployment(0)).To(BeNil())
		}

		By("Starting Machine Controller Manager")
		Expect(c.startMachineControllerManager(c.ctx)).To(BeNil())
		By("Starting Machine Controller")
		Expect(c.startMachineController(c.ctx)).To(BeNil())
	}
}

func (c *ControllerTest) BeforeEachCheck() {
	if !c.ControlKubeCluster.IsSeed(c.TargetKubeCluster) || len(c.mcContainerImageTag) == 0 || len(c.mcmContainerImageTag) == 0 {
		By("Check the number of goroutines running are 2")
		Expect(c.numberOfBgProcesses).To(BeEquivalentTo(2))
	}
	// Nodes are healthy
	By("Check nodes in target cluster are healthy")
	// Expect(TargetKubeCluster.NumberOfReadyNodes()).To(BeEquivalentTo(TargetKubeCluster.NumberOfNodes()))
	Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 180, 5).Should(BeNumerically("==", c.TargetKubeCluster.NumberOfNodes()))
}

func (c *ControllerTest) ControllerTests() {
	Describe("Machine Resource", func() {
		Describe("Creating one machine resource", func() {
			Context("In Control cluster", func() {

				// Probe nodes currently available in target cluster
				var initialNodes int16

				It("should not lead to any errors", func() {
					// apply machine resource yaml file
					initialNodes = c.TargetKubeCluster.NumberOfNodes()
					Expect(c.ControlKubeCluster.ApplyYamlFile("../../../kubernetes/machine.yaml", c.controlClusterNamespace)).To(BeNil())
					//fmt.Println("wait for 30 sec before probing for nodes")
				})
				It("should list existing +1 nodes in target cluster", func() {
					log.Println("Wait until a new node is added. Number of nodes should be ", initialNodes+1)
					// check whether there is one node more
					Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 600, 5).Should(BeNumerically("==", initialNodes+1))
				})
			})
		})

		Describe("Deleting one machine resource", func() {
			// BeforeEach(func() {
			// 	// Check there are no machine deployment and machinesets resources existing
			// 	deploymentList, err := ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(controlClusterNamespace).List(metav1.ListOptions{})
			// 	Expect(len(deploymentList.Items)).Should(BeZero(), "Zero MachineDeployments should exist")
			// 	Expect(err).Should(BeNil())
			// 	machineSetsList, err := ControlKubeCluster.McmClient.MachineV1alpha1().MachineSets(controlClusterNamespace).List(metav1.ListOptions{})
			// 	Expect(len(machineSetsList.Items)).Should(BeZero(), "Zero Machinesets should exist")
			// 	Expect(err).Should(BeNil())

			// })
			Context("When there are machine resources available in control cluster", func() {
				var initialNodes int16
				It("should not lead to errors", func() {
					machinesList, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(c.controlClusterNamespace).List(metav1.ListOptions{})
					if len(machinesList.Items) != 0 {
						//Context("When one machine is deleted randomly", func() { //randomly ? Caution - looks like we are not getting blank cluster
						// Keep count of nodes available
						//delete machine resource
						initialNodes = c.TargetKubeCluster.NumberOfNodes()
						Expect(c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(c.controlClusterNamespace).Delete("test-machine", &metav1.DeleteOptions{})).Should(BeNil(), "No Errors while deleting machine")
					}
				})
				It("should list existing nodes -1 in target cluster", func() {
					// check there are n-1 nodes
					if initialNodes != 0 {
						Eventually(c.TargetKubeCluster.NumberOfNodes, 180, 5).Should(BeNumerically("==", initialNodes-1))
					}
				})
			})
			Context("when there are no machines available", func() {
				var initialNodes int16
				// delete one machine (non-existent) by random text as name of resource
				It("should list existing nodes ", func() {
					// check there are no changes to nodes
					machinesList, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(c.controlClusterNamespace).List(metav1.ListOptions{})
					if len(machinesList.Items) == 0 {
						// Keep count of nodes available
						// delete machine resource
						initialNodes = c.TargetKubeCluster.NumberOfNodes()
						err := c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(c.controlClusterNamespace).Delete("test-machine-dummy", &metav1.DeleteOptions{})
						Expect(err).To(HaveOccurred())
						time.Sleep(30 * time.Second)
						Expect(c.TargetKubeCluster.NumberOfNodes()).To(BeEquivalentTo(initialNodes))
					}
				})
			})
		})
	})
	// Testcase #02 | Machine Deployment
	Describe("Machine Deployment resource", func() {
		var initialNodes int16
		Context("Creation with 3 replicas", func() {
			It("Should not lead to any errors", func() {
				//probe initialnodes before continuing
				initialNodes = c.TargetKubeCluster.NumberOfNodes()

				// apply machine resource yaml file
				Expect(c.ControlKubeCluster.ApplyYamlFile("../../../kubernetes/machine-deployment.yaml", c.controlClusterNamespace)).To(BeNil())
			})
			It("should lead to 3 more nodes in target cluster", func() {
				log.Println("Wait until new nodes are added. Number of nodes should be ", initialNodes+3)

				// check whether all the expected nodes are ready
				Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 180, 5).Should(BeNumerically("==", initialNodes+3))
			})
		})
		Context("Scale up to 6", func() {
			It("Should not lead to any errors", func() {

				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Replicas = 6
					_, updateErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Update(machineDployment)
					return updateErr
				})

				Expect(retryErr).NotTo(HaveOccurred())
			})
			It("should lead to 3 more nodes in target cluster", func() {
				log.Println("Wait until new nodes are added. Number of nodes should be ", initialNodes+6)

				// check whether all the expected nodes are ready
				Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 180, 5).Should(BeNumerically("==", initialNodes+6))
			})

		})
		Context("Scale down to 2", func() {
			// rapidly scaling back to 2 leading to a freezing and unfreezing
			// check for freezing and unfreezing of machine due to rapid scale up and scale down in the logs of mcm
			/* freeze_count=$(cat logs/${provider}-mcm.out | grep ' Froze MachineSet' | wc -l)
			 if [[ freeze_count -eq 0 ]]; then
				 printf "\tFailed: Freezing of machineSet failed. Exiting Test to avoid further conflicts.\n"
				 terminate_script
			 fi

			 unfreeze_count=$(cat logs/${provider}-mcm.out | grep ' Unfroze MachineSet' | wc -l)
			 if [[ unfreeze_count -eq 0 ]]; then
				 printf "\tFailed: Unfreezing of machineSet failed. Exiting Test to avoid further conflicts.\n"
				 terminate_script
			 fi */
			It("Should not lead to any errors", func() {

				//Fetch machine deployment
				machineDeployment, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})

				//revert replica count to 3
				machineDeployment.Spec.Replicas = 2

				//update machine deployment
				_, err := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Update(machineDeployment)

				//Check there is no error occured
				Expect(err).NotTo(HaveOccurred())
			})
			It("should lead to 2 nodes left in the target cluster", func() {
				Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 300, 5).Should(BeNumerically("==", initialNodes+2))
			})
			It("Should lead to freezing and unfreezing of machine", func() {
				By("Reading log file")
				data, err := ioutil.ReadFile(c.mcm_logFile)
				Expect(err).NotTo(HaveOccurred())
				By("Logging Froze in mcm log file")
				matched, _ := regexp.Match(` Froze MachineSet`, data)
				Expect(matched).To(BeTrue())
				By("Logging Unfroze in mcm log file")
				matched, err = regexp.Match(` Unfroze MachineSet`, data)
				Expect(matched).To(BeTrue())
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("Update the machine to v2 machine-class and scale up replicas", func() {
			// update machine type -> machineDeployment.spec.template.spec.class.name = "test-mc-dummy"
			// scale up replicas by 4
			// To-Do: Add check for rolling update completion (updatedReplicas check)
			It("should wait for machines to upgrade to larger machine types and scale up replicas", func() {
				// wait for 2400s till machines updates
				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Template.Spec.Class.Name = c.testMachineClassResources[1]
					machineDployment.Spec.Replicas = 6
					_, updateErr := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Update(machineDployment)
					return updateErr
				})
				//Check there is no error occured
				Expect(retryErr).NotTo(HaveOccurred())
				Eventually(c.ControlKubeCluster.GetUpdatedReplicasCount("test-machine-deployment", c.controlClusterNamespace), 300, 5).Should(BeNumerically("==", 6))
				Eventually(c.TargetKubeCluster.NumberOfReadyNodes, 300, 5).Should(BeNumerically("==", initialNodes+6))
			})
		})

		Context("Deletion", func() {
			Context("When there are machine deployment(s) available in control cluster", func() {
				var initialNodes int16
				It("should not lead to errors", func() {
					machinesList, _ := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).List(metav1.ListOptions{})
					if len(machinesList.Items) != 0 {
						// Keep count of nodes available
						initialNodes = c.TargetKubeCluster.NumberOfNodes()

						//delete machine resource
						Expect(c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Delete("test-machine-deployment", &metav1.DeleteOptions{})).Should(BeNil(), "No Errors while deleting machine deployment")
					}
				})
				It("should list existing nodes-6 in target cluster", func() {
					// check there are n-1 nodes
					if initialNodes != 0 {
						Eventually(c.TargetKubeCluster.NumberOfNodes, 300, 5).Should(BeNumerically("==", initialNodes-6))
					}
				})
			})
		})

	})
}

func (c *ControllerTest) Cleanup() bool {
	return AfterSuite(func() {
		if c.ControlKubeCluster.McmClient != nil {

			_, err := c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Get("test-machine-deployment", metav1.GetOptions{})
			if err != nil {
				c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments(c.controlClusterNamespace).Delete("test-machine-deployment", &metav1.DeleteOptions{})
			}

			c.ControlKubeCluster.McmClient.MachineV1alpha1().Machines(c.controlClusterNamespace).Delete("test-machine", &metav1.DeleteOptions{})
		}
		//<-time.After(3 * time.Second)
		//delete tempMachineClass
		c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(c.controlClusterNamespace).Delete(c.testMachineClassResources[0], &metav1.DeleteOptions{})
		c.ControlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(c.controlClusterNamespace).Delete(c.testMachineClassResources[1], &metav1.DeleteOptions{})

		if !c.ControlKubeCluster.IsSeed(c.TargetKubeCluster) {
			log.Println("Initiating gorouting cancel via context done")

			c.cancelFunc()

			log.Println("Terminating processes")
			c.wg.Wait()
			log.Println("processes terminated")
		} else {
			retry.RetryOnConflict(retry.DefaultRetry, func() error {
				// Retrieve the latest version of Deployment before attempting update
				// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
				_, updateErr := c.ControlKubeCluster.Clientset.AppsV1().Deployments(c.mcmDeploymentOrigObj.Namespace).Update(&(c.mcmDeploymentOrigObj))
				return updateErr
			})
		}
		c.scaleMcmDeployment(1)
	})
}
