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

	"github.com/gardener/machine-controller-manager-provider-aws/test/integration/helpers"
	v1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

var (
	controlKubeConfigPath = os.Getenv("controlKubeconfig")
	targetKubeConfigPath  = os.Getenv("targetKubeconfig")
	controlKubeCluster    *helpers.Cluster
	targetKubeCluster     *helpers.Cluster
	numberOfBgProcesses   int16
	mcmRepoPath           = "../../../dev/mcm"
	ctx, cancelFunc       = context.WithCancel(context.Background())
	wg                    sync.WaitGroup // prevents race condition between main and other goroutines exit
	mcm_logFile           = filepath.Join(os.Getenv("TEST_DIR"), "integration-test-mcm.log")
	mc_logFile            = filepath.Join(os.Getenv("TEST_DIR"), "integration-test-mc.log")
	mcContainerImageTag   = os.Getenv("mcContainerImage")
	mcmContainerImageTag  = os.Getenv("mcmContainerImage")
	mcmDeploymentOrigObj  v1.Deployment
)

var _ = Describe("Integration test", func() {
	BeforeSuite(func() {
		/*Check control cluster and target clusters are accessible
		- Check and create crds ( machineclass, machines, machinesets and machinedeployment ) if required
		  using file available in kubernetes/crds directory of machine-controller-manager repo
		- Start the Machine Controller manager and machine controller (provider-specific)
		- Assume secret resource for accesing the cloud provider service in already in the control cluster
		- Create machineclass resource from file available in kubernetes directory of provider specific repo in control cluster
		*/
		log.SetOutput(GinkgoWriter)

		By("Checking for the clusters if provided are available")
		Expect(prepareClusters()).To(BeNil())

		if !controlKubeCluster.IsSeed(targetKubeCluster) {
			By("Fetching kubernetes/crds and applying them into control cluster")
			Expect(applyCrds()).To(BeNil())

			By("Applying MachineClass")
			Expect(applyMachineClass()).To(BeNil())
		} else {
			By("Creating dup MachineClass with delta yaml")
			Expect(createDummyMachineClass()).To(BeNil())
		}

		if len(mcContainerImageTag) != 0 || len(mcmContainerImageTag) != 0 {
			/* - if any of mcmContainerImage  or mcContainerImageTag flag is non-empty then,
			create/update machinecontrollermanager deployment in the control-cluster with specified image
			- crds already exist in the cluster.
			TO-DO: try to look for crds in local kubernetes directory and apply them. this validates changes in crd structures (if any)
			*/
			By("Starting MCM Deployemnt")
			Expect(initMcmDeployment()).To(BeNil())
		} else {
			/* 	- applyCrds from the mcm repo by cloning it and then
			- as mcmContainerImage is empty, run mc and mcm locally
			*/

			By("Starting Machine Controller Manager")
			Expect(startMachineControllerManager(ctx)).To(BeNil())
			By("Starting Machine Controller")
			Expect(startMachineController(ctx)).To(BeNil())
		}
	})
	BeforeEach(func() {
		if !controlKubeCluster.IsSeed(targetKubeCluster) {
			By("Check the number of goroutines running are 2")
			Expect(numberOfBgProcesses).To(BeEquivalentTo(2))
		}
		// Nodes are healthy
		By("Check nodes in target cluster are healthy")
		// Expect(targetKubeCluster.NumberOfReadyNodes()).To(BeEquivalentTo(targetKubeCluster.NumberOfNodes()))
		Eventually(targetKubeCluster.NumberOfReadyNodes, 180, 5).Should(BeNumerically("==", targetKubeCluster.NumberOfNodes()))
	})

	// Check for Orphaned Resources Before Test Execution
	Describe("Check for orphaned resources before test execution", func() {
		Context("In target cluster", func() {
			Context("Check if there are any resources matching the tag exists", func() {
				It("Should list any orphaned resources if available", func() {
					// if available should delete orphaned resources in cloud provider
					machineClass, err := controlKubeCluster.McmClient.MachineV1alpha1().MachineClasses("default").Get("test-mc", metav1.GetOptions{})
					if err == nil {
						secret, err := controlKubeCluster.Clientset.CoreV1().Secrets(machineClass.SecretRef.Namespace).Get(machineClass.SecretRef.Name, metav1.GetOptions{})
						if err == nil {
							beforeTestExecutionInstances, beforeTestExecutionAvailVols, err = helpers.CheckForOrphanedResources(machineClass, secret)
							//Check there is no error occured
							Expect(err).NotTo(HaveOccurred())
						}
						Expect(err).NotTo(HaveOccurred())
					}
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})
	})

	Describe("Machine Resource", func() {
		Describe("Creating one machine resource", func() {
			Context("In Control cluster", func() {

				// Probe nodes currently available in target cluster
				var initialNodes int16

				It("should not lead to any errors", func() {
					// apply machine resource yaml file
					initialNodes = targetKubeCluster.NumberOfNodes()
					Expect(controlKubeCluster.ApplyYamlFile("../../../kubernetes/machine.yaml")).To(BeNil())
					//fmt.Println("wait for 30 sec before probing for nodes")
				})
				It("should list existing +1 nodes in target cluster", func() {
					log.Println("Wait until a new node is added. Number of nodes should be ", initialNodes+1)
					// check whether there is one node more
					Eventually(targetKubeCluster.NumberOfReadyNodes, 300, 5).Should(BeNumerically("==", initialNodes+1))
				})
			})
		})

		Describe("Deleting one machine resource", func() {
			BeforeEach(func() {
				// Check there are no machine deployment and machinesets resources existing
				deploymentList, err := controlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments("default").List(metav1.ListOptions{})
				Expect(len(deploymentList.Items)).Should(BeZero(), "Zero MachineDeployments should exist")
				Expect(err).Should(BeNil())
				machineSetsList, err := controlKubeCluster.McmClient.MachineV1alpha1().MachineSets("default").List(metav1.ListOptions{})
				Expect(len(machineSetsList.Items)).Should(BeZero(), "Zero Machinesets should exist")
				Expect(err).Should(BeNil())

			})
			Context("When there are machine resources available in control cluster", func() {
				var initialNodes int16
				It("should not lead to errors", func() {
					machinesList, _ := controlKubeCluster.McmClient.MachineV1alpha1().Machines("default").List(metav1.ListOptions{})
					if len(machinesList.Items) != 0 {
						//Context("When one machine is deleted randomly", func() { //randomly ? Caution - looks like we are not getting blank cluster
						// Keep count of nodes available
						//delete machine resource
						initialNodes = targetKubeCluster.NumberOfNodes()
						Expect(controlKubeCluster.McmClient.MachineV1alpha1().Machines("default").Delete("test1-machine1", &metav1.DeleteOptions{})).Should(BeNil(), "No Errors while deleting machine")
					}
				})
				It("should list existing nodes -1 in target cluster", func() {
					// check there are n-1 nodes
					if initialNodes != 0 {
						Eventually(targetKubeCluster.NumberOfNodes, 180, 5).Should(BeNumerically("==", initialNodes-1))
					}
				})
			})
			Context("when there are no machines available", func() {
				var initialNodes int16
				// delete one machine (non-existent) by random text as name of resource
				It("should list existing nodes ", func() {
					// check there are no changes to nodes
					machinesList, _ := controlKubeCluster.McmClient.MachineV1alpha1().Machines("default").List(metav1.ListOptions{})
					if len(machinesList.Items) == 0 {
						// Keep count of nodes available
						// delete machine resource
						initialNodes = targetKubeCluster.NumberOfNodes()
						err := controlKubeCluster.McmClient.MachineV1alpha1().Machines("default").Delete("test1-machine1-dummy", &metav1.DeleteOptions{})
						Expect(err).To(HaveOccurred())
						time.Sleep(30 * time.Second)
						Expect(targetKubeCluster.NumberOfNodes()).To(BeEquivalentTo(initialNodes))
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
				initialNodes = targetKubeCluster.NumberOfNodes()

				// apply machine resource yaml file
				Expect(controlKubeCluster.ApplyYamlFile("../../../kubernetes/machine-deployment.yaml")).To(BeNil())
			})
			It("should lead to 3 more nodes in target cluster", func() {
				log.Println("Wait until new nodes are added. Number of nodes should be ", initialNodes+3)

				// check whether all the expected nodes are ready
				Eventually(targetKubeCluster.NumberOfReadyNodes, 180, 5).Should(BeNumerically("==", initialNodes+3))
			})
		})
		Context("Scale up to 6", func() {
			It("Should not lead to any errors", func() {

				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := controlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments("default").Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Replicas = 6
					_, updateErr := controlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments("default").Update(machineDployment)
					return updateErr
				})

				Expect(retryErr).NotTo(HaveOccurred())
			})
			It("should lead to 3 more nodes in target cluster", func() {
				log.Println("Wait until new nodes are added. Number of nodes should be ", initialNodes+6)

				// check whether all the expected nodes are ready
				Eventually(targetKubeCluster.NumberOfReadyNodes, 180, 5).Should(BeNumerically("==", initialNodes+6))
			})

		})
		Context("Scale down to 2", func() {
			// TODO :- check for freezing and unfreezing
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
				machineDeployment, _ := controlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments("default").Get("test-machine-deployment", metav1.GetOptions{})

				//revert replica count to 3
				machineDeployment.Spec.Replicas = 2

				//update machine deployment
				_, err := controlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments("default").Update(machineDeployment)

				//Check there is no error occured
				Expect(err).NotTo(HaveOccurred())
			})
			It("should lead to 2 nodes left in the target cluster", func() {
				Eventually(targetKubeCluster.NumberOfReadyNodes, 300, 5).Should(BeNumerically("==", initialNodes+2))
			})
			It("Should lead to freezing and unfreezing of machine", func() {
				By("Reading log file")
				data, err := ioutil.ReadFile(mcm_logFile)
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
			// update machine type -> machineDeployment.spec.template.spec.class.name = test-mc-2
			// scale up replicas by 4
			It("should wait for machines to upgrade to larger machine types and scale up replicas", func() {
				// wait for 2400s till machines updates
				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					machineDployment, _ := controlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments("default").Get("test-machine-deployment", metav1.GetOptions{})
					machineDployment.Spec.Template.Spec.Class.Name = "test-mc-dummy"
					machineDployment.Spec.Replicas = 6
					_, updateErr := controlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments("default").Update(machineDployment)
					return updateErr
				})
				//Check there is no error occured
				Expect(retryErr).NotTo(HaveOccurred())
				Eventually(targetKubeCluster.NumberOfReadyNodes, 300, 5).Should(BeNumerically("==", initialNodes+6))
			})
		})

		Context("Deletion", func() {
			Context("When there are machine deployment(s) available in control cluster", func() {
				var initialNodes int16
				It("should not lead to errors", func() {
					machinesList, _ := controlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments("default").List(metav1.ListOptions{})
					if len(machinesList.Items) != 0 {
						// Keep count of nodes available
						initialNodes = targetKubeCluster.NumberOfNodes()

						//delete machine resource
						Expect(controlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments("default").Delete("test-machine-deployment", &metav1.DeleteOptions{})).Should(BeNil(), "No Errors while deleting machine deployment")
					}
				})
				It("should list existing nodes-6 in target cluster", func() {
					// check there are n-1 nodes
					if initialNodes != 0 {
						Eventually(targetKubeCluster.NumberOfNodes, 300, 5).Should(BeNumerically("==", initialNodes-6))
					}
				})
			})
		})

	})

	// ---------------------------------------------------------------------------------------
	// Testcase #03 | Orphaned Resources
	Describe("Check for orphaned resources", func() {
		Context("In target cluster", func() {
			Context("Check if there are any resources matching the tag exists", func() {
				It("Should list any orphaned resources if available", func() {
					// if available should delete orphaned resources in cloud provider
					machineClass, err := controlKubeCluster.McmClient.MachineV1alpha1().MachineClasses("default").Get("test-mc", metav1.GetOptions{})
					if err == nil {
						secret, err := controlKubeCluster.Clientset.CoreV1().Secrets(machineClass.SecretRef.Namespace).Get(machineClass.SecretRef.Name, metav1.GetOptions{})
						if err == nil {
							afterTestExecutionInstances, afterTestExecutionAvailVols, err := helpers.CheckForOrphanedResources(machineClass, secret)
							//Check there is no error occured
							Expect(err).NotTo(HaveOccurred())
							if err == nil {
								orphanedResourceInstances := helpers.DifferenceOrphanedResources(beforeTestExecutionInstances, afterTestExecutionInstances)
								Expect(orphanedResourceInstances).To(BeNil())
								orphanedResourceAvailVols := helpers.DifferenceOrphanedResources(beforeTestExecutionAvailVols, afterTestExecutionAvailVols)
								Expect(orphanedResourceAvailVols).To(BeNil())
							}
						}
						Expect(err).NotTo(HaveOccurred())
					}
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})
	})

	AfterSuite(func() {
		if controlKubeCluster.McmClient != nil {

			_, err := controlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments("default").Get("test-machine-deployment", metav1.GetOptions{})
			if err != nil {
				controlKubeCluster.McmClient.MachineV1alpha1().MachineDeployments("default").Delete("test-machine-deployment", &metav1.DeleteOptions{})
			}

			controlKubeCluster.McmClient.MachineV1alpha1().Machines("default").Delete("test1-machine1", &metav1.DeleteOptions{})
		}
		//<-time.After(3 * time.Second)
		if !controlKubeCluster.IsSeed(targetKubeCluster) {
			log.Println("Initiating gorouting cancel via context done")

			cancelFunc()

			log.Println("Terminating processes")
			wg.Wait()
			log.Println("processes terminated")
		} else {
			retry.RetryOnConflict(retry.DefaultRetry, func() error {
				// Retrieve the latest version of Deployment before attempting update
				// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
				_, updateErr := controlKubeCluster.Clientset.AppsV1().Deployments(mcmDeploymentOrigObj.Namespace).Update(&mcmDeploymentOrigObj)
				return updateErr
			})

			//delete tempMachineClass
			namespace, _ := targetKubeCluster.ClusterName()
			controlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(namespace).Delete("test-mc", &metav1.DeleteOptions{})
			controlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(namespace).Delete("test-mc-dummy", &metav1.DeleteOptions{})
		}
	})

})

func prepareClusters() error {
	/* prepareClusters checks for
	- the validity of controlKubeConfig and targetKubeConfig flags
	- It should return an error if thre is a error
	*/

	log.Printf("Control path is %s\n", controlKubeConfigPath)
	log.Printf("Target path is %s\n", targetKubeConfigPath)
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
		controlKubeCluster, err = helpers.NewCluster(controlKubeConfigPath)
		if err != nil {
			return err
		}
		targetKubeCluster, err = helpers.NewCluster(targetKubeConfigPath)
		if err != nil {
			return err
		}

		// update clientset and check whether the cluster is accessible
		err = controlKubeCluster.FillClientSets()
		if err != nil {
			log.Println("Failed to check nodes in the cluster")
			return err
		}

		err = targetKubeCluster.FillClientSets()
		if err != nil {
			log.Println("Failed to check nodes in the cluster")
			return err
		}
	} else if targetKubeConfigPath != "" {
		return fmt.Errorf("controlKubeconfig path is mandatory if using targetKubeConfigPath. Aborting!!!")
	}
	return nil
}

func applyCrds() error {
	/* TO-DO: applyCrds will
	- create the custom resources in the controlKubeConfig
	- yaml files are available in kubernetes/crds directory of machine-controller-manager repo
	- resources to be applied are machineclass, machines, machinesets and machinedeployment
	*/

	dst := mcmRepoPath
	src := "https://github.com/gardener/machine-controller-manager.git"
	applyCrdsDirectory := fmt.Sprintf("%s/kubernetes/crds", dst)

	helpers.CheckDst(dst)
	helpers.CloningRepo(dst, src)

	err := applyFiles(applyCrdsDirectory)
	if err != nil {
		return err
	}
	return nil
}

func startMachineControllerManager(ctx context.Context) error {
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
	command := fmt.Sprintf("make start CONTROL_KUBECONFIG=%s TARGET_KUBECONFIG=%s", controlKubeConfigPath, targetKubeConfigPath)
	log.Println("starting MachineControllerManager with command: ", command)
	dst_path := fmt.Sprintf("%s", mcmRepoPath)
	wg.Add(1)
	go execCommandAsRoutine(ctx, command, dst_path, mcm_logFile)
	return nil
}

func startMachineController(ctx context.Context) error {
	/*
		  startMachineController starts the machine controller
			  - if mcContainerImage flag is non-empty then, start a pod in the control-cluster with specified image
			  - if mcContainerImage is empty, runs machine controller locally
	*/
	command := fmt.Sprintf("make start CONTROL_KUBECONFIG=%s TARGET_KUBECONFIG=%s", controlKubeConfigPath, targetKubeConfigPath)
	log.Println("starting MachineController with command: ", command)
	wg.Add(1)
	go execCommandAsRoutine(ctx, command, "../../..", mc_logFile)
	return nil
}

func initMcmDeployment() error {
	/*
		 - if any of mcmContainerImage  or mcContainerImageTag flag is non-empty then,
			 update machinecontrollermanager deployment in the control-cluster with specified image
		 -
	*/
	namespace, _ := targetKubeCluster.ClusterName()
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		result, getErr := controlKubeCluster.Clientset.AppsV1().Deployments(namespace).Get("machine-controller-manager", metav1.GetOptions{})
		if getErr != nil {
			panic(fmt.Errorf("Failed to get latest version of Deployment: %v", getErr))
		}
		mcmDeploymentOrigObj = *result
		for i := range result.Spec.Template.Spec.Containers {
			if result.Spec.Template.Spec.Containers[i].Name == "machine-controller-manager" {
				if len(mcmContainerImageTag) != 0 {
					result.Spec.Template.Spec.Containers[i].Image = "eu.gcr.io/gardener-project/gardener/machine-controller-manager:" + mcmContainerImageTag
				}
			} else if result.Spec.Template.Spec.Containers[i].Name == "machine-controller" {
				if len(mcContainerImageTag) != 0 {
					result.Spec.Template.Spec.Containers[i].Image = "eu.gcr.io/gardener-project/gardener/machine-controller-manager-provider-aws:" + mcContainerImageTag
				}
			}
		}
		_, updateErr := controlKubeCluster.Clientset.AppsV1().Deployments(namespace).Update(result)
		return updateErr
	})
	if retryErr != nil {
		return retryErr
	} else {
		return nil
	}
}

func applyMachineClass() error {
	/*
		 - if isControlClusterIsShootsSeed is true, then use machineclass from cluster
			 probe for machine-class in the identified namespace and then creae a copy of this machine-class with additional delta available in machineclass-delta.yaml ( eg. tag (providerSpec.tags)  \"mcm-integration-test: "true"\" )
			  --- (Obsolete ?) ---> the namespace of the new machine-class should be default
	*/

	applyMC := "../../../kubernetes/machine-class.yaml"

	err := applyFiles(applyMC)
	if err != nil {
		return err
	}
	return nil
}

func createDummyMachineClass() error {
	/* TO-DO: createDummyMachineClass
	 This will read the control cluster machineclass resource and creates a duplicate of it
	 it will additionally add the delta part found in machineclass yaml file

	 - (if not use machine-class.yaml file)
			 look for a file available in kubernetes directory of provider specific repo and then use it instead for creating machine class

	*/

	namespace, _ := targetKubeCluster.ClusterName()
	machineClasses, err := controlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(namespace).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	var newMachineClass *v1alpha1.MachineClass

	// Create machine-class using yaml and any of existing machineclass resource combined
	for _, machineClass := range machineClasses.Items {

		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			result, getErr := controlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(namespace).Get(machineClass.GetName(), metav1.GetOptions{})
			if getErr != nil {
				log.Println("Failed to get latest version of machineclass")
				return getErr
			}
			//machineClassOrigObj = *result
			metaData := metav1.ObjectMeta{
				Name:        "test-mc",
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
			// applyFiles(machineClass)
			// remove dynamic fileds. eg uid, creation time e.t.c.,
			// create result (or machineClassOrigObj) with "../../../kubernetes/machine-class.yaml" content
			_, createErr := controlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(namespace).Create(newMachineClass)
			return createErr
		})
		if retryErr != nil {
			return retryErr
		}

		// patch

		retryErr = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// read machineClass patch yaml file ("../../../kubernetes/machine-class-patch.yaml" ) and update machine class(machineClass)
			data, _ := os.ReadFile("../../../kubernetes/machine-class-patch.yaml")
			_, patchErr := controlKubeCluster.McmClient.MachineV1alpha1().MachineClasses(namespace).Patch(newMachineClass.Name, types.JSONPatchType, data)
			return patchErr
		})
		if retryErr != nil {
			return retryErr
		}

	}
	return nil
}

func applyFiles(filePath string) error {
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
			err := controlKubeCluster.ApplyYamlFile(file)
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
	err = controlKubeCluster.CheckEstablished()
	if err != nil {
		return err
	}
	return nil
}

func execCommandAsRoutine(ctx context.Context, cmd string, dir string, logFile string) {
	numberOfBgProcesses++
	args := strings.Fields(cmd)

	command := exec.CommandContext(ctx, args[0], args[1:]...)
	outputFile, err := os.Create(logFile)

	if err != nil {
		log.Printf("Error occured while creating log file %s. Error is %s", logFile, err)
	}

	defer func() {
		numberOfBgProcesses = numberOfBgProcesses - 1
		outputFile.Close()

		err := command.Process.Kill()
		log.Printf("process has been terminated. Check %s\n%s", logFile, err)
		//command.Process.Signal(os.Interrupt)
		wg.Done()
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
