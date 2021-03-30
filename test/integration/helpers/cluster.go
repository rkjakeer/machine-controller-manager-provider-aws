package helpers

import (
	mcmClientset "github.com/gardener/machine-controller-manager/pkg/client/clientset/versioned"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

//Cluster type to hold cluster specific details
type Cluster struct {
	restConfig          *rest.Config
	Clientset           *kubernetes.Clientset
	apiextensionsClient *apiextensionsclientset.Clientset
	McmClient           *mcmClientset.Clientset
	kubeConfigFilePath  string
}

// FillClientSets checks whether the cluster is accessible and returns an error if not
func (c *Cluster) FillClientSets() error {
	clientset, err := kubernetes.NewForConfig(c.restConfig)
	if err == nil {
		c.Clientset = clientset
		err = c.ProbeNodes()
		if err != nil {
			return err
		}
		apiextensionsClient, err := apiextensionsclientset.NewForConfig(c.restConfig)
		if err == nil {
			c.apiextensionsClient = apiextensionsClient
		}
		mcmClient, err := mcmClientset.NewForConfig(c.restConfig)
		if err == nil {
			c.McmClient = mcmClient
		}
	}
	return err
}

// NewCluster returns a Cluster struct
func NewCluster(kubeConfigPath string) (c *Cluster, e error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath)
	if err == nil {
		c = &Cluster{
			kubeConfigFilePath: kubeConfigPath,
			restConfig:         config,
		}
	} else {
		c = &Cluster{}
	}

	return c, err
}

// IsSeed checks whether the cluster is seed of target cluster
func (c *Cluster) IsSeed(target *Cluster) bool {
	/*
		- (Check if the control cluster is a seed cluster and
			Try to retrieve the cluster-name (clusters[0].name) from the target kubeconfig passed in.
			 ---- Check if there is any cluster resource available ( means it is a seed cluster ) and see if there is any cluster with name same as to target cluster-name
			 ---- It is not clear as to which client to use for accessing kind: Cluster resources.
			 ---- Alternatively check if there is a namespace with same name as that of cluster name found in kube config
			kubectl get clusters -A
			NAME                             AGE
			shoot--dev--ash-shoot-06022021   46h

	*/
	targetClusterName, _ := target.ClusterName()
	nameSpaces, _ := c.Clientset.CoreV1().Namespaces().List(metav1.ListOptions{})
	for _, namespace := range nameSpaces.Items {
		if namespace.Name == targetClusterName {
			return true
		}
	}
	return false
}

//ClusterName retrieves cluster name from the kubeconfig
func (c *Cluster) ClusterName() (string, error) {
	/*
		- Retrieves cluster name as per the kubeconfig path clusters[0].name
	*/

	var clusterName string
	//kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: c.kubeConfigFilePath},
	//	&clientcmd.ConfigOverrides{})
	config, err := clientcmd.LoadFromFile(c.kubeConfigFilePath)
	if err != nil {
		return clusterName, err
	}
	for contextName, context := range config.Contexts {
		if contextName == config.CurrentContext {
			clusterName = context.Cluster
		}
	}
	return clusterName, err
}
