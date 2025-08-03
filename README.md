# Rollout Controller

A Kubernetes controller for managing application rollouts with support for Flux ImagePolicy integration.

## Overview

The Rollout Controller manages application deployments by:
- Monitoring available releases from Flux ImagePolicy
- Evaluating deployment gates
- Managing bake time for health checks
- Copying releases from source to target repositories

## Flux ImagePolicy Integration

The rollout controller integrates with Flux's image automation components to provide a GitOps-friendly approach to release management. Instead of directly accessing OCI repositories, the controller uses Flux's ImagePolicy to determine available releases.

### Architecture

```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│   ImageRepo     │    │  ImagePolicy    │    │    Rollout      │
│   (Flux)        │───▶│   (Flux)        │───▶│  Controller     │
└─────────────────┘    └─────────────────┘    └─────────────────┘
                                                         │
                                                         ▼
                                               ┌─────────────────┐
                                               │ Target Repo     │
                                               │ (Deployment)    │
                                               └─────────────────┘
```

### Setup

1. **Create an ImageRepository** (Flux component):
```yaml
apiVersion: image.toolkit.fluxcd.io/v1beta2
kind: ImageRepository
metadata:
  name: my-app-repo
  namespace: flux-system
spec:
  image: ghcr.io/myorg/myapp
  interval: 1m0s
  secretRef:
    name: ghcr-secret
```

2. **Create an ImagePolicy** (Flux component):
```yaml
apiVersion: image.toolkit.fluxcd.io/v1beta2
kind: ImagePolicy
metadata:
  name: my-app-policy
  namespace: flux-system
spec:
  imageRepositoryRef:
    name: my-app-repo
  policy:
    semver:
      range: '>=1.0.0'
      prereleases:
        enabled: true
        allow:
          - rc
          - beta
          - alpha
  digestReflectionPolicy: IfNotPresent
```

3. **Create a Rollout** (this controller):
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

  # Optional: bake time for health checks
  bakeTime: "10m"
  healthCheckSelector:
    matchLabels:
      app: myapp
```

### How it Works

1. **Release Discovery**: The controller reads the `latestRef` from the ImagePolicy status to determine available releases.

2. **Version Selection**: The controller applies semver logic to select the next release to deploy, considering:
   - Current deployed version
   - Available releases from ImagePolicy
   - Gates and constraints

3. **Resource Patching**: The controller finds and patches Flux resources with specific annotations:
   - **OCIRepositories**: Updates `spec.ref.tag` for resources with `rollout.kuberik.com/rollout: <rollout-name>`
   - **Kustomizations**: Updates `spec.postBuild.substitute.<NAME>` for resources with `rollout.kuberik.com/rollout: <rollout-name>` and `rollout.kuberik.com/substitute: <NAME>`

4. **Health Monitoring**: During bake time, the controller monitors HealthChecks to ensure deployment stability.

### Annotations

The rollout controller uses annotations to identify which Flux resources should be managed:

#### OCIRepository
```yaml
apiVersion: source.toolkit.fluxcd.io/v1beta2
kind: OCIRepository
metadata:
  name: my-app-repo
  annotations:
    rollout.kuberik.com/rollout: "my-app-rollout"  # References the rollout name
spec:
  url: oci://ghcr.io/myorg/myapp
  ref:
    tag: "1.0.0"  # This will be updated by the rollout controller
```

#### Kustomization
```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1beta2
kind: Kustomization
metadata:
  name: my-app-kustomization
  annotations:
    rollout.kuberik.com/rollout: "my-app-rollout"  # References the rollout name
    rollout.kuberik.com/substitute: "IMAGE_TAG"     # Specifies which substitute to update
spec:
  postBuild:
    substitute:
      IMAGE_TAG: "1.0.0"  # This will be updated by the rollout controller
```

### Benefits

- **GitOps Integration**: Leverages Flux's image automation for consistent, auditable release management
- **Separation of Concerns**: Image discovery is handled by Flux, deployment by the rollout controller
- **Policy-Driven**: Uses Flux's powerful image policy engine for release selection
- **Observability**: Integrates with Flux's monitoring and alerting capabilities

## Installation

### Prerequisites

- Kubernetes cluster
- Flux v2 installed with image-reflector-controller
- kubectl configured

### Install the Controller

```bash
# Install CRDs
kubectl apply -f config/crd/bases/

# Install the controller
kubectl apply -k config/default/
```

## Usage Examples

See the `config/samples/` directory for complete examples including:
- `v1alpha1_imagerepository.yaml` - Flux ImageRepository example
- `v1alpha1_imagepolicy.yaml` - Flux ImagePolicy example
- `v1alpha1_rollout.yaml` - Rollout controller example

## Development

### Building

```bash
make build
```

### Testing

```bash
make test
```

### Running Locally

```bash
make run
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests
5. Submit a pull request

## License

Apache 2.0
