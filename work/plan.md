# Plan: Refactor service-ca-operator to use library-go workload controller

## Context

The service-ca-operator currently manages its controller Deployment and computes ClusterOperator status conditions (Available, Progressing, Degraded) with ~170 lines of hand-rolled logic in `pkg/operator/status.go` and `pkg/operator/sync_common.go:manageDeployment()`. The library-go [workload controller](https://github.com/openshift/library-go/blob/master/pkg/operator/apiserver/controller/workload/workload.go) standardizes this pattern — it manages a Deployment, monitors its rollout, and writes status conditions via server-side apply. Adopting it reduces custom code and aligns with the pattern used by other OpenShift operators (e.g. cluster-openshift-apiserver-operator, cluster-authentication-operator).

Additionally, the 6 static resources (Namespace, ClusterRole, ClusterRoleBinding, Role, RoleBinding, ServiceAccount) are applied manually in `manageControllerNS()` and `manageControllerResources()`. These can be replaced by library-go's [StaticResourceController](https://github.com/openshift/library-go/blob/master/pkg/operator/staticresourcecontroller/static_resource_controller.go).

## Complexity assessment: Moderate

The refactor is **moderately complex** due to several interacting concerns:

1. **Neither the workload controller nor StaticResourceController is currently vendored** — requires a library-go vendor bump and potentially new transitive dependencies
2. **CA management is tightly coupled to the deployment sync** — when the CA rotates, it forces a deployment rollout. This coupling must be preserved inside the `Delegate.Sync()` implementation
3. **Status condition naming changes** — the workload controller writes prefixed conditions (`<prefix>DeploymentAvailable`, `<prefix>WorkloadDegraded`, etc.) instead of the bare `Available`/`Degraded`/`Progressing` the operator currently uses. The `ClusterOperatorStatusController` aggregates by suffix, so this should work, but it changes condition names visible in the ServiceCA CR
4. **Status update pattern changes** — current code uses `UpdateStatus()` (full replace), workload controller uses `ApplyOperatorStatus()` (server-side apply with field managers). These must not conflict — the remaining custom code (Upgradeable condition, CA-specific degraded) must also switch to server-side apply
5. **ManagementState `Removed` behavior changes** — the workload controller deletes the target namespace on `Removed`, which the current operator does not do. Need to decide if this is acceptable or if `SetOperatorNotRemovable()` should be called
6. **Test updates** — unit tests in `pkg/operator/` that test the sync loop and status computation will need significant rework

## Design: Two library-go controllers + a Delegate

Do NOT use `APIServerControllerSet` — it bundles controllers (audit, encryption, revision, prune) irrelevant to this operator. Use `workload.NewController` and `StaticResourceController` directly.

### Architecture after refactor

```
RunOperator() starts:
  1. StaticResourceController  — applies NS, ClusterRole, CRB, Role, RB, SA from bindata
  2. workload.Controller       — calls Delegate.Sync() which handles CA + Deployment
  3. ClusterOperatorStatus     — aggregates conditions from (1) and (2) into ClusterOperator
  4. ResourceSyncController    — syncs CA bundle ConfigMap (unchanged)
  5. LogLevelController        — watches LogLevel (unchanged)
```

### Step 1: Vendor bump

Add the following packages to the vendor tree:
- `github.com/openshift/library-go/pkg/operator/apiserver/controller/workload`
- `github.com/openshift/library-go/pkg/operator/staticresourcecontroller`
- `github.com/openshift/library-go/pkg/apps/deployment` (transitive dep of workload controller)
- Any other transitive dependencies

Run `go mod tidy && go mod vendor` and commit as a separate vendor commit.

### Step 2: Add StaticResourceController for static resources

**New code in `pkg/operator/starter.go`:**
```go
staticResourceController := staticresourcecontroller.NewStaticResourceController(
    "ServiceCAStaticResources",
    bindata.Asset,
    []string{
        "assets/ns.yaml",
        "assets/clusterrole.yaml",
        "assets/clusterrolebinding.yaml",
        "assets/role.yaml",
        "assets/rolebinding.yaml",
        "assets/sa.yaml",
    },
    resourceapply.NewClientHolder().
        WithKubernetes(kubeClient),
    operatorClient,
    controllerContext.EventRecorder,
).AddKubeInformers(kubeInformersForNamespaces)
```

**Note:** This requires adding `bindata.Asset` function (currently only `bindata.MustAsset` exists). The `StaticResourceController` expects an `AssetFunc` of type `func(string) ([]byte, error)` — need to add a wrapper or use `embed.FS.ReadFile` directly.

**Delete:** `manageControllerNS()` and `manageControllerResources()` from `sync_common.go`.

### Step 3: Implement the workload `Delegate`

Create a new file `pkg/operator/workload.go` with:

```go
type serviceCAWorkload struct {
    // Fields needed for CA management + deployment creation
    operatorClient       *operatorclient.OperatorClient
    operatorConfigLister operatorv1listers.ServiceCALister
    infrastructureLister configv1listers.InfrastructureLister
    appsv1Client         appsclientv1.AppsV1Interface
    corev1Client         coreclientv1.CoreV1Interface
    eventRecorder        events.Recorder
    // CA management fields
    minimumTrustDuration       time.Duration
    signingCertificateLifetime time.Duration
    enabledFeatureGates        map[string]bool
    pkiProvider                pki.PKIProfileProvider
}

// Sync implements workload.Delegate
func (w *serviceCAWorkload) Sync(ctx context.Context, syncCtx factory.SyncContext) (*appsv1.Deployment, bool, []error) {
    operatorConfig, err := w.operatorConfigLister.Get("cluster")
    if err != nil {
        return nil, false, []error{err}
    }
    infrastructure, err := w.infrastructureLister.Get("cluster")
    if err != nil {
        return nil, false, []error{err}
    }

    // 1. Manage CA (create/rotate)
    caModified, err := w.manageSignerCA(ctx, operatorConfig.Spec.UnsupportedConfigOverrides.Raw)
    if err != nil {
        return nil, false, []error{err}
    }

    // 2. Manage CA bundle
    _, err = w.manageSignerCABundle(ctx, caModified)
    if err != nil {
        return nil, false, []error{err}
    }

    // 3. Build and apply the Deployment
    deployment, err := w.manageDeployment(ctx, operatorConfig, caModified, shouldScheduleOnWorkers(infrastructure))
    if err != nil {
        return nil, false, []error{err}
    }

    // Return the deployment and whether the operator config is at the highest generation
    return deployment, true, nil
}

// PreconditionFulfilled implements workload.Delegate
func (w *serviceCAWorkload) PreconditionFulfilled(ctx context.Context) (bool, error) {
    // Check that the target namespace exists (created by StaticResourceController)
    _, err := w.corev1Client.Namespaces().Get(ctx, operatorclient.TargetNamespace, metav1.GetOptions{})
    if apierrors.IsNotFound(err) {
        return false, nil
    }
    return err == nil, err
}

// WorkloadDeleted implements workload.Delegate
func (w *serviceCAWorkload) WorkloadDeleted(ctx context.Context) (bool, string, error) {
    return false, "", nil  // This operator never deletes its workload
}
```

The existing `manageSignerCA()`, `manageSignerCABundle()`, and `manageDeployment()` move to this new type (or are called from it). The `manageDeployment()` function changes slightly: instead of returning `(bool, error)`, it returns `(*appsv1.Deployment, error)` so the workload controller can inspect its status.

### Step 4: Wire up workload.NewController in starter.go

```go
serviceCAWorkload := &serviceCAWorkload{...}

workloadController := workload.NewController(
    "ServiceCA",
    operatorclient.OperatorNamespace,
    operatorclient.TargetNamespace,
    os.Getenv(operatorVersionEnvName),
    "",           // operandNamePrefix (empty — version key is just the deployment name)
    "ServiceCA",  // conditionsPrefix
    operatorClient,
    kubeClient,
    kubeInformersForNamespaces.PodLister(),
    []factory.Informer{
        operatorClient.Informers.Operator().V1().ServiceCAs().Informer(),
        configInformers.Config().V1().Infrastructures().Informer(),
        // PKI informer if ConfigurablePKI enabled
    },
    []factory.Informer{
        kubeInformersNamespaced.Core().V1().Namespaces().Informer(),
    },
    serviceCAWorkload,
    controllerContext.EventRecorder,
    versionGetter,
)
```

### Step 5: Delete replaced code

- **Delete `pkg/operator/operator.go`** — the `serviceCAOperator` type, `NewServiceCAOperator()`, `Sync()`, `updateStatus()` are all replaced
- **Delete `pkg/operator/status.go`** — all status helper functions and `syncStatus()` are replaced by the workload controller's `updateOperatorStatus()`
- **Trim `pkg/operator/sync_common.go`** — remove `manageControllerNS()`, `manageControllerResources()`. Keep CA management functions and `manageDeployment()` (moved to `serviceCAWorkload`)
- **Delete `pkg/operator/sync.go`** — `syncControllers()` is replaced by `Delegate.Sync()`

### Step 6: Update starter.go controller list

Replace the old controller list:
```go
// Before: operator.Run, logLevel.Run, clusterOperatorStatus.Run, resourceSync.Run
// After:
for _, controllerRunner := range []func(ctx context.Context, workers int){
    staticResourceController.Run,
    workloadController.Run,
    operatorLogLevelController.Run,
    clusterOperatorStatus.Run,
    resourceSyncController.Run,
} {
    go controllerRunner(ctx, 1)
}
```

### Step 7: Update unit tests

- Tests in `pkg/operator/operator_test.go` and `pkg/operator/status_test.go` that test the old sync loop and status computation need to be rewritten to test the Delegate's `Sync()` method
- Tests for CA management (`sync_common_test.go`, `rotate_test.go`) are largely unaffected — they test functions that remain

## Key decisions requiring input

1. **Condition prefix**: Using `"ServiceCA"` as prefix yields conditions like `ServiceCADeploymentAvailable`, `ServiceCADeploymentProgressing`, `ServiceCAWorkloadDegraded`, `ServiceCADeploymentDegraded`. This changes the condition types visible in `oc get serviceca cluster -o yaml`. Is this acceptable?

2. **`Removed` management state**: The workload controller deletes the target namespace on `Removed`. The current operator no-ops. Should we call `management.SetOperatorNotRemovable()` to preserve current behavior, or is namespace deletion acceptable?

3. **`bindata.Asset` function**: The StaticResourceController needs `func(string) ([]byte, error)`. Currently we only have `bindata.MustAsset` (panics on error). We need to add an `Asset` function or use `bindata.FS.ReadFile` directly.

## Files modified

| File | Action |
|------|--------|
| `go.mod`, `go.sum`, `vendor/` | Vendor bump for new library-go packages |
| `bindata/assets.go` | Add `Asset()` function wrapper |
| `pkg/operator/starter.go` | Major rewrite: add StaticResourceController + workload.NewController, remove old operator |
| `pkg/operator/workload.go` | **New**: Delegate implementation with CA + deployment sync |
| `pkg/operator/operator.go` | **Delete** (replaced by workload controller) |
| `pkg/operator/status.go` | **Delete** (replaced by workload controller) |
| `pkg/operator/sync.go` | **Delete** (replaced by Delegate.Sync) |
| `pkg/operator/sync_common.go` | Trim: remove static resource functions, keep CA + deployment functions |
| `pkg/operator/*_test.go` | Update tests for new structure |

## Verification

1. `make build` — verify compilation
2. `make test-unit` — verify unit tests pass
3. `make verify` — verify generated files
4. E2e: deploy to a cluster, verify:
   - Static resources (NS, RBAC, SA) are created
   - Signing CA secret is created
   - Controller deployment is running
   - `oc get clusteroperator service-ca` shows Available=True, Progressing=False, Degraded=False
   - `oc get serviceca cluster -o yaml` shows the new condition types
   - CA rotation still works via UnsupportedConfigOverrides
   - Feature gates (ShortCertRotation, ConfigurablePKI) still forwarded to controller
