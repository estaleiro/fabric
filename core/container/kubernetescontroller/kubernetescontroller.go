/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
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
	args []string, env []string, builder container.BuildSpecFactory, prelaunchFunc container.PrelaunchFunc) error {

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

	runtimeImage := viper.GetString("chaincode.golang.runtime")

	kubernetesLogger.Debugf("Start deployment %s at namespace %d and runtime %s", deploymentID, namespace, runtimeImage)

	// Create a deployment with 1 container using args/envs received
	// builder will contain a targz with binpackage
	// that must be extracted in /usr/local/bin
	// after that command received in envs must be executed
	// TODO read dockerfile to get LABELs and ENV CORE_PEER_TLS_ROOTCERT_FILE
	deploymentsClient := client.AppsV1beta1().Deployments(namespace)

	deployment := &appsv1beta1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: deploymentID,
		},
		Spec: appsv1beta1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "fabric",
						"org.hyperledger.fabric.base.version":         "0.3.2",
						"org.hyperledger.fabric.chaincode.id.name":    "mycc",
						"org.hyperledger.fabric.chaincode.id.version": "1.0",
						"org.hyperledger.fabric.chaincode.type":       "GOLANG",
						"org.hyperledger.fabric.version":              "1.0.4",
					},
				},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{
						{
							Name:    "fabric-chaincode-mycc-container",
							Image:   "hub.estaleiro.serpro/bilhetador/fabric-chaincode-mycc:1.0",
							Command: []string{"chaincode", "-peer.address=peer0:7051"},
							Env: []apiv1.EnvVar{
								{Name: "CORE_CHAINCODE_ID_NAME", Value: "mycc:1.0"},
								{Name: "CORE_PEER_TLS_ENABLED", Value: "true"},
								{Name: "CORE_CHAINCODE_LOGGING_LEVEL", Value: "info"},
								{Name: "CORE_CHAINCODE_LOGGING_SHIM", Value: "warning"},
								{Name: "CORE_CHAINCCORE_CHAINCODE_LOGGING_FORMATODE_ID_NAME", Value: "%{color}%{time:2006-01-02 15:04:05.000 MST} [%{module}] %{shortfunc} -> %{level:.4s} %{id:03x}%{color:reset} %{message}"},
								{Name: "PATH", Value: "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
								{Name: "CORE_CHAINCODE_BUILDLEVEL", Value: "1.0.4"},
								{Name: "CORE_PEER_TLS_ROOTCERT_FILE", Value: "/etc/hyperledger/fabric/peer.crt"},
							},
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
