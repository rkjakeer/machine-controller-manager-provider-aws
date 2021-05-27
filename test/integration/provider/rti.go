package provider

import (
	"fmt"

	v1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	v1 "k8s.io/api/core/v1"
)

// type ResourcesTrackerInterface struct {
// 	InitialVolumes   []string
// 	InitialInstances []string
// 	MachineClass     *v1alpha1.MachineClass
// 	Secret           *v1.Secret
// 	ClusterName      string
// }

type ResourcesTrackerImpl struct {
	InitialVolumes   []string
	InitialInstances []string
	MachineClass     *v1alpha1.MachineClass
	Secret           *v1.Secret
	ClusterName      string
}

func (r *ResourcesTrackerImpl) InitializeResourcesTracker(machineClass *v1alpha1.MachineClass, secret *v1.Secret, clusterName string) error {

	clusterTag := "tag:kubernetes.io/cluster/" + clusterName
	clusterTagValue := "1"

	r.MachineClass = machineClass
	r.Secret = secret
	r.ClusterName = clusterName
	instances, err := DescribeInstancesWithTag("tag:mcm-integration-test", "true", machineClass, secret)
	if err == nil {
		r.InitialInstances = instances
		volumes, err := DescribeAvailableVolumes(clusterTag, clusterTagValue, machineClass, secret)
		if err == nil {
			r.InitialVolumes = volumes
			return nil
		} else {
			return err
		}
	} else {
		return err
	}
}

// CheckForOrphanedResources will search the cloud provider for orphaned resources that are left behind after the test cases
func (r *ResourcesTrackerImpl) ProbeResources() ([]string, []string, error) {
	// Check for VM instances with matching tags/labels
	// Describe volumes attached to VM instance & delete the volumes
	// Finally delete the VM instance

	clusterTag := "tag:kubernetes.io/cluster/" + r.ClusterName
	clusterTagValue := "1"

	instances, err := DescribeInstancesWithTag("tag:mcm-integration-test", "true", r.MachineClass, r.Secret)
	if err != nil {
		return instances, nil, err
	}

	// Check for available volumes in cloud provider with tag/label [Status:available]
	availVols, err := DescribeAvailableVolumes(clusterTag, clusterTagValue, r.MachineClass, r.Secret)
	if err != nil {
		return instances, availVols, err
	}

	// Check for available vpc and network interfaces in cloud provider with tag
	err = AdditionalResourcesCheck(clusterTag, clusterTagValue)
	if err != nil {
		return instances, availVols, err
	}

	return instances, availVols, nil
}

// DifferenceOrphanedResources checks for difference in the found orphaned resource before test execution with the list after test execution
func DifferenceOrphanedResources(beforeTestExecution []string, afterTestExecution []string) []string {
	var diff []string

	// Loop two times, first to find beforeTestExecution strings not in afterTestExecution,
	// second loop to find afterTestExecution strings not in beforeTestExecution
	for i := 0; i < 2; i++ {
		for _, b1 := range beforeTestExecution {
			found := false
			for _, a2 := range afterTestExecution {
				if b1 == a2 {
					found = true
					break
				}
			}
			// String not found. We add it to return slice
			if !found {
				diff = append(diff, b1)
			}
		}
		// Swap the slices, only if it was the first loop
		if i == 0 {
			beforeTestExecution, afterTestExecution = afterTestExecution, beforeTestExecution
		}
	}

	return diff
}

// DifferenceOrphanedResources checks for difference in the found orphaned resource before test execution with the list after test execution
func (r *ResourcesTrackerImpl) IsOrphanedResourcesAvailable() bool {
	afterTestExecutionInstances, afterTestExecutionAvailVols, err := r.ProbeResources()
	//Check there is no error occured
	if err == nil {
		orphanedResourceInstances := DifferenceOrphanedResources(r.InitialInstances, afterTestExecutionInstances)
		if orphanedResourceInstances != nil {
			fmt.Println("orphaned instances are:", orphanedResourceInstances)
			return true
		}
		orphanedResourceAvailVols := DifferenceOrphanedResources(r.InitialVolumes, afterTestExecutionAvailVols)
		if orphanedResourceAvailVols != nil {
			fmt.Println("orphaned volumes are:", orphanedResourceAvailVols)
			return true
		}
		return false
	}
	//assuming there are orphaned resources as probe can not be done
	return true
}
