package helpers

import (
	v1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	v1 "k8s.io/api/core/v1"
)

//ResourcesTrackerInterface provides an interface to check for orphan resources.
//The implementation should handle probing for resources while contructing or calling New method
//And reporting orphan resources whenever IsOrphanedResourcesAvailable is invoked
type ResourcesTrackerInterface interface {
	IsOrphanedResourcesAvailable() bool
	InitializeResourcesTracker(machineClass *v1alpha1.MachineClass, secret *v1.Secret, clusterName string) error
}
