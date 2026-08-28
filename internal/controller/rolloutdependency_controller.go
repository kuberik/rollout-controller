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
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kuberikcomv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// RolloutDependencyReconciler reconciles a RolloutDependency object.
//
// It translates an inter-service version dependency into the gate vocabulary the
// Rollout controller already understands: for each RolloutDependency it
// maintains one RolloutGate whose allowedVersions list holds exactly those
// consumer releases whose contract requirement is satisfied by what the provider
// Rollout has deployed. Rollout admission itself is untouched — it keeps evaluating
// gates the same way it does for schedules and manual approvals.
type RolloutDependencyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutdependencies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutdependencies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutdependencies/finalizers,verbs=update
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates,verbs=get;list;watch;create;update;patch;delete

// Reconcile evaluates a RolloutDependency and syncs the RolloutGate it manages.
func (r *RolloutDependencyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	dependency := &kuberikcomv1alpha1.RolloutDependency{}
	if err := r.Get(ctx, req.NamespacedName, dependency); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	contract := dependency.Spec.ContractName()
	consumerName := dependency.Spec.RolloutRef.Name
	if consumerName == "" {
		return ctrl.Result{}, r.markNotReady(ctx, dependency, "ConsumerNotSpecified",
			"spec.rolloutRef.name is empty")
	}

	// Establish the gate closed before anything that can fail.
	//
	// An absent gate is not a neutral state: evaluateGates only sees gates that
	// exist, so a dependency that errors out before its first successful
	// evaluation would leave the consumer entirely unrestricted. Publishing an
	// empty allow list first means every failure path below degrades to
	// "nothing admitted" rather than "no opinion".
	if _, err := r.ensureGate(ctx, dependency, consumerName); err != nil {
		return ctrl.Result{}, errors.Join(err,
			r.markNotReady(ctx, dependency, "GateSyncFailed", err.Error()))
	}

	// The consumer Rollout is the one being gated. It must live alongside the
	// dependency, because a RolloutGate can only reference a Rollout in its own
	// namespace.
	consumer := &kuberikcomv1alpha1.Rollout{}
	consumerKey := types.NamespacedName{Namespace: dependency.Namespace, Name: consumerName}
	if err := r.Get(ctx, consumerKey, consumer); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.markNotReady(ctx, dependency, "ConsumerNotFound",
				fmt.Sprintf("Rollout %s not found", consumerKey))
		}
		return ctrl.Result{}, err
	}

	// The provider Rollout may live in another namespace: a shared contract is
	// often produced by a service deployed elsewhere in the cluster.
	//
	// A missing provider must block, not disappear. A dependency created before
	// its provider — or pointing at a typo — would otherwise leave the consumer
	// with no gate at all and free rein to deploy, which is the exact failure
	// this resource exists to prevent.
	provider := &kuberikcomv1alpha1.Rollout{}
	providerKey := types.NamespacedName{
		Namespace: dependency.Spec.ProviderNamespace(dependency.Namespace),
		Name:      dependency.Spec.ProviderRef.Name,
	}
	if err := r.Get(ctx, providerKey, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.markNotReady(ctx, dependency, "ProviderNotFound",
				fmt.Sprintf("Provider Rollout %s not found", providerKey))
		}
		return ctrl.Result{}, err
	}

	// Read the contract version the provider currently has deployed. An unknown
	// version is not an error: the provider may simply not have deployed
	// anything yet, in which case every release requiring the contract stays
	// blocked until it does.
	providedVersion, providedTag := "", ""
	if deployed := deployedRelease(provider); deployed != nil {
		providedTag = deployed.Version.Tag
		if deployed.Version.Version != nil {
			if triple, err := contractTriple(*deployed.Version.Version); err == nil {
				providedVersion = triple.String()
			} else {
				log.V(4).Info("Provider release has an unparseable version annotation",
					"provider", providerKey, "tag", providedTag, "version", *deployed.Version.Version)
			}
		}
	}

	admitted, blocked := evaluateDependency(consumer.Status.AvailableReleases, contract, providedVersion)

	gateName, err := r.syncGate(ctx, dependency, consumerName, admitted)
	if err != nil {
		return ctrl.Result{}, errors.Join(err, r.markNotReady(ctx, dependency, "GateSyncFailed", err.Error()))
	}

	// Report satisfaction against the releases the consumer could actually deploy
	// next, not against every release ever seen: releases already behind the
	// deployed one are irrelevant to whether this dependency is holding anything
	// back.
	blocking := blockedTags(blocked, pendingReleases(consumer))

	dependency.Status.ProvidedVersion = nilIfEmpty(providedVersion)
	dependency.Status.ProvidedTag = nilIfEmpty(providedTag)
	dependency.Status.AdmittedVersions = admitted
	dependency.Status.BlockedReleases = blocked
	dependency.Status.GateName = gateName

	meta.SetStatusCondition(&dependency.Status.Conditions, metav1.Condition{
		Type:               kuberikcomv1alpha1.RolloutDependencyReady,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "GateSynced",
		Message:            fmt.Sprintf("Gate %s allows %d release(s)", gateName, len(admitted)),
	})

	satisfied := metav1.Condition{
		Type:               kuberikcomv1alpha1.RolloutDependencySatisfied,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "DependencySatisfied",
		Message:            fmt.Sprintf("No release is waiting on contract %q", contract),
	}
	if len(blocking) > 0 {
		satisfied.Status = metav1.ConditionFalse
		// "Newer version" would be wrong twice over: an exact constraint is unmet
		// by a provider that has moved *past* it, and an unparseable requirement
		// is never resolved by waiting at all — that one needs the image fixed.
		if unevaluable(blocked, blocking) {
			satisfied.Reason = "RequirementUnevaluable"
			satisfied.Message = fmt.Sprintf(
				"Cannot evaluate contract %q for release(s) %s; fix the requires annotation on the image",
				contract, summarizeTags(blocking))
		} else {
			satisfied.Reason = "WaitingForProvider"
			satisfied.Message = fmt.Sprintf(
				"Waiting for %s to serve contract %q at a version satisfying release(s) %s (deployed: %s)",
				providerKey, contract, summarizeTags(blocking), orNone(providedVersion))
		}
	}
	meta.SetStatusCondition(&dependency.Status.Conditions, satisfied)

	if err := r.Status().Update(ctx, dependency); err != nil {
		return ctrl.Result{}, err
	}

	log.V(4).Info("Evaluated rollout dependency",
		"contract", contract, "provided", providedVersion, "admitted", len(admitted), "blocking", len(blocking))

	return ctrl.Result{}, nil
}

// pendingReleases returns the tags the consumer Rollout could still move to,
// i.e. releases newer than the one currently deployed.
//
// An empty result means the consumer has nothing to advance to — either there
// are no releases at all, or its deployed tag is no longer in availableReleases
// (retention pruned it), so no upgrade path can be computed. Both are correctly
// read as "this dependency is holding nothing back": the Rollout has no
// candidate to deploy regardless of what any gate says.
func pendingReleases(consumer *kuberikcomv1alpha1.Rollout) []string {
	candidates, err := getNextReleaseCandidates(consumer.Status.AvailableReleases, &consumer.Status)
	if err != nil {
		return nil
	}
	tags := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		tags = append(tags, candidate.Tag)
	}
	return tags
}

// unevaluable reports whether any currently-blocking release is blocked because
// its requirement could not be parsed, rather than because the provider has not
// caught up. The two need different advice: one waits, the other needs a human.
func unevaluable(blocked []kuberikcomv1alpha1.BlockedRelease, blocking []string) bool {
	for _, release := range blocked {
		if release.Reason == "InvalidVersion" && slices.Contains(blocking, release.Tag) {
			return true
		}
	}
	return false
}

// blockedTags returns the tags of blocked releases that the consumer could
// otherwise deploy right now. Releases behind the deployed one are excluded:
// they are unreachable anyway, so reporting them would make the dependency look
// unsatisfied when it is holding nothing back.
func blockedTags(blocked []kuberikcomv1alpha1.BlockedRelease, pending []string) []string {
	var tags []string
	for _, release := range blocked {
		if slices.Contains(pending, release.Tag) {
			tags = append(tags, release.Tag)
		}
	}
	return tags
}

// syncGate creates or updates the RolloutGate that publishes this dependency's
// verdict to the consumer Rollout, and returns the gate name.
//
// The gate is always marked passing and carries an explicit allow list: that
// way a dependency holds back only the releases whose contract requirement is
// unmet, instead of freezing the consumer entirely. An empty allow list blocks
// every release, which is what "the provider has not caught up yet" means.
func (r *RolloutDependencyReconciler) syncGate(
	ctx context.Context,
	dependency *kuberikcomv1alpha1.RolloutDependency,
	consumerName string,
	admitted []string,
) (string, error) {
	passing := true
	allowed := admitted
	if allowed == nil {
		// A nil AllowedVersions means "no opinion" to the Rollout controller,
		// while an empty list means "nothing is allowed". Blocking is the
		// intended meaning here.
		allowed = []string{}
	}

	gate := &kuberikcomv1alpha1.RolloutGate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dependencyGateName(dependency),
			Namespace: dependency.Namespace,
		},
	}

	_, err := ctrl.CreateOrUpdate(ctx, r.Client, gate, func() error {
		if gate.Labels == nil {
			gate.Labels = map[string]string{}
		}
		gate.Labels[LabelDependencyName] = dependency.Name
		gate.Labels[LabelRolloutName] = consumerName

		if gate.Annotations == nil {
			gate.Annotations = map[string]string{}
		}
		gate.Annotations["gate.kuberik.com/pretty-name"] = fmt.Sprintf("Depends on %s", dependency.Spec.ProviderRef.Name)
		gate.Annotations["gate.kuberik.com/description"] = fmt.Sprintf(
			"Holds back releases that require a newer version of contract %q than %s has deployed.",
			dependency.Spec.ContractName(), dependency.Spec.ProviderRef.Name)

		gate.Spec.RolloutRef = &corev1.LocalObjectReference{Name: consumerName}
		gate.Spec.Passing = &passing
		gate.Spec.AllowedVersions = &allowed

		return ctrl.SetControllerReference(dependency, gate, r.Scheme)
	})
	if err != nil {
		return "", fmt.Errorf("failed to sync RolloutGate: %w", err)
	}

	return gate.Name, nil
}

// ensureGate makes sure the managed gate exists before evaluation runs, so that
// a failure part-way through leaves the consumer blocked rather than
// unrestricted. An existing gate is left untouched — its allow list is this
// dependency's last good verdict, and re-closing it on every reconcile would
// stall deploys for the duration of any transient error.
func (r *RolloutDependencyReconciler) ensureGate(
	ctx context.Context,
	dependency *kuberikcomv1alpha1.RolloutDependency,
	consumerName string,
) (string, error) {
	gate := &kuberikcomv1alpha1.RolloutGate{}
	key := types.NamespacedName{Name: dependencyGateName(dependency), Namespace: dependency.Namespace}
	switch err := r.Get(ctx, key, gate); {
	case err == nil:
		return gate.Name, nil
	case apierrors.IsNotFound(err):
		return r.syncGate(ctx, dependency, consumerName, nil)
	default:
		return "", err
	}
}

// dependencyGateName is the deterministic name of the gate managed by a
// dependency. One dependency owns exactly one gate.
func dependencyGateName(dependency *kuberikcomv1alpha1.RolloutDependency) string {
	return "dependency-" + dependency.Name
}

// markNotReady records a terminal evaluation failure on the dependency.
func (r *RolloutDependencyReconciler) markNotReady(
	ctx context.Context,
	dependency *kuberikcomv1alpha1.RolloutDependency,
	reason, message string,
) error {
	meta.SetStatusCondition(&dependency.Status.Conditions, metav1.Condition{
		Type:               kuberikcomv1alpha1.RolloutDependencyReady,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	})
	if r.Recorder != nil {
		r.Recorder.Event(dependency, corev1.EventTypeWarning, reason, message)
	}
	return r.Status().Update(ctx, dependency)
}

// summarizeTags renders a tag list for a condition message, which the API server
// caps at 32KiB. The full set always stays in status.blockedReleases; a message
// that outgrew the cap would make every status write fail with a non-retryable
// 422 and wedge the reconciler for good.
func summarizeTags(tags []string) string {
	const shown = 10
	if len(tags) <= shown {
		return fmt.Sprintf("%v", tags)
	}
	return fmt.Sprintf("%v and %d more", tags[:shown], len(tags)-shown)
}

func nilIfEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

// SetupWithManager sets up the controller with the Manager.
//
// Dependencies are re-evaluated whenever either side moves: the provider,
// because it may have just deployed a newer contract version, and the consumer,
// because it may have just discovered new releases to admit or block.
func (r *RolloutDependencyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("rolloutdependency-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuberikcomv1alpha1.RolloutDependency{}).
		Owns(&kuberikcomv1alpha1.RolloutGate{}).
		Watches(
			&kuberikcomv1alpha1.Rollout{},
			handler.EnqueueRequestsFromMapFunc(r.findDependenciesForRollout),
		).
		Named("rolloutdependency").
		Complete(r)
}

// findDependenciesForRollout maps a Rollout to every RolloutDependency that
// references it, as either the gated consumer or the contract provider.
func (r *RolloutDependencyReconciler) findDependenciesForRollout(ctx context.Context, obj client.Object) []reconcile.Request {
	rollout, ok := obj.(*kuberikcomv1alpha1.Rollout)
	if !ok {
		return nil
	}

	dependencyList := &kuberikcomv1alpha1.RolloutDependencyList{}
	if err := r.List(ctx, dependencyList); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list RolloutDependencies")
		return nil
	}

	var requests []reconcile.Request
	for _, dependency := range dependencyList.Items {
		isConsumer := dependency.Namespace == rollout.Namespace &&
			dependency.Spec.RolloutRef.Name == rollout.Name
		isProvider := dependency.Spec.ProviderNamespace(dependency.Namespace) == rollout.Namespace &&
			dependency.Spec.ProviderRef.Name == rollout.Name

		if isConsumer || isProvider {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: dependency.Namespace, Name: dependency.Name},
			})
		}
	}
	return requests
}
