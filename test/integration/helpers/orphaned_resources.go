package helpers

import (
	"fmt"

	v1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	v1 "k8s.io/api/core/v1"
)

type ProviderResourcesTracker struct {
	InitialVolumes   []string
	InitialInstances []string
	MachineClass     *v1alpha1.MachineClass
	Secret           *v1.Secret
	ClusterName      string
}

func NewProviderResourcesTracker(machineClass *v1alpha1.MachineClass, secret *v1.Secret, clusterName string) (p *ProviderResourcesTracker, e error) {

	clusterTag := "tag:kubernetes.io/cluster/" + clusterName
	clusterTagValue := "1"

	p = &ProviderResourcesTracker{
		MachineClass: machineClass,
		Secret:       secret,
		ClusterName:  clusterName,
	}

	instances, err := DescribeInstancesWithTag("tag:mcm-integration-test", "true", machineClass, secret)
	if err == nil {
		p.InitialInstances = instances
		volumes, err := DescribeAvailableVolumes(clusterTag, clusterTagValue, machineClass, secret)
		if err == nil {
			p.InitialVolumes = volumes
			return p, nil
		} else {
			return p, err
		}
	} else {
		return p, err
	}
}

// CheckForOrphanedResources will search the cloud provider for orphaned resources that are left behind after the test cases
func (p *ProviderResourcesTracker) CheckForResources() ([]string, []string, error) {
	// Check for VM instances with matching tags/labels
	// Describe volumes attached to VM instance & delete the volumes
	// Finally delete the VM instance

	clusterTag := "tag:kubernetes.io/cluster/" + p.ClusterName
	clusterTagValue := "1"

	instances, err := DescribeInstancesWithTag("tag:mcm-integration-test", "true", p.MachineClass, p.Secret)
	if err != nil {
		return instances, nil, err
	}

	// Check for available volumes in cloud provider with tag/label [Status:available]
	availVols, err := DescribeAvailableVolumes(clusterTag, clusterTagValue, p.MachineClass, p.Secret)
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
func (p *ProviderResourcesTracker) IsOrphanedResourcesAvailable() bool {
	afterTestExecutionInstances, afterTestExecutionAvailVols, err := p.CheckForResources()
	//Check there is no error occured
	if err == nil {
		orphanedResourceInstances := DifferenceOrphanedResources(p.InitialInstances, afterTestExecutionInstances)
		if orphanedResourceInstances != nil {
			fmt.Println("orphaned instances are:", orphanedResourceInstances)
			return true
		}
		orphanedResourceAvailVols := DifferenceOrphanedResources(p.InitialVolumes, afterTestExecutionAvailVols)
		if orphanedResourceAvailVols != nil {
			fmt.Println("orphaned volumes are:", orphanedResourceAvailVols)
			return true
		}
		return false
	}
	//assuming there are orphaned resources as probe can not be done
	return true
}
