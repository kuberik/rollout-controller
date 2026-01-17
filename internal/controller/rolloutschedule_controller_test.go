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
	"testing"
	"time"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// MockClock implements Clock interface for testing
type MockClock struct {
	CurrentTime time.Time
}

func (m *MockClock) Now() time.Time {
	return m.CurrentTime
}

func TestEvaluateScheduleRules(t *testing.T) {
	// Use UTC for simplicity in tests
	locUTC, _ := time.LoadLocation("UTC")

	tests := []struct {
		name           string
		now            time.Time
		rules          []rolloutv1alpha1.ScheduleRule
		timezone       string
		expectedActive bool
		expectedRules  []string
	}{
		{
			name: "Time range: inside window",
			now:  time.Date(2025, 1, 1, 10, 0, 0, 0, locUTC), // 10:00
			rules: []rolloutv1alpha1.ScheduleRule{
				{
					Name: "business-hours",
					TimeRange: &rolloutv1alpha1.TimeRange{
						Start: "09:00",
						End:   "17:00",
					},
				},
			},
			timezone:       "UTC",
			expectedActive: true,
			expectedRules:  []string{"business-hours"},
		},
		{
			name: "Time range: outside window (before)",
			now:  time.Date(2025, 1, 1, 8, 0, 0, 0, locUTC), // 8:00
			rules: []rolloutv1alpha1.ScheduleRule{
				{
					Name: "business-hours",
					TimeRange: &rolloutv1alpha1.TimeRange{
						Start: "09:00",
						End:   "17:00",
					},
				},
			},
			timezone:       "UTC",
			expectedActive: false,
			expectedRules:  nil,
		},
		{
			name: "Time range: cross midnight (inside)",
			now:  time.Date(2025, 1, 1, 23, 0, 0, 0, locUTC), // 23:00
			rules: []rolloutv1alpha1.ScheduleRule{
				{
					Name: "night-shift",
					TimeRange: &rolloutv1alpha1.TimeRange{
						Start: "22:00",
						End:   "06:00",
					},
				},
			},
			timezone:       "UTC",
			expectedActive: true,
			expectedRules:  []string{"night-shift"},
		},
		{
			name: "Time range: cross midnight (outside)",
			now:  time.Date(2025, 1, 1, 12, 0, 0, 0, locUTC), // 12:00
			rules: []rolloutv1alpha1.ScheduleRule{
				{
					Name: "night-shift",
					TimeRange: &rolloutv1alpha1.TimeRange{
						Start: "22:00",
						End:   "06:00",
					},
				},
			},
			timezone:       "UTC",
			expectedActive: false,
			expectedRules:  nil,
		},
		{
			name: "Days of week: match",
			now:  time.Date(2025, 1, 1, 12, 0, 0, 0, locUTC), // Wed Jan 1 2025
			rules: []rolloutv1alpha1.ScheduleRule{
				{
					Name: "wed-only",
					DaysOfWeek: []rolloutv1alpha1.DayOfWeek{
						rolloutv1alpha1.Wednesday,
					},
				},
			},
			timezone:       "UTC",
			expectedActive: true,
			expectedRules:  []string{"wed-only"},
		},
		{
			name: "Days of week: mismatch",
			now:  time.Date(2025, 1, 2, 12, 0, 0, 0, locUTC), // Thu Jan 2 2025
			rules: []rolloutv1alpha1.ScheduleRule{
				{
					Name: "wed-only",
					DaysOfWeek: []rolloutv1alpha1.DayOfWeek{
						rolloutv1alpha1.Wednesday,
					},
				},
			},
			timezone:       "UTC",
			expectedActive: false,
			expectedRules:  nil,
		},
		{
			name: "Date range: match",
			now:  time.Date(2025, 12, 25, 12, 0, 0, 0, locUTC),
			rules: []rolloutv1alpha1.ScheduleRule{
				{
					Name: "christmas",
					DateRange: &rolloutv1alpha1.DateRange{
						Start: "2025-12-24",
						End:   "2025-12-26",
					},
				},
			},
			timezone:       "UTC",
			expectedActive: true,
			expectedRules:  []string{"christmas"},
		},
		{
			name: "Date range: mismatch",
			now:  time.Date(2025, 12, 27, 12, 0, 0, 0, locUTC),
			rules: []rolloutv1alpha1.ScheduleRule{
				{
					Name: "christmas",
					DateRange: &rolloutv1alpha1.DateRange{
						Start: "2025-12-24",
						End:   "2025-12-26",
					},
				},
			},
			timezone:       "UTC",
			expectedActive: false,
			expectedRules:  nil,
		},
		{
			name: "Multiple rules (OR logic)",
			now:  time.Date(2025, 1, 1, 10, 0, 0, 0, locUTC),
			rules: []rolloutv1alpha1.ScheduleRule{
				{
					Name: "morning",
					TimeRange: &rolloutv1alpha1.TimeRange{
						Start: "09:00",
						End:   "11:00",
					},
				},
				{
					Name: "afternoon",
					TimeRange: &rolloutv1alpha1.TimeRange{
						Start: "14:00",
						End:   "16:00",
					},
				},
			},
			timezone:       "UTC",
			expectedActive: true,
			expectedRules:  []string{"morning"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			active, activeRules, _, err := evaluateScheduleRules(tt.now, tt.rules, tt.timezone)
			require.NoError(t, err)
			assert.Equal(t, tt.expectedActive, active)
			if tt.expectedRules != nil {
				assert.Equal(t, tt.expectedRules, activeRules)
			}
		})
	}
}

func TestRolloutScheduleReconciler(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, rolloutv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	// Setup basic objects
	rollout := &rolloutv1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-rollout",
			Namespace: "default",
			Labels: map[string]string{
				"app": "my-app",
			},
		},
	}

	schedule := &rolloutv1alpha1.RolloutSchedule{
		TypeMeta: metav1.TypeMeta{
			APIVersion: rolloutv1alpha1.GroupVersion.String(),
			Kind:       "RolloutSchedule",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "business-hours",
			Namespace: "default",
		},
		Spec: rolloutv1alpha1.RolloutScheduleSpec{
			RolloutSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "my-app"},
			},
			Rules: []rolloutv1alpha1.ScheduleRule{
				{
					Name: "workday",
					TimeRange: &rolloutv1alpha1.TimeRange{
						Start: "09:00",
						End:   "17:00",
					},
				},
			},
			Action: rolloutv1alpha1.RolloutScheduleActionAllow,
		},
	}

	// Mock clock at 10:00 (inside window)
	mockClock := &MockClock{
		CurrentTime: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rollout, schedule).
		WithStatusSubresource(schedule). // Add status subresource support
		Build()

	// Verify object exists
	checkSchedule := &rolloutv1alpha1.RolloutSchedule{}
	err := client.Get(context.Background(), types.NamespacedName{Name: schedule.Name, Namespace: schedule.Namespace}, checkSchedule)
	require.NoError(t, err, "Failed to find schedule in fake client during setup")

	r := &RolloutScheduleReconciler{
		Client:   client,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
		Clock:    mockClock,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      schedule.Name,
			Namespace: schedule.Namespace,
		},
	}

	// 1. First Reconciliation - Should create gate passing=true (Allow action + Inside window)
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	// Verify gate created
	gate := &rolloutv1alpha1.RolloutGate{}
	gateName := "business-hours-my-rollout" // schedule-rollout
	err = client.Get(context.Background(), types.NamespacedName{Name: gateName, Namespace: "default"}, gate)
	require.NoError(t, err)
	require.NotNil(t, gate.Spec.Passing)
	assert.True(t, *gate.Spec.Passing, "Gate should be passing (Allow + Inside window)")
	assert.Equal(t, rollout.Name, gate.Spec.RolloutRef.Name)

	// Verify status updated
	err = client.Get(context.Background(), types.NamespacedName{Name: schedule.Name, Namespace: schedule.Namespace}, schedule)
	require.NoError(t, err)
	assert.True(t, schedule.Status.Active)
	assert.Contains(t, schedule.Status.ManagedGates, gateName)

	// 2. Advance time to 20:00 (outside window)
	mockClock.CurrentTime = time.Date(2025, 1, 1, 20, 0, 0, 0, time.UTC)

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	// Verify gate updated to passing=false
	err = client.Get(context.Background(), types.NamespacedName{Name: gateName, Namespace: "default"}, gate)
	require.NoError(t, err)
	require.NotNil(t, gate.Spec.Passing)
	assert.False(t, *gate.Spec.Passing, "Gate should NOT be passing (Allow + Outside window)")

	// 3. Change Action to Deny
	err = client.Get(context.Background(), types.NamespacedName{Name: schedule.Name, Namespace: schedule.Namespace}, schedule)
	require.NoError(t, err)
	schedule.Spec.Action = rolloutv1alpha1.RolloutScheduleActionDeny
	err = client.Update(context.Background(), schedule)
	require.NoError(t, err)

	// Reconcile (still outside window at 20:00)
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	// Outside window + Deny action = Passing (Deny only blocks active periods)
	err = client.Get(context.Background(), types.NamespacedName{Name: gateName, Namespace: "default"}, gate)
	require.NoError(t, err)
	assert.True(t, *gate.Spec.Passing, "Gate should be passing (Deny + Outside window)")

	// 4. Advance time back to 10:00 (inside window)
	mockClock.CurrentTime = time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC)

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	// Inside window + Deny action = Not Passing
	err = client.Get(context.Background(), types.NamespacedName{Name: gateName, Namespace: "default"}, gate)
	require.NoError(t, err)
	assert.False(t, *gate.Spec.Passing, "Gate should NOT be passing (Deny + Inside window)")
}

func TestClusterRolloutScheduleReconciler(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, rolloutv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	// Objects: Rollout in "prod", Rollout in "dev"
	prodRollout := &rolloutv1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-prod",
			Namespace: "prod",
			Labels:    map[string]string{"app": "foo"},
		},
	}
	devRollout := &rolloutv1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-dev",
			Namespace: "dev",
			Labels:    map[string]string{"app": "foo"},
		},
	}

	prodNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod", Labels: map[string]string{"env": "prod"}}}
	devNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "dev", Labels: map[string]string{"env": "dev"}}}

	schedule := &rolloutv1alpha1.ClusterRolloutSchedule{
		TypeMeta: metav1.TypeMeta{
			APIVersion: rolloutv1alpha1.GroupVersion.String(),
			Kind:       "ClusterRolloutSchedule",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "prod-freeze",
		},
		Spec: rolloutv1alpha1.ClusterRolloutScheduleSpec{
			// Start with namespace selector
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"env": "prod"},
			},
			RolloutSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "foo"},
			},
			Rules: []rolloutv1alpha1.ScheduleRule{
				{
					Name: "freeze",
					// Always active date range for test
					DateRange: &rolloutv1alpha1.DateRange{
						Start: "2025-01-01",
						End:   "2025-01-02",
					},
				},
			},
			Action: rolloutv1alpha1.RolloutScheduleActionDeny,
		},
	}

	mockClock := &MockClock{
		CurrentTime: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(prodRollout, devRollout, prodNs, devNs, schedule).
		WithStatusSubresource(schedule).
		Build()

	// Verify object exists
	checkSchedule := &rolloutv1alpha1.ClusterRolloutSchedule{}
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: schedule.Name}, checkSchedule)
	require.NoError(t, err, "Failed to find cluster schedule in fake client during setup")

	r := &ClusterRolloutScheduleReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
		Clock:    mockClock,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name: schedule.Name,
		},
	}

	// 1. Reconcile - Should affect only prod rollout
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	// Check prod gate
	prodGate := &rolloutv1alpha1.RolloutGate{}
	prodGateName := "prod-freeze-app-prod"
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: prodGateName, Namespace: "prod"}, prodGate)
	require.NoError(t, err)
	assert.False(t, *prodGate.Spec.Passing, "Prod gate should block")

	// Check dev gate (should not exist)
	devGate := &rolloutv1alpha1.RolloutGate{}
	devGateName := "prod-freeze-app-dev"
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: devGateName, Namespace: "dev"}, devGate)
	assert.Error(t, err)
	assert.True(t, client.IgnoreNotFound(err) == nil)

	// 2. Remove Namespace Selector (matches all)
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: schedule.Name}, schedule)
	require.NoError(t, err)
	schedule.Spec.NamespaceSelector = &metav1.LabelSelector{} // Empty matches all?
	// Make it nil or empty
	// Actually metav1.LabelSelector{} (empty) matches everything in LabelSelectorAsSelector?
	// Wait, empty label selector matches everything.

	err = fakeClient.Update(context.Background(), schedule)
	require.NoError(t, err)

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	// Now dev gate should exist and block
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: devGateName, Namespace: "dev"}, devGate)
	require.NoError(t, err)
	assert.False(t, *devGate.Spec.Passing, "Dev gate should block now")
}
