/*
Copyright 2021 The KEDA Authors

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

package scalers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	az "github.com/Azure/go-autorest/autorest/azure"
	v2beta2 "k8s.io/api/autoscaling/v2beta2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/metrics/pkg/apis/external_metrics"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/kedacore/keda/v2/pkg/scalers/azure"
	kedautil "github.com/kedacore/keda/v2/pkg/util"
)

const (
	azureMonitorMetricName = "metricName"
	targetValueName        = "targetValue"
)

type azureMonitorScaler struct {
	metricType  v2beta2.MetricTargetType
	metadata    *azureMonitorMetadata
	podIdentity kedav1alpha1.PodIdentityProvider
}

type azureMonitorMetadata struct {
	azureMonitorInfo azure.MonitorInfo
	targetValue      int64
	scalerIndex      int
}

var azureMonitorLog = logf.Log.WithName("azure_monitor_scaler")

// NewAzureMonitorScaler creates a new AzureMonitorScaler
func NewAzureMonitorScaler(config *ScalerConfig) (Scaler, error) {
	metricType, err := GetMetricTargetType(config)
	if err != nil {
		return nil, fmt.Errorf("error getting scaler metric type: %s", err)
	}

	meta, err := parseAzureMonitorMetadata(config)
	if err != nil {
		return nil, fmt.Errorf("error parsing azure monitor metadata: %s", err)
	}

	return &azureMonitorScaler{
		metricType:  metricType,
		metadata:    meta,
		podIdentity: config.PodIdentity,
	}, nil
}

func parseAzureMonitorMetadata(config *ScalerConfig) (*azureMonitorMetadata, error) {
	meta := azureMonitorMetadata{
		azureMonitorInfo: azure.MonitorInfo{},
	}

	if val, ok := config.TriggerMetadata[targetValueName]; ok && val != "" {
		targetValue, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			azureMonitorLog.Error(err, "Error parsing azure monitor metadata", "targetValue", targetValueName)
			return nil, fmt.Errorf("error parsing azure monitor metadata %s: %s", targetValueName, err.Error())
		}
		meta.targetValue = targetValue
	} else {
		return nil, fmt.Errorf("no targetValue given")
	}

	if val, ok := config.TriggerMetadata["resourceURI"]; ok && val != "" {
		resourceURI := strings.Split(val, "/")
		if len(resourceURI) != 3 {
			return nil, fmt.Errorf("resourceURI not in the correct format. Should be namespace/resource_type/resource_name")
		}
		meta.azureMonitorInfo.ResourceURI = val
	} else {
		return nil, fmt.Errorf("no resourceURI given")
	}

	if val, ok := config.TriggerMetadata["resourceGroupName"]; ok && val != "" {
		meta.azureMonitorInfo.ResourceGroupName = val
	} else {
		return nil, fmt.Errorf("no resourceGroupName given")
	}

	if val, ok := config.TriggerMetadata[azureMonitorMetricName]; ok && val != "" {
		meta.azureMonitorInfo.Name = val
	} else {
		return nil, fmt.Errorf("no metricName given")
	}

	if val, ok := config.TriggerMetadata["metricAggregationType"]; ok && val != "" {
		meta.azureMonitorInfo.AggregationType = val
	} else {
		return nil, fmt.Errorf("no metricAggregationType given")
	}

	if val, ok := config.TriggerMetadata["metricFilter"]; ok && val != "" {
		meta.azureMonitorInfo.Filter = val
	}

	if val, ok := config.TriggerMetadata["metricAggregationInterval"]; ok && val != "" {
		aggregationInterval := strings.Split(val, ":")
		if len(aggregationInterval) != 3 {
			return nil, fmt.Errorf("metricAggregationInterval not in the correct format. Should be hh:mm:ss")
		}
		meta.azureMonitorInfo.AggregationInterval = val
	}

	// Required authentication parameters below

	if val, ok := config.TriggerMetadata["subscriptionId"]; ok && val != "" {
		meta.azureMonitorInfo.SubscriptionID = val
	} else {
		return nil, fmt.Errorf("no subscriptionId given")
	}

	if val, ok := config.TriggerMetadata["tenantId"]; ok && val != "" {
		meta.azureMonitorInfo.TenantID = val
	} else {
		return nil, fmt.Errorf("no tenantId given")
	}

	if val, ok := config.TriggerMetadata["metricNamespace"]; ok {
		meta.azureMonitorInfo.Namespace = val
	}

	clientID, clientPassword, err := parseAzurePodIdentityParams(config)
	if err != nil {
		return nil, err
	}
	meta.azureMonitorInfo.ClientID = clientID
	meta.azureMonitorInfo.ClientPassword = clientPassword

	meta.scalerIndex = config.ScalerIndex

	azureResourceManagerEndpointProvider := func(env az.Environment) (string, error) {
		return env.ResourceManagerEndpoint, nil
	}
	azureResourceManagerEndpoint, err := azure.ParseEnvironmentProperty(config.TriggerMetadata, "azureResourceManagerEndpoint", azureResourceManagerEndpointProvider)
	if err != nil {
		return nil, err
	}
	meta.azureMonitorInfo.AzureResourceManagerEndpoint = azureResourceManagerEndpoint

	activeDirectoryEndpoint, err := azure.ParseActiveDirectoryEndpoint(config.TriggerMetadata)
	if err != nil {
		return nil, err
	}
	meta.azureMonitorInfo.ActiveDirectoryEndpoint = activeDirectoryEndpoint

	return &meta, nil
}

// parseAzurePodIdentityParams gets the activeDirectory clientID and password
func parseAzurePodIdentityParams(config *ScalerConfig) (clientID string, clientPassword string, err error) {
	switch config.PodIdentity {
	case "", kedav1alpha1.PodIdentityProviderNone:
		clientID, err = getParameterFromConfig(config, "activeDirectoryClientId", true)
		if err != nil || clientID == "" {
			return "", "", fmt.Errorf("no activeDirectoryClientId given")
		}

		if config.AuthParams["activeDirectoryClientPassword"] != "" {
			clientPassword = config.AuthParams["activeDirectoryClientPassword"]
		} else if config.TriggerMetadata["activeDirectoryClientPasswordFromEnv"] != "" {
			clientPassword = config.ResolvedEnv[config.TriggerMetadata["activeDirectoryClientPasswordFromEnv"]]
		}

		if len(clientPassword) == 0 {
			return "", "", fmt.Errorf("no activeDirectoryClientPassword given")
		}
	case kedav1alpha1.PodIdentityProviderAzure, kedav1alpha1.PodIdentityProviderAzureWorkload:
		// no params required to be parsed
	default:
		return "", "", fmt.Errorf("azure Monitor doesn't support pod identity %s", config.PodIdentity)
	}

	return clientID, clientPassword, nil
}

// Returns true if the Azure Monitor metric value is greater than zero
func (s *azureMonitorScaler) IsActive(ctx context.Context) (bool, error) {
	val, err := azure.GetAzureMetricValue(ctx, s.metadata.azureMonitorInfo, s.podIdentity)
	if err != nil {
		azureMonitorLog.Error(err, "error getting azure monitor metric")
		return false, err
	}

	return val > 0, nil
}

func (s *azureMonitorScaler) Close(context.Context) error {
	return nil
}

func (s *azureMonitorScaler) GetMetricSpecForScaling(context.Context) []v2beta2.MetricSpec {
	externalMetric := &v2beta2.ExternalMetricSource{
		Metric: v2beta2.MetricIdentifier{
			Name: GenerateMetricNameWithIndex(s.metadata.scalerIndex, kedautil.NormalizeString(fmt.Sprintf("azure-monitor-%s", s.metadata.azureMonitorInfo.Name))),
		},
		Target: GetMetricTarget(s.metricType, s.metadata.targetValue),
	}
	metricSpec := v2beta2.MetricSpec{External: externalMetric, Type: externalMetricType}
	return []v2beta2.MetricSpec{metricSpec}
}

// GetMetrics returns value for a supported metric and an error if there is a problem getting the metric
func (s *azureMonitorScaler) GetMetrics(ctx context.Context, metricName string, metricSelector labels.Selector) ([]external_metrics.ExternalMetricValue, error) {
	val, err := azure.GetAzureMetricValue(ctx, s.metadata.azureMonitorInfo, s.podIdentity)
	if err != nil {
		azureMonitorLog.Error(err, "error getting azure monitor metric")
		return []external_metrics.ExternalMetricValue{}, err
	}

	metric := external_metrics.ExternalMetricValue{
		MetricName: metricName,
		Value:      *resource.NewQuantity(val, resource.DecimalSI),
		Timestamp:  metav1.Now(),
	}

	return append([]external_metrics.ExternalMetricValue{}, metric), nil
}
