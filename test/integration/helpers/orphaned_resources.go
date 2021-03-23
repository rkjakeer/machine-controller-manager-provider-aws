package helpers

import (
	v1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	v1 "k8s.io/api/core/v1"
)

// CheckForOrphanedResources will search the cloud provider for orphaned resources that are left behind after the test cases
func CheckForOrphanedResources(machineClass *v1alpha1.MachineClass, secret *v1.Secret) ([]string, []string, error) {
	// Check for VM instances with matching tags/labels
	// Describe volumes attached to VM instance & delete the volumes
	// Finally delete the VM instance

	clusterTag := "tag:kubernetes.io/cluster/shoot--mcm-test--tonia-aws"
	clusterTagValue := "1"

	instances, err := DescribeInstancesWithTag("tag:mcm-integration-test", "true", machineClass, secret)
	if err != nil {
		return instances, nil, err
	}

	// Check for available volumes in cloud provider with tag/label [Status:available]
	availVols, err := DescribeAvailableVolumes(clusterTag, clusterTagValue, machineClass, secret)
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
