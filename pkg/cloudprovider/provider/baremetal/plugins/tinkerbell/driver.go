/*
Copyright 2021 The Machine Controller Authors.

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

package tinkerbell

import (
	"context"
	"encoding/base64"
	"fmt"

	providerconfigtypes "github.com/kubermatic/machine-controller/pkg/providerconfig/types"

	"github.com/kubermatic/machine-controller/pkg/cloudprovider/provider/baremetal/plugins"
	"github.com/kubermatic/machine-controller/pkg/cloudprovider/provider/baremetal/plugins/tinkerbell/client"
	metadataclient "github.com/kubermatic/machine-controller/pkg/cloudprovider/provider/baremetal/plugins/tinkerbell/metadata"
	tinktypes "github.com/kubermatic/machine-controller/pkg/cloudprovider/provider/baremetal/plugins/tinkerbell/types"
	tinkv1alpha1 "github.com/tinkerbell/tink/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubectl/pkg/scheme"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

type driver struct {
	ClusterName    string
	OSImageURL     string
	HegelURL       string
	TinkClient     ctrlruntimeclient.Client
	KubeClient     ctrlruntimeclient.Client
	HardwareRef    types.NamespacedName
	MetadataClient metadataclient.Client
	HardwareClient client.HardwareClient
	WorkflowClient client.WorkflowClient
	TemplateClient client.TemplateClient
}

func init() {
	// Ensure the Tinkerbell API types are registered with the global scheme.
	if err := tinkv1alpha1.SchemeBuilder.AddToScheme(scheme.Scheme); err != nil {
		panic(fmt.Sprintf("failed to add kubevirtv1 to scheme: %v", err))
	}
}

// NewTinkerbellDriver returns a new TinkerBell driver with a configured tinkserver address and a client timeout.
func NewTinkerbellDriver(mdConfig *metadataclient.Config, tinkConfig tinktypes.Config, tinkSpec *tinktypes.TinkerbellPluginSpec) (plugins.PluginDriver, error) {
	tinkClient, err := ctrlruntimeclient.New(tinkConfig.RestConfig, ctrlruntimeclient.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %w", err)
	}

	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get Kubernetes config: %w", err)
	}

	// Setup the Scheme for Kubernetes types and Tinkerbell CRDs
	k8sClient, err := ctrlruntimeclient.New(cfg, ctrlruntimeclient.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	mdClient, err := metadataclient.NewMetadataClient(mdConfig)

	if err != nil {
		return nil, err
	}

	hwClient := client.NewHardwareClient(k8sClient, tinkClient)

	wkClient := client.NewWorkflowClient(tinkClient)

	tmplClient := client.NewTemplateClient(tinkClient)

	d := driver{
		ClusterName:    tinkSpec.ClusterName.Value,
		TinkClient:     tinkClient,
		HardwareRef:    tinkSpec.HardwareRef,
		KubeClient:     k8sClient,
		MetadataClient: mdClient,
		HardwareClient: *hwClient,
		WorkflowClient: *wkClient,
		TemplateClient: *tmplClient,
		OSImageURL:     tinkSpec.OSImageURL.Value,
		HegelURL:       tinkSpec.HegelURL.Value,
	}

	return &d, nil
}

func (d *driver) GetServer(ctx context.Context, meta metav1.ObjectMeta, _ runtime.RawExtension) (plugins.Server, error) {
	targetHardware, err := d.HardwareClient.GetHardwareWithID(ctx, string(meta.UID))

	if err != nil {
		return nil, err
	}
	server := tinktypes.Hardware{Hardware: targetHardware}
	return &server, nil
}

func (d *driver) ProvisionServer(ctx context.Context, meta metav1.ObjectMeta, _ runtime.RawExtension, userdata string) (plugins.Server, error) {
	hardware, err := d.HardwareClient.GetHardware(ctx, d.HardwareRef)
	if err != nil {
		return nil, err
	}

	err = d.HardwareClient.SetHardwareID(ctx, hardware, string(meta.UID))

	if err != nil {
		return nil, err
	}

	err = d.HardwareClient.SetHardwareUserData(ctx, hardware, userdata)

	if err != nil {
		return nil, err
	}

	err = d.HardwareClient.CreateHardwareOnTinkCluster(ctx, hardware)

	if err != nil {
		return nil, err
	}

	template := &tinkv1alpha1.Template{}

	tmplNamespacedName := types.NamespacedName{Name: meta.Name, Namespace: "tink-stack"}
	if err := d.TinkClient.Get(ctx, tmplNamespacedName, template); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get template: %w", err)
		}
		// Create template if not exists
		template, err = d.TemplateClient.CreateTemplate(ctx, tmplNamespacedName, d.OSImageURL, d.HegelURL)
		if err != nil {
			return nil, err
		}
	}
	server := tinktypes.Hardware{Hardware: hardware}

	err = d.WorkflowClient.CreateWorkflow(ctx, server.Name, template.Name, server)
	if err != nil {
		return nil, err
	}
	return &server, nil
}

func (d *driver) Validate(_ runtime.RawExtension) error {

	return nil
}

func (d *driver) DeprovisionServer(ctx context.Context, meta metav1.ObjectMeta) error {
	targetHardware, err := d.HardwareClient.GetHardwareWithID(ctx, string(meta.UID))

	if err != nil {
		return err
	}

	// Step 3: Delete the associated Workflow
	workflowName := targetHardware.Name + "-workflow" // Assuming workflow names are derived from hardware names
	if err := d.WorkflowClient.DeleteWorkflow(ctx, workflowName, targetHardware.Namespace); err != nil {
		return fmt.Errorf("failed to delete workflow %s: %w", workflowName, err)
	}

	// Step 4: Delete the Hardware
	if err := d.TinkClient.Delete(ctx, targetHardware); err != nil {
		return fmt.Errorf("failed to delete hardware %s: %w", targetHardware.Name, err)
	}

	// Step 5: Reset the hardware ID in the machine-controller cluster
	if err := d.HardwareClient.SetHardwareID(ctx, targetHardware, ""); err != nil {
		return fmt.Errorf("failed to reset hardware ID for %s: %w", targetHardware.Name, err)
	}

	// Step 6: Delete the Template object
	tmplNamespacedName := types.NamespacedName{Name: meta.Name, Namespace: "tink-stack"}
	if err := d.TemplateClient.Delete(ctx, tmplNamespacedName); err != nil {
		return fmt.Errorf("failed to reset hardware ID for %s: %w", targetHardware.Name, err)
	}
	return nil
}

func GetConfig(driverConfig tinktypes.TinkerbellPluginSpec, aa func(configVar providerconfigtypes.ConfigVarString, envVarName string) (string, error)) (*tinktypes.Config, error) {
	config := tinktypes.Config{}
	var err error
	// Kubeconfig was specified directly in the Machine/MachineDeployment CR. In this case we need to ensure that the value is base64 encoded.
	if driverConfig.Auth.Kubeconfig.Value != "" {
		val, err := base64.StdEncoding.DecodeString(driverConfig.Auth.Kubeconfig.Value)
		if err != nil {
			// An error here means that this is not a valid base64 string
			// We can be more explicit here with the error for visibility. Webhook will return this error if we hit this scenario.
			return nil, fmt.Errorf("failed to decode base64 encoded kubeconfig. Expected value is a base64 encoded Kubeconfig in JSON or YAML format: %w", err)
		}
		config.Kubeconfig = string(val)
	} else {
		// Environment variable or secret reference was used for providing the value of kubeconfig
		// We have to be lenient in this case and allow unencoded values as well.
		config.Kubeconfig, err = aa(driverConfig.Auth.Kubeconfig, "TINK_KUBECONFIG")
		if err != nil {
			return nil, fmt.Errorf(`failed to get value of "kubeconfig" field: %w`, err)
		}
	}
	config.ClusterName, err = aa(driverConfig.ClusterName, "CLUSTER_NAME")
	if err != nil {
		return nil, fmt.Errorf(`failed to get value of "clusterName" field: %w`, err)
	}

	config.OSImageURL, err = aa(driverConfig.OSImageURL, "OS_IMAGE_URL")
	if err != nil {
		return nil, fmt.Errorf(`failed to get value of "OSImageURL" field: %w`, err)
	}

	config.HegelURL, err = aa(driverConfig.HegelURL, "HEGEL_URL")
	if err != nil {
		return nil, fmt.Errorf(`failed to get value of "HegelURL" field: %w`, err)
	}

	config.RestConfig, err = clientcmd.RESTConfigFromKubeConfig([]byte(config.Kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("failed to decode kubeconfig: %w", err)
	}
	return &config, nil
}
