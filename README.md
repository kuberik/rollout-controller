# Rollout Controller

A Kubernetes controller for managing application rollouts with support for health checks, gates, and bake time.

## Features

- **Health Check Integration**: Monitor application health during rollouts
- **Rollout Gates**: Control deployment progression with custom gates
- **Bake Time**: Wait for a specified duration before considering a deployment successful
- **Multiple Rollout Support**: Kustomizations can be managed by multiple rollouts using rollout-specific annotations
- **Time-Based Scheduling**: Control when deployments can occur using RolloutSchedule resources

## Multiple Rollout Support

### Kustomizations

Kustomizations can be managed by multiple rollouts simultaneously using rollout-specific annotations. Each rollout has its own annotation, preventing conflicts between different rollouts.

#### Example

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: my-app
  annotations:
    rollout.kuberik.com/substitute.frontend_version.from: "frontend-rollout"
    rollout.kuberik.com/substitute.backend_version.from: "backend-rollout"
    rollout.kuberik.com/substitute.api_version.from: "api-rollout"
spec:
  # ... other spec fields
  postBuild:
    substitute:
      frontend_version: "1.0.0"  # Managed by frontend-rollout
      backend_version: "2.0.0"   # Managed by backend-rollout
      api_version: "3.0.0"       # Managed by api-rollout
```

In this example:
- `frontend-rollout` manages the `frontend_version` substitute
- `backend-rollout` manages the `backend_version` substitute
- `api-rollout` manages the `api_version` substitute

### OCIRepositories

OCIRepositories use a simpler approach since they only have one modifiable field (tag). Each OCIRepository can be managed by a single rollout.

#### Example

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: my-image-repo
  annotations:
    rollout.kuberik.com/rollout: "my-app-rollout"
spec:
  # ... other spec fields
  reference:
    tag: "latest"  # This will be updated by the referenced rollout
```

### Annotation Format

- **Kustomizations**: `rollout.kuberik.com/substitute.<variable>.from: <rollout-name>`
- **OCIRepositories**: `rollout.kuberik.com/rollout`

Each rollout can manage its own substitute in Kustomizations, while OCIRepositories are managed by a single rollout.

## Time-Based Scheduling

RolloutSchedule and ClusterRolloutSchedule resources enable time-based control over when rollouts can be deployed. Schedules automatically manage RolloutGate resources based on time windows, days of the week, and date ranges.

### Key Concepts

**Schedule Types:**
- **RolloutSchedule**: Namespaced resource that applies to rollouts in the same namespace
- **ClusterRolloutSchedule**: Cluster-scoped resource that can apply across multiple namespaces

**Actions:**
- **Allow**: Deployments are permitted during the schedule window, blocked outside it
- **Deny**: Deployments are blocked during the schedule window, permitted outside it

**Rules:**
Multiple rules can be defined in a schedule. The schedule is active if **ANY rule matches** (OR logic).

Each rule can specify:
- **timeRange**: Specific hours in HH:MM format (24-hour)
- **daysOfWeek**: List of days (Monday, Tuesday, Wednesday, Thursday, Friday, Saturday, Sunday)
- **dateRange**: Specific date range in YYYY-MM-DD format
- **timezone**: IANA timezone format (e.g., "America/New_York", "Europe/London")

### How It Works

1. The schedule controller evaluates time-based rules using the specified timezone
2. For each rollout matching the `rolloutSelector`, a RolloutGate is created/updated
3. The gate's `passing` status is set based on schedule evaluation:
   - **Action: "Allow"** → `passing = active` (allows rollouts when active)
   - **Action: "Deny"** → `passing = !active` (blocks rollouts when active)
4. The rollout controller respects gates when deciding whether to deploy new versions

### RolloutSchedule Example: Business Hours Only

Allow deployments only during weekday business hours:

```yaml
apiVersion: kuberik.com/v1alpha1
kind: RolloutSchedule
metadata:
  name: business-hours-allow
  namespace: default
spec:
  rolloutSelector:
    matchLabels:
      schedule: business-hours
  rules:
    - name: "weekday-business-hours"
      timeRange:
        start: "09:00"
        end: "17:00"
      daysOfWeek:
        - Monday
        - Tuesday
        - Wednesday
        - Thursday
        - Friday
  timezone: "America/New_York"
  action: Allow  # Only allow rollouts during business hours
```

Label your rollout to apply this schedule:

```yaml
apiVersion: kuberik.com/v1alpha1
kind: Rollout
metadata:
  name: my-app-rollout
  labels:
    schedule: business-hours
spec:
  # ... rollout spec
```

### ClusterRolloutSchedule Example: Block Peak Hours

Prevent deployments during peak traffic hours across production namespaces:

```yaml
apiVersion: kuberik.com/v1alpha1
kind: ClusterRolloutSchedule
metadata:
  name: production-peak-hours-deny
spec:
  rolloutSelector:
    matchLabels:
      tier: frontend
  namespaceSelector:
    matchLabels:
      environment: production
  rules:
    - name: "weekday-peak-hours"
      timeRange:
        start: "09:00"
        end: "17:00"
      daysOfWeek:
        - Monday
        - Tuesday
        - Wednesday
        - Thursday
        - Friday
    - name: "saturday-morning"
      timeRange:
        start: "09:00"
        end: "12:00"
      daysOfWeek:
        - Saturday
  timezone: "America/New_York"
  action: Deny  # Block rollouts during peak hours
```

### Common Use Cases

#### Maintenance Windows

Allow deployments only during scheduled maintenance windows:

```yaml
apiVersion: kuberik.com/v1alpha1
kind: RolloutSchedule
metadata:
  name: maintenance-window
  namespace: production
spec:
  rolloutSelector:
    matchLabels:
      maintenance: "true"
  rules:
    - name: "sunday-early-morning"
      timeRange:
        start: "02:00"
        end: "06:00"
      daysOfWeek:
        - Sunday
  timezone: "UTC"
  action: Allow
```

#### Holiday Deployment Freeze

Block all deployments during holiday periods:

```yaml
apiVersion: kuberik.com/v1alpha1
kind: ClusterRolloutSchedule
metadata:
  name: holiday-freeze
spec:
  rolloutSelector:
    matchLabels:
      freeze: "holiday"
  rules:
    - name: "christmas-freeze"
      dateRange:
        start: "2026-12-23"
        end: "2026-12-26"
    - name: "new-year-freeze"
      dateRange:
        start: "2026-12-31"
        end: "2027-01-02"
  timezone: "America/New_York"
  action: Deny
```

#### Weekend-Only Deployments

Restrict certain rollouts to weekends only:

```yaml
apiVersion: kuberik.com/v1alpha1
kind: RolloutSchedule
metadata:
  name: weekend-only
  namespace: default
spec:
  rolloutSelector:
    matchLabels:
      schedule: weekend
  rules:
    - name: "weekend-deployment"
      daysOfWeek:
        - Saturday
        - Sunday
  timezone: "America/Los_Angeles"
  action: Allow
```

#### Cross-Midnight Time Windows

Time ranges automatically handle cross-midnight windows:

```yaml
apiVersion: kuberik.com/v1alpha1
kind: RolloutSchedule
metadata:
  name: night-deployment
  namespace: default
spec:
  rolloutSelector:
    matchLabels:
      schedule: night
  rules:
    - name: "overnight-window"
      timeRange:
        start: "22:00"  # 10 PM
        end: "06:00"    # 6 AM (next day)
      daysOfWeek:
        - Monday
        - Tuesday
        - Wednesday
        - Thursday
        - Friday
  timezone: "UTC"
  action: Allow
```

### Schedule Status

View the current status of schedules:

```bash
kubectl get rolloutschedules
kubectl get clusterrolloutschedules
```

Output shows:
- **ACTION**: Allow or Deny
- **ACTIVE**: Current state (true/false)
- **MATCHING**: Number of rollouts matched by selectors

Describe a schedule for detailed information:

```bash
kubectl describe rolloutschedule business-hours-allow
```

Status includes:
- `active`: Whether the schedule is currently active
- `activeRules`: Names of rules currently matching
- `nextTransition`: When the active state will next change
- `managedGates`: List of RolloutGate names managed by this schedule
- `matchingRollouts`: Count of rollouts matched by selectors

### Multiple Rules (OR Logic)

A schedule is active if **ANY** rule matches. This enables flexible scheduling:

```yaml
apiVersion: kuberik.com/v1alpha1
kind: RolloutSchedule
metadata:
  name: flexible-window
  namespace: default
spec:
  rolloutSelector:
    matchLabels:
      schedule: flexible
  rules:
    - name: "morning-window"
      timeRange:
        start: "09:00"
        end: "11:00"
    - name: "afternoon-window"
      timeRange:
        start: "14:00"
        end: "16:00"
    - name: "weekend-anytime"
      daysOfWeek:
        - Saturday
        - Sunday
  timezone: "America/New_York"
  action: Allow
```

This schedule allows deployments:
- 9-11 AM on any day
- 2-4 PM on any day
- Anytime on Saturday or Sunday

### Integration with RolloutGate

Schedules work by automatically creating and managing RolloutGate resources. Each managed gate is named: `{schedule-name}-{rollout-name}`.

You can view the gates created by schedules:

```bash
kubectl get rolloutgates -l "app.kubernetes.io/managed-by=rollout-schedule"
```

The gates are automatically cleaned up when:
- The schedule is deleted
- A rollout no longer matches the selector
- The rollout is deleted

### Timezone Support

All schedules use IANA timezone database names. Common examples:

- **UTC**: "UTC"
- **US Eastern**: "America/New_York"
- **US Pacific**: "America/Los_Angeles"
- **UK**: "Europe/London"
- **Central European**: "Europe/Paris"
- **Japan**: "Asia/Tokyo"

If no timezone is specified, UTC is used by default.

### Troubleshooting

**Schedule not activating:**
1. Check the schedule status: `kubectl describe rolloutschedule <name>`
2. Verify the timezone is correct
3. Check that rollouts match the `rolloutSelector` labels
4. Review the time range and day of week settings
5. Check the `activeRules` in status to see which rules are matching

**Deployments still blocked:**
1. Verify the action is correct (Allow vs Deny)
2. Check if other RolloutGates are blocking the rollout
3. Review rollout events: `kubectl describe rollout <name>`

**Cross-namespace schedules not working:**
1. Ensure you're using ClusterRolloutSchedule (not RolloutSchedule)
2. Check the `namespaceSelector` matches target namespaces
3. Verify RBAC permissions for the controller

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
   - **Kustomizations**: Updates `spec.postBuild.substitute.<NAME>` for resources with `rollout.kuberik.com/substitute.<NAME>.from: <rollout-name>`

4. **Health Monitoring**: During bake time, the controller monitors HealthChecks to ensure deployment stability.

### Annotations

The rollout controller uses annotations to identify which Flux resources should be managed:

#### OCIRepository
```yaml
apiVersion: source.toolkit.fluxcd.io/v1
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
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: my-app-kustomization
  annotations:
    rollout.kuberik.com/substitute.IMAGE_TAG.from: "my-app-rollout"  # References the rollout name
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
- `v1alpha1_rolloutschedule.yaml` - RolloutSchedule example
- `v1alpha1_clusterrolloutschedule.yaml` - ClusterRolloutSchedule example
- `v1alpha1_rolloutschedule_*.yaml` - Additional scheduling scenarios

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
