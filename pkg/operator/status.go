package operator

import (
	"fmt"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

func setDegradedTrue(operatorConfig *operatorv1.ServiceCA, reason, message string) {
	v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions,
		operatorv1.OperatorCondition{
			Type:    operatorv1.OperatorStatusTypeDegraded,
			Status:  operatorv1.ConditionTrue,
			Reason:  reason,
			Message: message,
		})
}

func setDegradedFalse(operatorConfig *operatorv1.ServiceCA, reason string) {
	v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions,
		operatorv1.OperatorCondition{
			Type:   operatorv1.OperatorStatusTypeDegraded,
			Status: operatorv1.ConditionFalse,
			Reason: reason,
		})
}

func setProgressingTrue(operatorConfig *operatorv1.ServiceCA, reason, message string) {
	v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1.OperatorCondition{
		Type:    operatorv1.OperatorStatusTypeProgressing,
		Status:  operatorv1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
}

func setAvailableTrue(operatorConfig *operatorv1.ServiceCA, reason string) {
	v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1.OperatorCondition{
		Type:   operatorv1.OperatorStatusTypeAvailable,
		Status: operatorv1.ConditionTrue,
		Reason: reason,
	})
}

func setProgressingFalse(operatorConfig *operatorv1.ServiceCA, reason, message string) {
	v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1.OperatorCondition{
		Type:    operatorv1.OperatorStatusTypeProgressing,
		Status:  operatorv1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
}

func setAvailableFalse(operatorConfig *operatorv1.ServiceCA, reason, message string) {
	v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1.OperatorCondition{
		Type:    operatorv1.OperatorStatusTypeAvailable,
		Status:  operatorv1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
}

func setUpgradeableTrue(operatorConfig *operatorv1.ServiceCA, reason string) {
	v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1.OperatorCondition{
		Type:   operatorv1.OperatorStatusTypeUpgradeable,
		Status: operatorv1.ConditionTrue,
		Reason: reason,
	})
}

func isDeploymentStatusAvailable(deploy appsv1.Deployment) bool {
	return deploy.Status.AvailableReplicas > 0
}

// Return true if no replica instances remaining from the previous deployment.
// There may still be additional replica instances being created.
func isDeploymentStatusAvailableAndUpdated(deploy appsv1.Deployment) bool {
	return deploy.Status.AvailableReplicas > 0 &&
		deploy.Status.ObservedGeneration >= deploy.Generation &&
		deploy.Status.UpdatedReplicas == deploy.Status.Replicas
}

func isDeploymentStatusComplete(deploy appsv1.Deployment) bool {
	desiredReplicas := ptr.Deref(deploy.Spec.Replicas, 1)
	return isDeploymentUpToDate(deploy) && deploy.Status.AvailableReplicas == desiredReplicas
}

// isDeploymentUpToDate returns true when the deployment controller has
// observed the current spec and all replicas use the current pod template.
func isDeploymentUpToDate(deploy appsv1.Deployment) bool {
	desiredReplicas := ptr.Deref(deploy.Spec.Replicas, 1)
	return deploy.Status.ObservedGeneration >= deploy.Generation &&
		deploy.Status.UpdatedReplicas == desiredReplicas &&
		deploy.Status.Replicas == desiredReplicas
}

func (c *serviceCAOperator) syncStatus(operatorConfigCopy *operatorv1.ServiceCA, existingDeployments *appsv1.DeploymentList, targetDeploymentNames sets.String) {
	versionUpdatable := true
	versionUpdatableAndDeploymentsComplete := true
	deploymentUnavailableButUpToDate := false
	deploymentUnavailableNotUpToDate := false
	statusMsg := ""
	existingDeploymentNames := sets.String{}
	for _, dep := range existingDeployments.Items {
		existingDeploymentNames.Insert(dep.Name)
		// If there isn't at least one replica from each deployment, Available=False
		reason := "ManagedDeploymentsNotReady"
		if dep.DeletionTimestamp != nil {
			statusMsg += fmt.Sprintf("\n%s deleting", dep.Name)
			setProgressingTrue(operatorConfigCopy, reason, statusMsg)
			setAvailableFalse(operatorConfigCopy, reason, statusMsg)
			versionUpdatable = false
			versionUpdatableAndDeploymentsComplete = false
			continue
		}
		if !isDeploymentStatusAvailable(dep) {
			if isDeploymentUpToDate(dep) {
				// The deployment spec is fully rolled out but the pod is
				// temporarily unavailable (e.g. node reboot or pod eviction).
				// This is not operator-initiated progress toward a new state.
				statusMsg += fmt.Sprintf("\n%s: all replicas are up-to-date but not yet available", dep.Name)
				deploymentUnavailableButUpToDate = true
			} else {
				statusMsg += fmt.Sprintf("\n%s does not have available replicas", dep.Name)
				deploymentUnavailableNotUpToDate = true
			}
			versionUpdatable = false
			versionUpdatableAndDeploymentsComplete = false
			continue
		}
		if !isDeploymentStatusAvailableAndUpdated(dep) {
			statusMsg += fmt.Sprintf("\n%s is updating", dep.Name)
			versionUpdatable = false
			versionUpdatableAndDeploymentsComplete = false
			continue
		} else if !isDeploymentStatusComplete(dep) {
			versionUpdatableAndDeploymentsComplete = false
			statusMsg += fmt.Sprintf("\n%s is creating replicas.", dep.Name)
			continue
		}
	}
	missing := targetDeploymentNames.Difference(existingDeploymentNames)
	if len(missing) > 0 {
		reason := "ManagedDeploymentsNotFound"
		statusMsg = fmt.Sprintf("Deployments %v not found", missing)
		setProgressingTrue(operatorConfigCopy, reason, statusMsg)
		setAvailableFalse(operatorConfigCopy, reason, statusMsg)
		return
	}

	// All deployments and their replicas are available and updated, no previous instances exist
	if versionUpdatableAndDeploymentsComplete {
		reason := "ManagedDeploymentsCompleteAndUpdated"
		setAvailableTrue(operatorConfigCopy, reason)
		setProgressingFalse(operatorConfigCopy, reason, "All service-ca-operator deployments updated")
		c.setVersion()
		return
	}
	// Deployment is up-to-date (correct version) but temporarily unavailable.
	// This happens during node reboots or pod evictions, not operator-initiated
	// progress. Report not Progressing since the deployment is at the desired state.
	if deploymentUnavailableButUpToDate {
		reason := "ManagedDeploymentsUpToDateButUnavailable"
		setAvailableFalse(operatorConfigCopy, reason, statusMsg)
		setProgressingFalse(operatorConfigCopy, reason, statusMsg)
		c.setVersion()
		return
	}
	// Deployment is not up-to-date and has no available replicas (e.g. recreate
	// rollout where old pods are terminated before new ones are ready).
	if deploymentUnavailableNotUpToDate {
		reason := "ManagedDeploymentsNotReady"
		setAvailableFalse(operatorConfigCopy, reason, statusMsg)
		setProgressingTrue(operatorConfigCopy, reason, statusMsg)
		return
	}
	// No instances of previous deployments,
	// some replicas are missing, but each has at least 1 available; set Progressing=true
	if versionUpdatable {
		reason := "ManagedDeploymentsAvailableAndUpdated"
		setAvailableTrue(operatorConfigCopy, reason)
		setProgressingTrue(operatorConfigCopy, reason, statusMsg)
		c.setVersion()
		return
	}
	// All deployments have at least 1 replica, but some are of previous versions
	// don't report new version, set Progressing=true
	reason := "ManagedDeploymentsAvailable"
	setAvailableTrue(operatorConfigCopy, reason)
	setProgressingTrue(operatorConfigCopy, reason, statusMsg)
}

func (c *serviceCAOperator) setVersion() {
	version := os.Getenv(operatorVersionEnvName)
	if c.versionGetter.GetVersions()["operator"] != version {
		// Set current version
		c.versionGetter.SetVersion("operator", version)
	}
}
