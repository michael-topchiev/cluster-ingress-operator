package ingress

import (
	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-ingress-operator/pkg/operator/controller"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func foo(ci *operatorv1.IngressController, deployment *appsv1.Deployment, ingressConfig *configv1.Ingress, infraConfig *configv1.Infrastructure) bool {
	desiredReplicas := determineDeploymentReplicas(ci, ingressConfig, infraConfig)
	deployment.Spec.Replicas = &desiredReplicas

	if singleReplica(ingressConfig, infraConfig) {
		// non-HA ingress controllers should have default rolling deployment strategy
		return false
	}

	configureAffinity := false
	switch ci.Status.EndpointPublishingStrategy.Type {
	case operatorv1.HostNetworkStrategyType:
		// Typically, an ingress controller will be scaled with replicas
		// set equal to the node pool size, in which case, using surge
		// for rolling updates would fail to create new replicas (in the
		// absence of node auto-scaling).  Thus, when using HostNetwork,
		// we set max unavailable to 25% and surge to 0.
		pointerTo := func(ios intstr.IntOrString) *intstr.IntOrString { return &ios }
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: pointerTo(intstr.FromString("25%")),
				MaxSurge:       pointerTo(intstr.FromInt(0)),
			},
		}

		// Pod replicas for ingress controllers that use the host
		// network cannot be colocated because replicas on the same node
		// would conflict with each other by trying to bind the same
		// ports.  The scheduler avoids scheduling multiple pods that
		// use host networking and specify the same port to the same
		// node.  Thus no affinity policy is required when using
		// HostNetwork.
	case operatorv1.PrivateStrategyType, operatorv1.LoadBalancerServiceStrategyType, operatorv1.NodePortServiceStrategyType:
		// To avoid downtime during a rolling update, we need two
		// things: a deployment strategy and an affinity policy.  First,
		// the deployment strategy: During a rolling update, we want the
		// deployment controller to scale up the new replica set first
		// and scale down the old replica set once the new replica is
		// ready.  Thus set max unavailable to 50% (if replicas < 4) or
		// 25% (if replicas >= 4) and surge to 25%.  Note that the
		// deployment controller rounds surge up and max unavailable
		// down.

		maxUnavailable := "50%"
		if desiredReplicas >= 4 {
			maxUnavailable = "25%"
		}
		pointerTo := func(ios intstr.IntOrString) *intstr.IntOrString { return &ios }
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: pointerTo(intstr.FromString(maxUnavailable)),
				MaxSurge:       pointerTo(intstr.FromString("25%")),
			},
		}

		// Next, the affinity policy: We want the deployment controller
		// to scale the new replica set up in such a way that each new
		// pod is colocated with a pod from the old replica set.  To
		// this end, we add a label with a hash of the deployment, using
		// which we can select replicas of the same generation (or
		// select replicas that are *not* of the same generation).
		// Then, we can configure affinity to colocate replicas of
		// different generations of the same ingress controller, and configure
		// anti-affinity to prevent colocation of replicas of the same
		// generation of the same ingress controller.
		//
		// Together, the deployment strategy and affinity policy ensure
		// that a node that had local endpoints at the start of a
		// rolling update continues to have local endpoints for the
		// duration of and at the completion of the update.
		configureAffinity = true
		deployment.Spec.Template.Spec.Affinity = &corev1.Affinity{
			PodAffinity: &corev1.PodAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
					{
						Weight: int32(100),
						PodAffinityTerm: corev1.PodAffinityTerm{
							TopologyKey: "kubernetes.io/hostname",
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      controller.ControllerDeploymentLabel,
										Operator: metav1.LabelSelectorOpIn,
										Values:   []string{controller.IngressControllerDeploymentLabel(ci)},
									},
									{
										Key:      controller.ControllerDeploymentHashLabel,
										Operator: metav1.LabelSelectorOpNotIn,
										// Values is set at the end of the calling function.
									},
								},
							},
						},
					},
				},
			},
			// TODO: Once https://issues.redhat.com/browse/RFE-1759
			// is implemented, replace
			// "RequiredDuringSchedulingIgnoredDuringExecution" with
			// "PreferredDuringSchedulingIgnoredDuringExecution".
			PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
					{
						TopologyKey: "kubernetes.io/hostname",
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      controller.ControllerDeploymentLabel,
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{controller.IngressControllerDeploymentLabel(ci)},
								},
								{
									Key:      controller.ControllerDeploymentHashLabel,
									Operator: metav1.LabelSelectorOpIn,
									// Values is set at the end of this function.
								},
							},
						},
					},
				},
			},
		}
	}

	return configureAffinity
}

func singleReplica(ingressConfig *configv1.Ingress, infraConfig *configv1.Infrastructure) bool {
	// DefaultPlacement affects which topology field we're interested in
	topology := infraConfig.Status.InfrastructureTopology
	if ingressConfig.Status.DefaultPlacement == configv1.DefaultPlacementControlPlane {
		topology = infraConfig.Status.ControlPlaneTopology
	}

	return topology == configv1.SingleReplicaTopologyMode
}

// determineDeploymentReplicas determines the number of replicas that should be
// set in the Deployment for an IngressController. If the user explicitly set a
// replica count in the IngressController resource, that value will be used.
// Otherwise, if unset, we follow the choice algorithm as described in the
// documentation for the IngressController replicas parameter.
func determineDeploymentReplicas(ic *operatorv1.IngressController, ingressConfig *configv1.Ingress, infraConfig *configv1.Infrastructure) int32 {
	if ic.Spec.Replicas != nil {
		return *ic.Spec.Replicas
	}

	return DetermineReplicas(ingressConfig, infraConfig)
}
