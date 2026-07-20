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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/rollout/v1alpha1"
	"github.com/kuberik/rollout-controller/pkg/rollouttest/upstream"
)

const rolloutTestUpstreamGateRequeue = 15 * time.Second

// RolloutTestUpstreamGateReconciler evaluates cross-environment upstream gates
// for RolloutTest resources and publishes the result in status.upstreamGate.
type RolloutTestUpstreamGateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=rollout.kuberik.com,resources=rollouttests,verbs=get;list;watch
// +kubebuilder:rbac:groups=rollout.kuberik.com,resources=rollouttests/status,verbs=get;update;patch

func (r *RolloutTestUpstreamGateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rolloutTest rolloutv1alpha1.RolloutTest
	if err := r.Get(ctx, req.NamespacedName, &rolloutTest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if rolloutTest.Spec.UpstreamGate == nil {
		return ctrl.Result{}, nil
	}

	upstreamTests, err := r.listUpstreamRolloutTests(ctx, rolloutTest.Spec.UpstreamGate.Environment)
	if err != nil {
		return ctrl.Result{}, err
	}

	result := upstream.Evaluate(upstream.Input{
		LocalTest:            rolloutTest,
		TargetCanaryRevision: rolloutTest.Status.ObservedCanaryRevision,
		UpstreamTests:        upstreamTests,
	})

	original := rolloutTest.Status.DeepCopy()
	rolloutTest.Status.UpstreamGate = &result.Status
	setUpstreamGateCondition(&rolloutTest, result.Status)

	if upstreamGateStatusChanged(original.UpstreamGate, rolloutTest.Status.UpstreamGate) ||
		upstreamGateConditionChanged(original.Conditions, rolloutTest.Status.Conditions) {
		if err := r.Status().Update(ctx, &rolloutTest); err != nil {
			return ctrl.Result{}, err
		}
	}

	if result.Status.State == rolloutv1alpha1.RolloutTestUpstreamGateBlocked {
		return ctrl.Result{RequeueAfter: rolloutTestUpstreamGateRequeue}, nil
	}

	return ctrl.Result{}, nil
}

func (r *RolloutTestUpstreamGateReconciler) listUpstreamRolloutTests(
	ctx context.Context,
	environment string,
) ([]rolloutv1alpha1.RolloutTest, error) {
	var rolloutTests rolloutv1alpha1.RolloutTestList
	if err := r.List(ctx, &rolloutTests, client.MatchingLabels{
		rolloutv1alpha1.RolloutTestEnvironmentLabel: environment,
	}); err != nil {
		return nil, err
	}
	return rolloutTests.Items, nil
}

func setUpstreamGateCondition(
	rolloutTest *rolloutv1alpha1.RolloutTest,
	status rolloutv1alpha1.RolloutTestUpstreamGateStatus,
) {
	conditionStatus := metav1.ConditionFalse
	reason := "UpstreamBlocked"
	switch status.State {
	case rolloutv1alpha1.RolloutTestUpstreamGateAllowed:
		conditionStatus = metav1.ConditionTrue
		reason = "UpstreamSucceeded"
	case rolloutv1alpha1.RolloutTestUpstreamGateFailed:
		reason = "UpstreamFailed"
	case rolloutv1alpha1.RolloutTestUpstreamGateBlocked:
		reason = "UpstreamBlocked"
	}

	meta.SetStatusCondition(&rolloutTest.Status.Conditions, metav1.Condition{
		Type:               rolloutv1alpha1.RolloutTestConditionUpstreamGate,
		Status:             conditionStatus,
		ObservedGeneration: rolloutTest.Generation,
		Reason:             reason,
		Message:            status.Message,
		LastTransitionTime: metav1.Now(),
	})
}

func upstreamGateStatusChanged(
	before, after *rolloutv1alpha1.RolloutTestUpstreamGateStatus,
) bool {
	if before == nil && after == nil {
		return false
	}
	if before == nil || after == nil {
		return true
	}
	return before.State != after.State ||
		before.Message != after.Message ||
		before.ObservedCanaryRevision != after.ObservedCanaryRevision ||
		before.ObservedUpstreamPhase != after.ObservedUpstreamPhase ||
		before.UpstreamRolloutTestName != after.UpstreamRolloutTestName ||
		before.UpstreamEnvironment != after.UpstreamEnvironment
}

func upstreamGateConditionChanged(before, after []metav1.Condition) bool {
	beforeCond := meta.FindStatusCondition(before, rolloutv1alpha1.RolloutTestConditionUpstreamGate)
	afterCond := meta.FindStatusCondition(after, rolloutv1alpha1.RolloutTestConditionUpstreamGate)
	if beforeCond == nil && afterCond == nil {
		return false
	}
	if beforeCond == nil || afterCond == nil {
		return true
	}
	return beforeCond.Status != afterCond.Status ||
		beforeCond.Reason != afterCond.Reason ||
		beforeCond.Message != afterCond.Message
}

func (r *RolloutTestUpstreamGateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutv1alpha1.RolloutTest{}).
		Watches(
			&rolloutv1alpha1.RolloutTest{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				return MapRolloutTestsForUpstreamEnvironment(ctx, r.Client, obj)
			}),
		).
		Named("rollouttest-upstream-gate").
		Complete(r)
}

// MapRolloutTestsForUpstreamEnvironment enqueues downstream RolloutTests that
// depend on the updated upstream-environment RolloutTest.
func MapRolloutTestsForUpstreamEnvironment(ctx context.Context, c client.Reader, obj client.Object) []reconcile.Request {
	upstreamTest, ok := obj.(*rolloutv1alpha1.RolloutTest)
	if !ok {
		return nil
	}

	environment := upstreamTest.Labels[rolloutv1alpha1.RolloutTestEnvironmentLabel]
	if environment == "" {
		return nil
	}

	var rolloutTests rolloutv1alpha1.RolloutTestList
	if err := c.List(ctx, &rolloutTests); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for i := range rolloutTests.Items {
		candidate := rolloutTests.Items[i]
		gate := candidate.Spec.UpstreamGate
		if gate == nil || gate.Environment != environment {
			continue
		}
		upstreamName := candidate.Name
		if gate.RolloutTestName != "" {
			upstreamName = gate.RolloutTestName
		}
		if upstreamName != upstreamTest.Name {
			continue
		}
		if candidate.Spec.RolloutName != upstreamTest.Spec.RolloutName {
			continue
		}
		if candidate.Spec.StepIndex != upstreamTest.Spec.StepIndex {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&candidate),
		})
	}
	return requests
}
