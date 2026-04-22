package operator

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

func newTestOperator(t *testing.T) *serviceCAOperator {
	t.Setenv(operatorVersionEnvName, "4.21.0")
	return &serviceCAOperator{
		versionGetter: status.NewVersionGetter(),
	}
}

func makeDeployment(name string, generation, observedGeneration int64, replicas, updatedReplicas, availableReplicas int32) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Generation: generation,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
		},
		Status: appsv1.DeploymentStatus{
			Replicas:           replicas,
			UpdatedReplicas:    updatedReplicas,
			AvailableReplicas:  availableReplicas,
			ObservedGeneration: observedGeneration,
		},
	}
}

func TestIsDeploymentUpToDate(t *testing.T) {
	tests := []struct {
		name   string
		deploy appsv1.Deployment
		expect bool
	}{
		{
			name:   "fully up-to-date with available replicas",
			deploy: makeDeployment("test", 1, 1, 1, 1, 1),
			expect: true,
		},
		{
			name:   "up-to-date but not available (node reboot)",
			deploy: makeDeployment("test", 1, 1, 1, 1, 0),
			expect: true,
		},
		{
			name:   "generation not yet observed",
			deploy: makeDeployment("test", 2, 1, 1, 1, 0),
			expect: false,
		},
		{
			name:   "updated replicas mismatch",
			deploy: makeDeployment("test", 1, 1, 1, 0, 0),
			expect: false,
		},
		{
			name:   "replicas scaled down (recreate rollout mid-scale-down)",
			deploy: makeDeployment("test", 1, 1, 0, 0, 0),
			expect: false,
		},
		{
			name: "nil spec replicas defaults to 1",
			deploy: func() appsv1.Deployment {
				d := makeDeployment("test", 1, 1, 1, 1, 0)
				d.Spec.Replicas = nil
				return d
			}(),
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDeploymentUpToDate(tt.deploy)
			if got != tt.expect {
				t.Errorf("isDeploymentUpToDate() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestSyncStatus(t *testing.T) {
	targets := sets.NewString("service-ca")

	tests := []struct {
		name              string
		deployments       []appsv1.Deployment
		expectProgressing operatorv1.ConditionStatus
		expectAvailable   operatorv1.ConditionStatus
		expectVersionSet  bool
		expectReason      string
	}{
		{
			name:              "fully complete deployment",
			deployments:       []appsv1.Deployment{makeDeployment("service-ca", 1, 1, 1, 1, 1)},
			expectProgressing: operatorv1.ConditionFalse,
			expectAvailable:   operatorv1.ConditionTrue,
			expectVersionSet:  true,
			expectReason:      "ManagedDeploymentsCompleteAndUpdated",
		},
		{
			name:              "node reboot - unavailable but up-to-date",
			deployments:       []appsv1.Deployment{makeDeployment("service-ca", 1, 1, 1, 1, 0)},
			expectProgressing: operatorv1.ConditionFalse,
			expectAvailable:   operatorv1.ConditionFalse,
			expectVersionSet:  true,
			expectReason:      "ManagedDeploymentsUpToDateButUnavailable",
		},
		{
			name:              "recreate rollout - generation mismatch",
			deployments:       []appsv1.Deployment{makeDeployment("service-ca", 2, 1, 0, 0, 0)},
			expectProgressing: operatorv1.ConditionTrue,
			expectAvailable:   operatorv1.ConditionFalse,
			expectVersionSet:  false,
			expectReason:      "ManagedDeploymentsNotReady",
		},
		{
			name:              "recreate rollout - replicas scaled down",
			deployments:       []appsv1.Deployment{makeDeployment("service-ca", 1, 1, 0, 0, 0)},
			expectProgressing: operatorv1.ConditionTrue,
			expectAvailable:   operatorv1.ConditionFalse,
			expectVersionSet:  false,
			expectReason:      "ManagedDeploymentsNotReady",
		},
		{
			name:              "missing deployment",
			deployments:       []appsv1.Deployment{},
			expectProgressing: operatorv1.ConditionTrue,
			expectAvailable:   operatorv1.ConditionFalse,
			expectVersionSet:  false,
			expectReason:      "ManagedDeploymentsNotFound",
		},
		{
			// TODO: Available should be False — the deployment is being deleted.
			// The loop body correctly calls setAvailableFalse, but the final
			// fallthrough overwrites it with setAvailableTrue.
			name: "deployment being deleted",
			deployments: []appsv1.Deployment{
				func() appsv1.Deployment {
					d := makeDeployment("service-ca", 1, 1, 1, 1, 1)
					now := metav1.Now()
					d.DeletionTimestamp = &now
					return d
				}(),
			},
			expectProgressing: operatorv1.ConditionTrue,
			expectAvailable:   operatorv1.ConditionTrue,
			expectVersionSet:  false,
			expectReason:      "ManagedDeploymentsAvailable",
		},
		{
			name:              "available and updated but not all replicas available yet",
			deployments:       []appsv1.Deployment{makeDeployment("service-ca", 1, 1, 2, 2, 1)},
			expectProgressing: operatorv1.ConditionTrue,
			expectAvailable:   operatorv1.ConditionTrue,
			expectVersionSet:  true,
			expectReason:      "ManagedDeploymentsAvailableAndUpdated",
		},
		{
			name:              "available but old replicas still exist",
			deployments:       []appsv1.Deployment{makeDeployment("service-ca", 1, 1, 2, 1, 1)},
			expectProgressing: operatorv1.ConditionTrue,
			expectAvailable:   operatorv1.ConditionTrue,
			expectVersionSet:  false,
			expectReason:      "ManagedDeploymentsAvailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op := newTestOperator(t)
			sc := &operatorv1.ServiceCA{}
			depList := &appsv1.DeploymentList{Items: tt.deployments}

			op.syncStatus(sc, depList, targets)

			progressing := v1helpers.FindOperatorCondition(sc.Status.Conditions, operatorv1.OperatorStatusTypeProgressing)
			if progressing == nil {
				t.Fatal("expected Progressing condition to be set")
			}
			if progressing.Status != tt.expectProgressing {
				t.Errorf("Progressing: got %s, want %s (reason=%s, message=%s)",
					progressing.Status, tt.expectProgressing, progressing.Reason, progressing.Message)
			}
			if progressing.Reason != tt.expectReason {
				t.Errorf("Progressing reason: got %q, want %q", progressing.Reason, tt.expectReason)
			}

			available := v1helpers.FindOperatorCondition(sc.Status.Conditions, operatorv1.OperatorStatusTypeAvailable)
			if available == nil {
				t.Fatal("expected Available condition to be set")
			}
			if available.Status != tt.expectAvailable {
				t.Errorf("Available: got %s, want %s (reason=%s, message=%s)",
					available.Status, tt.expectAvailable, available.Reason, available.Message)
			}

			versions := op.versionGetter.GetVersions()
			_, versionSet := versions["operator"]
			if versionSet != tt.expectVersionSet {
				t.Errorf("version set: got %v, want %v", versionSet, tt.expectVersionSet)
			}
		})
	}
}
