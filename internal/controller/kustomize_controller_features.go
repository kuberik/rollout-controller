/*
Copyright 2025.

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

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// CancelHealthCheckOnNewRevisionFeatureGate is the kustomize-controller feature gate
	// that cancels in-flight health checks when a new revision becomes available
	// (Flux 2.7+, extended in Flux 2.8). Without it, a Kustomization with wait: true
	// stays stuck on a failing revision until its timeout expires.
	CancelHealthCheckOnNewRevisionFeatureGate = "CancelHealthCheckOnNewRevision"

	// KustomizeControllerFeatureModeAuto detects the feature gate from the
	// kustomize-controller Deployment args.
	KustomizeControllerFeatureModeAuto = "auto"
	// KustomizeControllerFeatureModeEnabled assumes the feature gate is enabled.
	KustomizeControllerFeatureModeEnabled = "enabled"
	// KustomizeControllerFeatureModeDisabled assumes the feature gate is disabled.
	KustomizeControllerFeatureModeDisabled = "disabled"

	// kustomizeControllerAppLabel is the label value Flux puts on the kustomize-controller Deployment.
	kustomizeControllerAppLabel = "kustomize-controller"

	defaultFeatureDetectionTTL = time.Minute
)

// KustomizeControllerFeatureDetector reports whether kustomize-controller runs with
// the CancelHealthCheckOnNewRevision feature gate enabled. Feature gates are process
// flags and leave no trace on Kustomization objects, so in auto mode the gate is read
// from the args of every Deployment labelled app=kustomize-controller. The gate counts
// as enabled only when at least one such Deployment is found and all of them enable it.
type KustomizeControllerFeatureDetector struct {
	// Reader is used to list Deployments. Use an uncached reader (mgr.GetAPIReader())
	// to avoid starting a cluster-wide Deployment informer.
	Reader client.Reader
	// Mode is one of auto, enabled or disabled. Empty means auto.
	Mode string
	// TTL controls how long a detection result is cached. Zero means one minute.
	TTL   time.Duration
	Clock Clock

	mu        sync.Mutex
	cached    bool
	checkedAt time.Time
	hasCached bool
}

// ParseKustomizeControllerFeatureMode validates a feature mode flag value.
func ParseKustomizeControllerFeatureMode(mode string) (string, error) {
	switch mode {
	case "", KustomizeControllerFeatureModeAuto:
		return KustomizeControllerFeatureModeAuto, nil
	case KustomizeControllerFeatureModeEnabled, KustomizeControllerFeatureModeDisabled:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid mode %q, must be one of %s, %s, %s", mode,
			KustomizeControllerFeatureModeAuto, KustomizeControllerFeatureModeEnabled, KustomizeControllerFeatureModeDisabled)
	}
}

// CancelHealthCheckOnNewRevision reports whether the feature gate is enabled.
// A nil detector reports false.
func (d *KustomizeControllerFeatureDetector) CancelHealthCheckOnNewRevision(ctx context.Context) bool {
	if d == nil {
		return false
	}
	switch d.Mode {
	case KustomizeControllerFeatureModeEnabled:
		return true
	case KustomizeControllerFeatureModeDisabled:
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	ttl := d.TTL
	if ttl == 0 {
		ttl = defaultFeatureDetectionTTL
	}
	if d.hasCached && now.Sub(d.checkedAt) < ttl {
		return d.cached
	}

	enabled, err := d.detect(ctx)
	if err != nil {
		// Fall back to the conservative per-resource health check and retry next time.
		logf.FromContext(ctx).Error(err, "failed to detect kustomize-controller feature gates, assuming "+
			CancelHealthCheckOnNewRevisionFeatureGate+" is disabled")
		return false
	}
	d.cached = enabled
	d.checkedAt = now
	d.hasCached = true
	return enabled
}

func (d *KustomizeControllerFeatureDetector) now() time.Time {
	if d.Clock == nil {
		return time.Now()
	}
	return d.Clock.Now()
}

func (d *KustomizeControllerFeatureDetector) detect(ctx context.Context) (bool, error) {
	deployments := &appsv1.DeploymentList{}
	if err := d.Reader.List(ctx, deployments, client.MatchingLabels{"app": kustomizeControllerAppLabel}); err != nil {
		return false, fmt.Errorf("failed to list kustomize-controller deployments: %w", err)
	}
	if len(deployments.Items) == 0 {
		return false, nil
	}
	for _, deployment := range deployments.Items {
		if !deploymentEnablesFeatureGate(&deployment, CancelHealthCheckOnNewRevisionFeatureGate) {
			return false, nil
		}
	}
	return true, nil
}

// deploymentEnablesFeatureGate reports whether the kustomize-controller (or the only)
// container of the Deployment enables the given feature gate via --feature-gates.
func deploymentEnablesFeatureGate(deployment *appsv1.Deployment, gate string) bool {
	containers := deployment.Spec.Template.Spec.Containers
	for _, c := range containers {
		if c.Name == "manager" || c.Name == kustomizeControllerAppLabel || len(containers) == 1 {
			return featureGateEnabledInArgs(append(append([]string{}, c.Command...), c.Args...), gate)
		}
	}
	return false
}

// featureGateEnabledInArgs parses --feature-gates flags (both "--feature-gates=a=true,b=false"
// and "--feature-gates a=true" forms). When the flag is repeated, the last value wins,
// matching pflag map semantics for each key.
func featureGateEnabledInArgs(args []string, gate string) bool {
	enabled := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var value string
		switch {
		case strings.HasPrefix(arg, "--feature-gates="):
			value = strings.TrimPrefix(arg, "--feature-gates=")
		case arg == "--feature-gates" && i+1 < len(args):
			i++
			value = args[i]
		default:
			continue
		}
		for _, pair := range strings.Split(value, ",") {
			k, v, found := strings.Cut(strings.TrimSpace(pair), "=")
			if !found || strings.TrimSpace(k) != gate {
				continue
			}
			if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
				enabled = b
			}
		}
	}
	return enabled
}
