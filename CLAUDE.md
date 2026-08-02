# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Rules

- **Always use kubebuilder CLI to scaffold controllers.** Never manually create controller files. Use:
  ```bash
  # For controllers with new CRDs:
  kubebuilder create api --group <group> --version <version> --kind <Kind>

  # For controllers watching external CRDs:
  kubebuilder create api \
    --group <group> \
    --version <version> \
    --kind <Kind> \
    --controller=true \
    --resource=false \
    --external-api-domain <domain> \
    --external-api-path <import-path>
  ```
  Then implement your logic in the scaffolded `*_controller.go` file.

## What is Rollout Controller?

A Kubernetes controller for managing application rollouts with support for health checks, gates, and bake time. It integrates tightly with Flux CD for GitOps workflows, providing progressive delivery capabilities.

## Key Features

- **Flux ImagePolicy Integration**: Reads available releases from Flux ImagePolicy
- **Multiple Rollout Support**: Kustomizations can be managed by multiple rollouts using rollout-specific annotations
- **Health Check Integration**: Monitor application health during rollouts
- **Rollout Gates**: Control deployment progression with custom gates
- **Bake Time**: Wait for a specified duration before considering a deployment successful
- **Version History**: Maintains version history with configurable retention policies
- **Gate Bypass**: Emergency deployment support via annotations

## Common Development Commands

```bash
# Generate code after modifying CRD types
make manifests              # Generate CRD YAML from Go types
make generate               # Generate DeepCopy methods

# Code quality
make fmt                    # go fmt
make vet                    # go vet
make lint                   # gofmt + govet

# Testing
make test                   # Run unit tests (Ginkgo/Gomega)
make test-e2e              # Run e2e tests on Kind cluster

# Building
make build                  # Build binary
make docker-build           # Build container image
make docker-push            # Push to registry

# Deployment
make install                # Install CRDs to cluster
make deploy                 # Deploy controller to cluster
make uninstall              # Remove CRDs and controller
make run                    # Run locally (without cluster deployment)
```

## Development Workflow

1. Modify CRD type definitions in `api/v1alpha1/*_types.go`
2. Run `make manifests generate` to update CRDs and DeepCopy methods
3. Implement reconciliation logic in `internal/controller/*_controller.go`
4. Write tests in `internal/controller/*_controller_test.go`
5. Run `make test` to verify
6. Deploy with `make docker-build docker-push deploy IMG=registry/image:tag`

## Flux CD Integration

### Annotation-Based Resource Patching

**OCIRepository** - managed by single rollout:
```yaml
metadata:
  annotations:
    rollout.kuberik.com/rollout: "my-app-rollout"
spec:
  ref:
    tag: "1.0.0"  # Updated by rollout controller
```

**Kustomization** - multiple rollouts can manage different variables:
```yaml
metadata:
  annotations:
    rollout.kuberik.com/substitute.IMAGE_TAG.from: "frontend-rollout"
    rollout.kuberik.com/substitute.VERSION.from: "backend-rollout"
spec:
  postBuild:
    substitute:
      IMAGE_TAG: "1.0.0"  # Managed by frontend-rollout
      VERSION: "2.0.0"    # Managed by backend-rollout
```

### ImagePolicy Workflow

1. Flux ImageRepository scans OCI registry
2. Flux ImagePolicy filters releases with semver/regex
3. Rollout Controller reads `latestRef` from ImagePolicy status
4. Controller patches OCIRepository/Kustomization resources
5. Flux reconciles changes and deploys

## CRDs Managed

- **Rollout**: Main resource defining deployment strategy
- **RolloutGate**: Conditions that must pass before deployment
- **RolloutDependency**: Gates a rollout on the deployed contract version of another rollout
- **RolloutSchedule**: Scheduled deployment configurations
- **HealthCheck**: Health status from various sources
- **ClusterRolloutSchedule**: Cross-cluster schedules

## Rollout Dependencies

`RolloutDependency` orders rollouts by inter-service contract version: a
consumer waits until the provider it was built against has actually deployed.

Releases declare what they were built against using OCI annotations, so the
dependency travels with the image rather than the manifests:

```
org.opencontainers.image.version    = <MAJOR.MINOR.PATCH>-<seq>   # own contract version
com.kuberik.rollout.requires.<name> = <constraint>                # per consumed contract
```

The requirement is a version constraint parsed by
[Masterminds/semver](https://github.com/Masterminds/semver#checking-version-constraints),
applied verbatim — this controller adds no rules of its own, so that page is the
reference. `^1.2.0`, `~1.2.0`, `>=1.2.0 <2.0.0`, `1.2.x` and combinations all
work.

Note that a bare version (`1.2.0`) is an **exact match** there, not a floor. A
release that tolerates later providers has to say so: `^1.2.0` for
compatible-within-major, `~1.2.0` for within-minor, `>=1.2.0` for anything later.

The `-<seq>` suffix on the provider's own version is a monotonic per-release
ordinal, attached as a SemVer **pre-release** identifier (not build metadata,
which SemVer ignores for precedence). That ordinal, and only that ordinal, is
stripped before the constraint is evaluated: by SemVer §11 a pre-release sorts
below its own triple, so a suffixed provider version would never satisfy a
constraint on that triple. A real pre-release (`2.0.0-alpha.1`) is kept, and
correctly fails a constraint on `2.0.0`.

Parsing is strict. Lenient SemVer coerces `1.2` into `1.2.0` and a CalVer stamp
like `2024-01-15` into `2024.0.0`, which would satisfy every constraint ever
written; an unparseable version blocks instead.

The controller reads each consumer release's `requires` values from
`Rollout.status.availableReleases` (populated by `parseOCIManifest`), compares
them against the contract version of the provider's successfully baked release,
and publishes the verdict as a managed `RolloutGate` named
`dependency-<name>` whose `allowedVersions` holds exactly the admitted
releases. Rollout admission logic is untouched.

```yaml
apiVersion: kuberik.com/v1alpha1
kind: RolloutDependency
metadata:
  name: frontend-needs-backend
spec:
  rolloutRef:                 # consumer being gated (same namespace)
    name: frontend-app
  providerRef:                # provider, optionally in another namespace
    name: backend-app
  contract: backend           # defaults to providerRef.name
```

### Trust model and limits

- **The requires annotation is a self-declaration.** It gates the release that
  carries it, so anyone who can push to the consumer's image repo can also write
  `>=0.0.0` and self-admit. `RolloutDependency` is a coordination and ordering
  control, not an authorization boundary — it does not contain a hostile image
  publisher.
- **A first deploy is not gated.** A Rollout with no history falls back to the
  raw release candidates when gates filter them all out, so a brand-new consumer
  reaches an initial version regardless of an unmet dependency. It is gated from
  its second release on.
- **A provider with no `bakeTime` records `Succeeded` immediately**, before the
  workload has rolled. Configure `bakeTime` on providers whose consumers must
  not start until the contract is actually live.
- **Apply the CRDs before rolling the controller.** `requires` is a new property
  on the existing Rollout CRD; if the schema is not updated first the API server
  prunes it on every status write and releases read back as declaring nothing.
- Releases already in `status.availableReleases` from before this change carry no
  `requires`, and are only re-resolved when the image policy points at them
  again. A dependency added to an existing Rollout does not retroactively gate
  that backlog.

Worked example: `../rollout-dashboard/example/hello-dep`.

## Rollout Spec Example

```yaml
apiVersion: kuberik.com/v1alpha1
kind: Rollout
metadata:
  name: my-app-rollout
spec:
  releasesImagePolicy:
    name: my-app-policy

  versionHistoryLimit: 10
  releaseUpdateInterval: "5m"

  # Bake time for health checks
  bakeTime: "10m"
  healthCheckSelector:
    matchLabels:
      app: myapp
```

## Important Annotations

```yaml
# OCIRepository
rollout.kuberik.com/rollout: "rollout-name"

# Kustomization
rollout.kuberik.com/substitute.<VAR>.from: "rollout-name"

# Gate bypass for emergency deployments
rollout.kuberik.com/bypass-gates: "v1.2.3"
```

## Common Patterns

### Controller Reconciliation

```go
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // 1. Get resource (with IgnoreNotFound for deleted resources)
    rollout := &kuberikv1alpha1.Rollout{}
    if err := r.Get(ctx, req.NamespacedName, rollout); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // 2. Add finalizer if needed
    // 3. Perform business logic
    // 4. Update resource status with conditions
    // 5. Return Result with optional RequeueAfter

    return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}
```

**Result Options:**
- `ctrl.Result{}` - Stop, don't requeue
- `ctrl.Result{Requeue: true}` - Retry immediately
- `ctrl.Result{RequeueAfter: 5*time.Second}` - Retry after delay

### Testing with Ginkgo/Gomega

```go
var _ = Describe("RolloutController", func() {
    Context("when rollout is created", func() {
        It("should update status", func() {
            rollout := &kuberikv1alpha1.Rollout{...}
            Expect(k8sClient.Create(ctx, rollout)).To(Succeed())

            Eventually(func() string {
                err := k8sClient.Get(ctx, key, rollout)
                if err != nil {
                    return ""
                }
                return rollout.Status.Phase
            }).Should(Equal("Ready"))
        })
    })
})
```

### Event Recording

```go
r.Recorder.Event(rollout, corev1.EventTypeNormal, "RolloutSucceeded", "Deployment completed")
r.Recorder.Event(rollout, corev1.EventTypeWarning, "GatesFailing", "Required gates not passing")
```

### Status Subresource Pattern

```go
r.Client.Status().Update(ctx, resource)  // Update status
r.Client.Update(ctx, resource)           // Update spec/metadata
```

## Dependencies

Key dependencies:
- `github.com/fluxcd/image-reflector-controller/api` - ImagePolicy
- `github.com/fluxcd/kustomize-controller/api` - Kustomization
- `github.com/fluxcd/source-controller/api` - OCIRepository
- `github.com/google/go-containerregistry` - OCI image operations
- `sigs.k8s.io/controller-runtime` - Kubebuilder framework

## Debugging

```bash
# Controller logs
kubectl logs -n kuberik-system deployment/rollout-controller -f

# Events
kubectl get events --sort-by='.lastTimestamp'

# Describe resources
kubectl describe rollout <name>
kubectl describe rolloutgate <name>
kubectl describe healthcheck <name>
```
