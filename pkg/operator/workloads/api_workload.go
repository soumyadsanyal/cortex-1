/*
Copyright 2019 Cortex Labs, Inc.

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

package workloads

import (
	"path"

	kapps "k8s.io/api/apps/v1"
	kcore "k8s.io/api/core/v1"
	kextensions "k8s.io/api/extensions/v1beta1"
	kresource "k8s.io/apimachinery/pkg/api/resource"
	intstr "k8s.io/apimachinery/pkg/util/intstr"

	"github.com/cortexlabs/cortex/pkg/consts"
	"github.com/cortexlabs/cortex/pkg/lib/errors"
	"github.com/cortexlabs/cortex/pkg/lib/k8s"
	"github.com/cortexlabs/cortex/pkg/operator/api/context"
	"github.com/cortexlabs/cortex/pkg/operator/api/userconfig"
	"github.com/cortexlabs/cortex/pkg/operator/config"
)

const (
	apiContainerName       = "api"
	tfServingContainerName = "serve"

	defaultPortInt32, defaultPortStr     = int32(8888), "8888"
	tfServingPortInt32, tfServingPortStr = int32(9000), "9000"
)

type APIWorkload struct {
	BaseWorkload
}

func populateAPIWorkloadIDs(ctx *context.Context, latestResourceWorkloadIDs map[string]string) {
	for _, api := range ctx.APIs {
		if api.WorkloadID != "" {
			continue
		}
		if workloadID := latestResourceWorkloadIDs[api.ID]; workloadID != "" {
			api.WorkloadID = workloadID
			continue
		}
		api.WorkloadID = generateWorkloadID()
	}
}

func extractAPIWorkloads(ctx *context.Context) []Workload {
	workloads := make([]Workload, 0, len(ctx.APIs))

	for _, api := range ctx.APIs {
		workloads = append(workloads, &APIWorkload{
			singleBaseWorkload(api, ctx.App.Name, workloadTypeAPI),
		})
	}

	return workloads
}

func (aw *APIWorkload) Start(ctx *context.Context) error {
	api := ctx.APIs.OneByID(aw.GetSingleResourceID())

	k8sDeloymentName := internalAPIName(api.Name, ctx.App.Name)
	k8sDeloyment, err := config.Kubernetes.GetDeployment(k8sDeloymentName)
	if err != nil {
		return err
	}
	hpa, err := config.Kubernetes.GetHPA(k8sDeloymentName)
	if err != nil {
		return err
	}

	desiredReplicas := getRequestedReplicasFromDeployment(api, k8sDeloyment, hpa)

	var deploymentSpec *kapps.Deployment

	switch api.ModelFormat {
	case userconfig.TensorFlowModelFormat:
		deploymentSpec = tfAPISpec(ctx, api, aw.WorkloadID, desiredReplicas)
	case userconfig.ONNXModelFormat:
		deploymentSpec = onnxAPISpec(ctx, api, aw.WorkloadID, desiredReplicas)
	default:
		return errors.New(api.Name, "unknown model format encountered") // unexpected
	}

	_, err = config.Kubernetes.ApplyIngress(ingressSpec(ctx, api))
	if err != nil {
		return err
	}

	_, err = config.Kubernetes.ApplyService(serviceSpec(ctx, api))
	if err != nil {
		return err
	}

	_, err = config.Kubernetes.ApplyDeployment(deploymentSpec)
	if err != nil {
		return err
	}

	// Delete HPA while updating replicas to avoid unwanted autoscaling
	_, err = config.Kubernetes.DeleteHPA(k8sDeloymentName)
	if err != nil {
		return err
	}

	return nil
}

func (aw *APIWorkload) IsSucceeded(ctx *context.Context) (bool, error) {
	api := ctx.APIs.OneByID(aw.GetSingleResourceID())
	k8sDeloymentName := internalAPIName(api.Name, ctx.App.Name)

	k8sDeployment, err := config.Kubernetes.GetDeployment(k8sDeloymentName)
	if err != nil {
		return false, err
	}
	if k8sDeployment == nil || k8sDeployment.Labels["resourceID"] != api.ID || k8sDeployment.DeletionTimestamp != nil {
		return false, nil
	}

	if doesAPIComputeNeedsUpdating(api, k8sDeployment) {
		return false, nil
	}

	updatedReplicas, err := numUpdatedReadyReplicas(ctx, api)
	if err != nil {
		return false, err
	}
	requestedReplicas := getRequestedReplicasFromDeployment(api, k8sDeployment, nil)
	if updatedReplicas < requestedReplicas {
		return false, nil
	}

	return true, nil
}

func (aw *APIWorkload) IsRunning(ctx *context.Context) (bool, error) {
	api := ctx.APIs.OneByID(aw.GetSingleResourceID())
	k8sDeloymentName := internalAPIName(api.Name, ctx.App.Name)

	k8sDeployment, err := config.Kubernetes.GetDeployment(k8sDeloymentName)
	if err != nil {
		return false, err
	}
	if k8sDeployment == nil || k8sDeployment.Labels["resourceID"] != api.ID || k8sDeployment.DeletionTimestamp != nil {
		return false, nil
	}

	if doesAPIComputeNeedsUpdating(api, k8sDeployment) {
		return false, nil
	}

	updatedReplicas, err := numUpdatedReadyReplicas(ctx, api)
	if err != nil {
		return false, err
	}
	requestedReplicas := getRequestedReplicasFromDeployment(api, k8sDeployment, nil)
	if updatedReplicas < requestedReplicas {
		return true, nil
	}

	return false, nil
}

func (aw *APIWorkload) IsStarted(ctx *context.Context) (bool, error) {
	api := ctx.APIs.OneByID(aw.GetSingleResourceID())
	k8sDeloymentName := internalAPIName(api.Name, ctx.App.Name)

	k8sDeployment, err := config.Kubernetes.GetDeployment(k8sDeloymentName)
	if err != nil {
		return false, err
	}
	if k8sDeployment == nil || k8sDeployment.Labels["resourceID"] != api.ID || k8sDeployment.DeletionTimestamp != nil {
		return false, nil
	}

	if doesAPIComputeNeedsUpdating(api, k8sDeployment) {
		return false, nil
	}

	return true, nil
}

func (aw *APIWorkload) CanRun(ctx *context.Context) (bool, error) {
	return areAllDataDependenciesSucceeded(ctx, aw.GetResourceIDs())
}

func (aw *APIWorkload) IsFailed(ctx *context.Context) (bool, error) {
	api := ctx.APIs.OneByID(aw.GetSingleResourceID())

	pods, err := config.Kubernetes.ListPodsByLabels(map[string]string{
		"appName":      ctx.App.Name,
		"workloadType": workloadTypeAPI,
		"apiName":      api.Name,
		"resourceID":   api.ID,
		"workloadID":   aw.GetWorkloadID(),
		"userFacing":   "true",
	})
	if err != nil {
		return false, err
	}

	for _, pod := range pods {
		if k8s.GetPodStatus(&pod) == k8s.PodStatusFailed {
			return true, nil
		}
	}

	return false, nil
}

func tfAPISpec(
	ctx *context.Context,
	api *context.API,
	workloadID string,
	desiredReplicas int32,
) *kapps.Deployment {
	transformResourceList := kcore.ResourceList{}
	tfServingResourceList := kcore.ResourceList{}
	tfServingLimitsList := kcore.ResourceList{}

	q1, q2 := api.Compute.CPU.SplitInTwo()
	transformResourceList[kcore.ResourceCPU] = *q1
	tfServingResourceList[kcore.ResourceCPU] = *q2

	if api.Compute.Mem != nil {
		q1, q2 := api.Compute.Mem.SplitInTwo()
		transformResourceList[kcore.ResourceMemory] = *q1
		tfServingResourceList[kcore.ResourceMemory] = *q2
	}

	servingImage := config.Cortex.TFServeImage
	if api.Compute.GPU > 0 {
		servingImage = config.Cortex.TFServeImageGPU
		tfServingResourceList["nvidia.com/gpu"] = *kresource.NewQuantity(api.Compute.GPU, kresource.DecimalSI)
		tfServingLimitsList["nvidia.com/gpu"] = *kresource.NewQuantity(api.Compute.GPU, kresource.DecimalSI)
	}

	return k8s.Deployment(&k8s.DeploymentSpec{
		Name:     internalAPIName(api.Name, ctx.App.Name),
		Replicas: desiredReplicas,
		Labels: map[string]string{
			"appName":      ctx.App.Name,
			"workloadType": workloadTypeAPI,
			"apiName":      api.Name,
			"resourceID":   ctx.APIs[api.Name].ID,
			"workloadID":   workloadID,
		},
		Selector: map[string]string{
			"appName":      ctx.App.Name,
			"workloadType": workloadTypeAPI,
			"apiName":      api.Name,
		},
		PodSpec: k8s.PodSpec{
			Labels: map[string]string{
				"appName":      ctx.App.Name,
				"workloadType": workloadTypeAPI,
				"apiName":      api.Name,
				"resourceID":   ctx.APIs[api.Name].ID,
				"workloadID":   workloadID,
				"userFacing":   "true",
			},
			K8sPodSpec: kcore.PodSpec{
				Containers: []kcore.Container{
					{
						Name:            apiContainerName,
						Image:           config.Cortex.TFAPIImage,
						ImagePullPolicy: "Always",
						Args: []string{
							"--workload-id=" + workloadID,
							"--port=" + defaultPortStr,
							"--tf-serve-port=" + tfServingPortStr,
							"--context=" + config.AWS.S3Path(ctx.Key),
							"--api=" + ctx.APIs[api.Name].ID,
							"--model-dir=" + path.Join(consts.EmptyDirMountPath, "model"),
							"--cache-dir=" + consts.ContextCacheDir,
						},
						Env:          k8s.AWSCredentials(),
						VolumeMounts: k8s.DefaultVolumeMounts(),
						ReadinessProbe: &kcore.Probe{
							InitialDelaySeconds: 5,
							TimeoutSeconds:      5,
							PeriodSeconds:       5,
							SuccessThreshold:    1,
							FailureThreshold:    2,
							Handler: kcore.Handler{
								HTTPGet: &kcore.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.IntOrString{
										IntVal: defaultPortInt32,
									},
								},
							},
						},
						Resources: kcore.ResourceRequirements{
							Requests: transformResourceList,
						},
					},
					{
						Name:            tfServingContainerName,
						Image:           servingImage,
						ImagePullPolicy: "Always",
						Args: []string{
							"--port=" + tfServingPortStr,
							"--model_base_path=" + path.Join(consts.EmptyDirMountPath, "model"),
						},
						Env:          k8s.AWSCredentials(),
						VolumeMounts: k8s.DefaultVolumeMounts(),
						ReadinessProbe: &kcore.Probe{
							InitialDelaySeconds: 5,
							TimeoutSeconds:      5,
							PeriodSeconds:       5,
							SuccessThreshold:    1,
							FailureThreshold:    2,
							Handler: kcore.Handler{
								TCPSocket: &kcore.TCPSocketAction{
									Port: intstr.IntOrString{
										IntVal: tfServingPortInt32,
									},
								},
							},
						},
						Resources: kcore.ResourceRequirements{
							Requests: tfServingResourceList,
							Limits:   tfServingLimitsList,
						},
					},
				},
				Volumes:            k8s.DefaultVolumes(),
				ServiceAccountName: "default",
			},
		},
		Namespace: config.Cortex.Namespace,
	})
}

func onnxAPISpec(
	ctx *context.Context,
	api *context.API,
	workloadID string,
	desiredReplicas int32,
) *kapps.Deployment {
	servingImage := config.Cortex.ONNXServeImage
	resourceList := kcore.ResourceList{}
	resourceLimitsList := kcore.ResourceList{}
	resourceList[kcore.ResourceCPU] = api.Compute.CPU.Quantity

	if api.Compute.Mem != nil {
		resourceList[kcore.ResourceMemory] = api.Compute.Mem.Quantity
	}

	if api.Compute.GPU > 0 {
		servingImage = config.Cortex.ONNXServeImageGPU
		resourceList["nvidia.com/gpu"] = *kresource.NewQuantity(api.Compute.GPU, kresource.DecimalSI)
		resourceLimitsList["nvidia.com/gpu"] = *kresource.NewQuantity(api.Compute.GPU, kresource.DecimalSI)
	}

	return k8s.Deployment(&k8s.DeploymentSpec{
		Name:     internalAPIName(api.Name, ctx.App.Name),
		Replicas: desiredReplicas,
		Labels: map[string]string{
			"appName":      ctx.App.Name,
			"workloadType": workloadTypeAPI,
			"apiName":      api.Name,
			"resourceID":   ctx.APIs[api.Name].ID,
			"workloadID":   workloadID,
		},
		Selector: map[string]string{
			"appName":      ctx.App.Name,
			"workloadType": workloadTypeAPI,
			"apiName":      api.Name,
		},
		PodSpec: k8s.PodSpec{
			Labels: map[string]string{
				"appName":      ctx.App.Name,
				"workloadType": workloadTypeAPI,
				"apiName":      api.Name,
				"resourceID":   ctx.APIs[api.Name].ID,
				"workloadID":   workloadID,
				"userFacing":   "true",
			},
			K8sPodSpec: kcore.PodSpec{
				Containers: []kcore.Container{
					{
						Name:            apiContainerName,
						Image:           servingImage,
						ImagePullPolicy: "Always",
						Args: []string{
							"--workload-id=" + workloadID,
							"--port=" + defaultPortStr,
							"--context=" + config.AWS.S3Path(ctx.Key),
							"--api=" + ctx.APIs[api.Name].ID,
							"--model-dir=" + path.Join(consts.EmptyDirMountPath, "model"),
							"--cache-dir=" + consts.ContextCacheDir,
						},
						Env:          k8s.AWSCredentials(),
						VolumeMounts: k8s.DefaultVolumeMounts(),
						ReadinessProbe: &kcore.Probe{
							InitialDelaySeconds: 5,
							TimeoutSeconds:      5,
							PeriodSeconds:       5,
							SuccessThreshold:    1,
							FailureThreshold:    2,
							Handler: kcore.Handler{
								HTTPGet: &kcore.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.IntOrString{
										IntVal: defaultPortInt32,
									},
								},
							},
						},
						Resources: kcore.ResourceRequirements{
							Requests: resourceList,
							Limits:   resourceLimitsList,
						},
					},
				},
				Volumes:            k8s.DefaultVolumes(),
				ServiceAccountName: "default",
			},
		},
		Namespace: config.Cortex.Namespace,
	})
}

func ingressSpec(ctx *context.Context, api *context.API) *kextensions.Ingress {
	return k8s.Ingress(&k8s.IngressSpec{
		Name:         internalAPIName(api.Name, ctx.App.Name),
		ServiceName:  internalAPIName(api.Name, ctx.App.Name),
		ServicePort:  defaultPortInt32,
		Path:         context.APIPath(api.Name, ctx.App.Name),
		IngressClass: "apis",
		Labels: map[string]string{
			"appName":      ctx.App.Name,
			"workloadType": workloadTypeAPI,
			"apiName":      api.Name,
		},
		Namespace: config.Cortex.Namespace,
	})
}

func serviceSpec(ctx *context.Context, api *context.API) *kcore.Service {
	return k8s.Service(&k8s.ServiceSpec{
		Name:       internalAPIName(api.Name, ctx.App.Name),
		Port:       defaultPortInt32,
		TargetPort: defaultPortInt32,
		Labels: map[string]string{
			"appName":      ctx.App.Name,
			"workloadType": workloadTypeAPI,
			"apiName":      api.Name,
		},
		Selector: map[string]string{
			"appName":      ctx.App.Name,
			"workloadType": workloadTypeAPI,
			"apiName":      api.Name,
		},
		Namespace: config.Cortex.Namespace,
	})
}

func doesAPIComputeNeedsUpdating(api *context.API, deployment *kapps.Deployment) bool {
	curCPU, curMem, curGPU := APIPodCompute(deployment.Spec.Template.Spec.Containers)
	if !userconfig.QuantityPtrsEqual(curCPU, &api.Compute.CPU) {
		return true
	}
	if !userconfig.QuantityPtrsEqual(curMem, api.Compute.Mem) {
		return true
	}
	if curGPU != api.Compute.GPU {
		return true
	}

	return false
}

func deleteOldAPIs(ctx *context.Context) {
	ingresses, _ := config.Kubernetes.ListIngressesByLabels(map[string]string{
		"appName":      ctx.App.Name,
		"workloadType": workloadTypeAPI,
	})
	for _, ingress := range ingresses {
		if _, ok := ctx.APIs[ingress.Labels["apiName"]]; !ok {
			config.Kubernetes.DeleteIngress(ingress.Name)
		}
	}

	services, _ := config.Kubernetes.ListServicesByLabels(map[string]string{
		"appName":      ctx.App.Name,
		"workloadType": workloadTypeAPI,
	})
	for _, service := range services {
		if _, ok := ctx.APIs[service.Labels["apiName"]]; !ok {
			config.Kubernetes.DeleteService(service.Name)
		}
	}

	deployments, _ := config.Kubernetes.ListDeploymentsByLabels(map[string]string{
		"appName":      ctx.App.Name,
		"workloadType": workloadTypeAPI,
	})
	for _, deployment := range deployments {
		if _, ok := ctx.APIs[deployment.Labels["apiName"]]; !ok {
			config.Kubernetes.DeleteDeployment(deployment.Name)
		}
	}

	hpas, _ := config.Kubernetes.ListHPAsByLabels(map[string]string{
		"appName":      ctx.App.Name,
		"workloadType": workloadTypeAPI,
	})
	for _, hpa := range hpas {
		if _, ok := ctx.APIs[hpa.Labels["apiName"]]; !ok {
			config.Kubernetes.DeleteHPA(hpa.Name)
		}
	}
}

// This returns map apiName -> deployment (not internalName -> deployment)
func apiDeploymentMap(appName string) (map[string]*kapps.Deployment, error) {
	deploymentList, err := config.Kubernetes.ListDeploymentsByLabels(map[string]string{
		"appName":      appName,
		"workloadType": workloadTypeAPI,
	})
	if err != nil {
		return nil, errors.Wrap(err, appName)
	}

	deployments := make(map[string]*kapps.Deployment, len(deploymentList))
	for _, deployment := range deploymentList {
		addToDeploymentMap(deployments, deployment)
	}
	return deployments, nil
}

// Avoid pointer in loop issues
func addToDeploymentMap(deployments map[string]*kapps.Deployment, deployment kapps.Deployment) {
	apiName := deployment.Labels["apiName"]
	deployments[apiName] = &deployment
}

func internalAPIName(apiName string, appName string) string {
	return appName + "----" + apiName
}

func APIsBaseURL() (string, error) {
	service, err := config.Kubernetes.GetService("nginx-controller-apis")
	if err != nil {
		return "", err
	}
	if service == nil {
		return "", ErrorCortexInstallationBroken()
	}
	if len(service.Status.LoadBalancer.Ingress) == 0 {
		return "", ErrorLoadBalancerInitializing()
	}
	return "https://" + service.Status.LoadBalancer.Ingress[0].Hostname, nil
}

func APIPodComputeID(containers []kcore.Container) string {
	cpu, mem, gpu := APIPodCompute(containers)
	if cpu == nil {
		cpu = &userconfig.Quantity{} // unexpected, since 0 is disallowed
	}
	podAPICompute := userconfig.APICompute{
		CPU: *cpu,
		Mem: mem,
		GPU: gpu,
	}
	return podAPICompute.IDWithoutReplicas()
}

func APIPodCompute(containers []kcore.Container) (*userconfig.Quantity, *userconfig.Quantity, int64) {
	var totalCPU *userconfig.Quantity
	var totalMem *userconfig.Quantity
	var totalGPU int64

	for _, container := range containers {
		if container.Name != apiContainerName && container.Name != tfServingContainerName {
			continue
		}

		requests := container.Resources.Requests
		if len(requests) == 0 {
			continue
		}

		if cpu, ok := requests[kcore.ResourceCPU]; ok {
			if totalCPU == nil {
				totalCPU = &userconfig.Quantity{}
			}
			totalCPU.Add(cpu)
		}
		if mem, ok := requests[kcore.ResourceMemory]; ok {
			if totalMem == nil {
				totalMem = &userconfig.Quantity{}
			}
			totalMem.Add(mem)
		}
		if gpu, ok := requests["nvidia.com/gpu"]; ok {
			gpuVal, ok := gpu.AsInt64()
			if ok {
				totalGPU += gpuVal
			}
		}
	}

	return totalCPU, totalMem, totalGPU
}