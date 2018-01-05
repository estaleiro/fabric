/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubernetescontroller

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/hyperledger/fabric/common/flogging"
	container "github.com/hyperledger/fabric/core/container/api"
	"github.com/hyperledger/fabric/core/container/ccintf"
	cutil "github.com/hyperledger/fabric/core/container/util"
	"github.com/spf13/viper"
	"golang.org/x/net/context"
	appsv1beta1 "k8s.io/api/apps/v1beta1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	kubernetesLogger = flogging.MustGetLogger("kubernetescontroller")
	vmRegExp         = regexp.MustCompile("[^a-zA-Z0-9-_.]")
)

// getClient returns an instance for kubernetes.Clientset
type getClient func() (*kubernetes.Clientset, error)

// KubernetesVM is a vm. It is identified by an image id
type KubernetesVM struct {
	id           string
	getClientFnc getClient
}

// NewKubernetesVM returns a new KubernetesVM instance
func NewKubernetesVM() *KubernetesVM {
	vm := KubernetesVM{}
	vm.getClientFnc = getKubernetesClient
	return &vm
}

func getKubernetesClient() (*kubernetes.Clientset, error) {
	return cutil.NewKubernetesClient()
}

//Deploy not used yet
func (vm *KubernetesVM) Deploy(ctxt context.Context, ccid ccintf.CCID,
	args []string, env []string, reader io.Reader) error {
	return nil
}

//Start starts a container using a previously created docker image
func (vm *KubernetesVM) Start(ctxt context.Context, ccid ccintf.CCID,
	args []string, env []string, filesToUpload map[string][]byte, builder container.BuildSpecFactory, prelaunchFunc container.PrelaunchFunc) error {

	client, err := vm.getClientFnc()
	if err != nil {
		kubernetesLogger.Debugf("start - cannot create client %s", err)
		return err
	}

	deploymentID, err := vm.GetVMName(ccid, nil)
	if err != nil {
		return err
	}

	// Delete the deployment if is running
	kubernetesLogger.Debugf("Cleanup deployment %s", deploymentID)
	vm.stopInternal(ctxt, client, deploymentID, 0, false, false)

	namespace := apiv1.NamespaceDefault

	if viper.IsSet("peer.kubernetes.namespace") {
		namespace = viper.GetString("peer.kubernetes.namespace")
	}

	builderImage := viper.GetString("chaincode.builder")

	//TODO find way to define what platform is
	runtimeImage := viper.GetString("chaincode.golang.runtime")

	kubernetesLogger.Debugf("Start deployment %s at namespace %d using builder %s and runtime %s", deploymentID, namespace, builderImage, runtimeImage)

	// Create a deployment with 2 containers that will share a volume
	//   a) builder using the image at chaincode.builder. He will generate the binary
	//   He will run "GOPATH=/chaincode/input:$GOPATH go build -tags \"%s\" %s -o /chaincode/output/chaincode %s"
	//   b) runtime using the image at chaincode.golang.runtime (for example)
	//   using envs and fileToUpload from chaincode_support.getLaunchConfigs

	deploymentsClient := client.AppsV1beta1().Deployments(namespace)

	deployment := &appsv1beta1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: deploymentID,
		},
		Spec: appsv1beta1.DeploymentSpec{
			Replicas: int32Ptr(2),
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "demo",
					},
				},
				Spec: apiv1.PodSpec{
					InitContainers: []apiv1.Container{
						{
							Name:  "builder",
							Image: builderImage,
						},
					},
					Containers: []apiv1.Container{
						{
							Name:  "runtime",
							Image: runtimeImage,
						},
					},
				},
			},
		},
	}

	// Create Deployment
	_, err = deploymentsClient.Create(deployment)
	if err != nil {
		kubernetesLogger.Errorf("start-could not create deployment <%s>, because of %s", deploymentID, err)
		return err
	}

	kubernetesLogger.Debugf("Started deployment %s", deploymentID)

	return nil
}

//Stop stops a running chaincode
func (vm *KubernetesVM) Stop(ctxt context.Context, ccid ccintf.CCID, timeout uint, dontkill bool, dontremove bool) error {
	id, err := vm.GetVMName(ccid, nil)
	if err != nil {
		return err
	}

	client, err := vm.getClientFnc()
	if err != nil {
		kubernetesLogger.Debugf("stop - cannot create client %s", err)
		return err
	}
	id = strings.Replace(id, ":", "_", -1)

	err = vm.stopInternal(ctxt, client, id, timeout, dontkill, dontremove)

	return err
}

func (vm *KubernetesVM) stopInternal(ctxt context.Context, client *kubernetes.Clientset,
	id string, timeout uint, dontkill bool, dontremove bool) error {

	deploymentsClient := client.AppsV1beta1().Deployments(apiv1.NamespaceDefault)

	deletePolicy := metav1.DeletePropagationForeground

	err := deploymentsClient.Delete(id, &metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	})

	if err != nil {
		kubernetesLogger.Debugf("Delete deployment %s (%s)", id, err)
	} else {
		kubernetesLogger.Debugf("Deleted deployment %s", id)
	}

	return err
}

//Destroy not used yet
func (vm *KubernetesVM) Destroy(ctxt context.Context, ccid ccintf.CCID, force bool, noprune bool) error {
	return nil
}

// GetVMName generates the VM name from peer information. It accepts a format
// function parameter to allow different formatting based on the desired use of
// the name.
func (vm *KubernetesVM) GetVMName(ccid ccintf.CCID, format func(string) (string, error)) (string, error) {
	name := ccid.GetName()

	if ccid.NetworkID != "" && ccid.PeerID != "" {
		name = fmt.Sprintf("%s-%s-%s", ccid.NetworkID, ccid.PeerID, name)
	} else if ccid.NetworkID != "" {
		name = fmt.Sprintf("%s-%s", ccid.NetworkID, name)
	} else if ccid.PeerID != "" {
		name = fmt.Sprintf("%s-%s", ccid.PeerID, name)
	}

	if format != nil {
		formattedName, err := format(name)
		if err != nil {
			return formattedName, err
		}
		name = formattedName
	}

	// replace any invalid characters with "-" (either in network id, peer id, or in the
	// entire name returned by any format function)
	name = vmRegExp.ReplaceAllString(name, "-")

	return name, nil
}

func int32Ptr(i int32) *int32 { return &i }
