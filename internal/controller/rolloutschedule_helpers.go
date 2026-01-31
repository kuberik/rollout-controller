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
	"time"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// evaluateScheduleRules determines if any rule in the schedule is currently active.
// Returns whether we're active, which rules are active, and when the next transition will occur.
func evaluateScheduleRules(now time.Time, rules []rolloutv1alpha1.ScheduleRule, timezone string) (bool, []string, time.Time, error) {
	// Load the timezone
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return false, nil, time.Time{}, fmt.Errorf("invalid timezone %q: %w", timezone, err)
	}

	nowInTZ := now.In(loc)
	var activeRules []string
	var nextTransition time.Time

	// Evaluate each rule
	for _, rule := range rules {
		active, ruleNextTransition, err := evaluateRule(nowInTZ, rule, loc)
		if err != nil {
			return false, nil, time.Time{}, fmt.Errorf("failed to evaluate rule %q: %w", rule.Name, err)
		}

		if active {
			activeRules = append(activeRules, rule.Name)
		}

		// Track the earliest next transition
		if !ruleNextTransition.IsZero() {
			if nextTransition.IsZero() || ruleNextTransition.Before(nextTransition) {
				nextTransition = ruleNextTransition
			}
		}
	}

	// Schedule is active if ANY rule is active
	isActive := len(activeRules) > 0

	return isActive, activeRules, nextTransition, nil
}

// evaluateRule evaluates a single schedule rule.
func evaluateRule(now time.Time, rule rolloutv1alpha1.ScheduleRule, loc *time.Location) (bool, time.Time, error) {
	// Check date range first (if specified)
	if rule.DateRange != nil {
		inDateRange, err := isInDateRange(now, rule.DateRange, loc)
		if err != nil {
			return false, time.Time{}, err
		}
		if !inDateRange {
			// Not in date range, rule doesn't match
			// Next transition is at the start of the date range (or end if we're past it)
			nextTransition := calculateDateRangeTransition(now, rule.DateRange, loc)
			return false, nextTransition, nil
		}
	}

	// Check day of week (if specified)
	if len(rule.DaysOfWeek) > 0 {
		currentDay := now.Weekday()
		dayMatches := false
		for _, allowedDay := range rule.DaysOfWeek {
			if dayOfWeekMatches(currentDay, allowedDay) {
				dayMatches = true
				break
			}
		}
		if !dayMatches {
			// Wrong day, rule doesn't match
			nextTransition := findNextMatchingDay(now, rule.DaysOfWeek, rule.TimeRange, loc)
			return false, nextTransition, nil
		}
	}

	// Check time range (if specified)
	if rule.TimeRange != nil {
		return isInTimeRange(now, rule.TimeRange, rule.DaysOfWeek, loc)
	}

	// No time range specified but date/day matched - rule is active all day
	// Next transition is midnight tomorrow (or next day change)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	nextTransition := midnight.AddDate(0, 0, 1)
	return true, nextTransition, nil
}

// isInDateRange checks if the current date is within the specified date range.
func isInDateRange(now time.Time, dateRange *rolloutv1alpha1.DateRange, loc *time.Location) (bool, error) {
	startDate, err := time.ParseInLocation("2006-01-02", dateRange.Start, loc)
	if err != nil {
		return false, fmt.Errorf("invalid start date %q: %w", dateRange.Start, err)
	}

	endDate, err := time.ParseInLocation("2006-01-02", dateRange.End, loc)
	if err != nil {
		return false, fmt.Errorf("invalid end date %q: %w", dateRange.End, err)
	}

	// Normalize to midnight for date comparison
	currentDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	startDate = time.Date(startDate.Year(), startDate.Month(), startDate.Day(), 0, 0, 0, 0, loc)
	endDate = time.Date(endDate.Year(), endDate.Month(), endDate.Day(), 0, 0, 0, 0, loc)

	return (currentDate.Equal(startDate) || currentDate.After(startDate)) &&
		(currentDate.Equal(endDate) || currentDate.Before(endDate)), nil
}

// calculateDateRangeTransition calculates when the next transition will occur for a date range.
func calculateDateRangeTransition(now time.Time, dateRange *rolloutv1alpha1.DateRange, loc *time.Location) time.Time {
	startDate, _ := time.ParseInLocation("2006-01-02", dateRange.Start, loc)
	endDate, _ := time.ParseInLocation("2006-01-02", dateRange.End, loc)

	currentDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	if currentDate.Before(startDate) {
		// Before the range, next transition is start date
		return startDate
	}

	// After or in the range, next transition is day after end date
	return endDate.AddDate(0, 0, 1)
}

// dayOfWeekMatches checks if a time.Weekday matches a DayOfWeek.
func dayOfWeekMatches(weekday time.Weekday, day rolloutv1alpha1.DayOfWeek) bool {
	switch day {
	case rolloutv1alpha1.Monday:
		return weekday == time.Monday
	case rolloutv1alpha1.Tuesday:
		return weekday == time.Tuesday
	case rolloutv1alpha1.Wednesday:
		return weekday == time.Wednesday
	case rolloutv1alpha1.Thursday:
		return weekday == time.Thursday
	case rolloutv1alpha1.Friday:
		return weekday == time.Friday
	case rolloutv1alpha1.Saturday:
		return weekday == time.Saturday
	case rolloutv1alpha1.Sunday:
		return weekday == time.Sunday
	default:
		return false
	}
}

// isInTimeRange checks if the current time is within the time range.
func isInTimeRange(now time.Time, tr *rolloutv1alpha1.TimeRange, daysOfWeek []rolloutv1alpha1.DayOfWeek, loc *time.Location) (bool, time.Time, error) {
	startOffset, err := parseTimeOfDay(tr.Start)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("invalid start time %q: %w", tr.Start, err)
	}

	endOffset, err := parseTimeOfDay(tr.End)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("invalid end time %q: %w", tr.End, err)
	}

	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	currentOffset := now.Sub(midnight)

	var inWindow bool
	var nextTransition time.Time

	if startOffset < endOffset {
		// Normal range (e.g., 09:00-17:00)
		inWindow = currentOffset >= startOffset && currentOffset < endOffset
		if inWindow {
			// Next transition is at end time today
			nextTransition = midnight.Add(endOffset)
		} else if currentOffset < startOffset {
			// Before the window starts today
			nextTransition = midnight.Add(startOffset)
		} else {
			// After the window ends today, next is start time tomorrow (if days match)
			nextTransition = findNextTimeRangeStart(now, startOffset, daysOfWeek, loc)
		}
	} else {
		// Crosses midnight (e.g., 22:00-02:00)
		inWindow = currentOffset >= startOffset || currentOffset < endOffset
		if inWindow {
			if currentOffset >= startOffset {
				// After start time today, ends tomorrow
				nextTransition = midnight.AddDate(0, 0, 1).Add(endOffset)
			} else {
				// Before end time today (started yesterday)
				nextTransition = midnight.Add(endOffset)
			}
		} else {
			// Not in window (between end and start)
			nextTransition = midnight.Add(startOffset)
		}
	}

	return inWindow, nextTransition, nil
}

// parseTimeOfDay parses a time string like "09:00" into a duration from midnight.
func parseTimeOfDay(timeStr string) (time.Duration, error) {
	parts := strings.Split(timeStr, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid time format %q, expected HH:MM", timeStr)
	}

	hours, err := strconv.Atoi(parts[0])
	if err != nil || hours < 0 || hours > 23 {
		return 0, fmt.Errorf("invalid hours %q", parts[0])
	}

	minutes, err := strconv.Atoi(parts[1])
	if err != nil || minutes < 0 || minutes > 59 {
		return 0, fmt.Errorf("invalid minutes %q", parts[1])
	}

	return time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute, nil
}

// findNextTimeRangeStart finds the next time the time range will start.
func findNextTimeRangeStart(now time.Time, startOffset time.Duration, daysOfWeek []rolloutv1alpha1.DayOfWeek, loc *time.Location) time.Time {
	// If no day restrictions, next window starts tomorrow at start time
	if len(daysOfWeek) == 0 {
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		return midnight.AddDate(0, 0, 1).Add(startOffset)
	}

	// Find the next matching day
	return findNextMatchingDay(now, daysOfWeek, &rolloutv1alpha1.TimeRange{Start: formatDuration(startOffset)}, loc)
}

// findNextMatchingDay finds the next occurrence of a matching day of week.
func findNextMatchingDay(now time.Time, allowedDays []rolloutv1alpha1.DayOfWeek, tr *rolloutv1alpha1.TimeRange, loc *time.Location) time.Time {
	// Start from tomorrow
	checkDate := now.AddDate(0, 0, 1)

	// Check up to 7 days ahead
	for i := 0; i < 7; i++ {
		dayToCheck := checkDate.AddDate(0, 0, i)
		for _, allowedDay := range allowedDays {
			if dayOfWeekMatches(dayToCheck.Weekday(), allowedDay) {
				midnight := time.Date(dayToCheck.Year(), dayToCheck.Month(), dayToCheck.Day(), 0, 0, 0, 0, loc)
				if tr != nil && tr.Start != "" {
					offset, _ := parseTimeOfDay(tr.Start)
					return midnight.Add(offset)
				}
				return midnight
			}
		}
	}

	// Shouldn't happen, but default to tomorrow
	midnight := time.Date(checkDate.Year(), checkDate.Month(), checkDate.Day(), 0, 0, 0, 0, loc)
	return midnight
}

// formatDuration formats a duration as HH:MM.
func formatDuration(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	return fmt.Sprintf("%02d:%02d", hours, minutes)
}

// calculateGateStatus determines if the gate should pass based on active state and action.
func calculateGateStatus(active bool, action rolloutv1alpha1.RolloutScheduleAction) bool {
	switch action {
	case rolloutv1alpha1.RolloutScheduleActionAllow:
		// Allow when active, deny when inactive
		return active
	case rolloutv1alpha1.RolloutScheduleActionDeny:
		// Deny when active, allow when inactive
		return !active
	default:
		// Default to deny behavior
		return !active
	}
}

// syncRolloutGate creates or updates a RolloutGate for a rollout.
func syncRolloutGate(
	ctx context.Context,
	c client.Client,
	rollout *rolloutv1alpha1.Rollout,
	gateName string,
	passing bool,
	ownerRef metav1.OwnerReference,
	scheduleAnnotations map[string]string,
) error {
	gate := &rolloutv1alpha1.RolloutGate{}
	err := c.Get(ctx, types.NamespacedName{
		Namespace: rollout.Namespace,
		Name:      gateName,
	}, gate)

	if errors.IsNotFound(err) {
		// Create new gate
		// Copy gate-related annotations from schedule to gate
		annotations := make(map[string]string)
		if prettyName, ok := scheduleAnnotations["gate.kuberik.com/pretty-name"]; ok {
			annotations["gate.kuberik.com/pretty-name"] = prettyName
		}
		if description, ok := scheduleAnnotations["gate.kuberik.com/description"]; ok {
			annotations["gate.kuberik.com/description"] = description
		}

		gate = &rolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{
				Name:            gateName,
				Namespace:       rollout.Namespace,
				Annotations:     annotations,
				OwnerReferences: []metav1.OwnerReference{ownerRef},
			},
			Spec: rolloutv1alpha1.RolloutGateSpec{
				RolloutRef: &corev1.LocalObjectReference{
					Name: rollout.Name,
				},
				Passing: &passing,
			},
		}
		return c.Create(ctx, gate)
	}

	if err != nil {
		return fmt.Errorf("failed to get gate %s: %w", gateName, err)
	}

	// Update existing gate if needed
	needsUpdate := false
	if gate.Spec.Passing == nil || *gate.Spec.Passing != passing {
		gate.Spec.Passing = &passing
		needsUpdate = true
	}

	// Update annotations from schedule if needed
	if gate.Annotations == nil {
		gate.Annotations = make(map[string]string)
	}

	if prettyName, ok := scheduleAnnotations["gate.kuberik.com/pretty-name"]; ok {
		if gate.Annotations["gate.kuberik.com/pretty-name"] != prettyName {
			gate.Annotations["gate.kuberik.com/pretty-name"] = prettyName
			needsUpdate = true
		}
	}

	if description, ok := scheduleAnnotations["gate.kuberik.com/description"]; ok {
		if gate.Annotations["gate.kuberik.com/description"] != description {
			gate.Annotations["gate.kuberik.com/description"] = description
			needsUpdate = true
		}
	}

	// Ensure owner reference is set
	hasOwner := false
	for _, ref := range gate.OwnerReferences {
		if ref.UID == ownerRef.UID {
			hasOwner = true
			break
		}
	}
	if !hasOwner {
		gate.OwnerReferences = append(gate.OwnerReferences, ownerRef)
		needsUpdate = true
	}

	if needsUpdate {
		return c.Update(ctx, gate)
	}

	return nil
}

// cleanupOrphanedGates removes gates that are no longer needed.
func cleanupOrphanedGates(
	ctx context.Context,
	c client.Client,
	managedGates []string,
	currentGates []string,
	namespace string,
) error {
	// Convert currentGates to a map for quick lookup
	current := make(map[string]bool)
	for _, name := range currentGates {
		current[name] = true
	}

	// Delete gates that are in managedGates but not in currentGates
	for _, gateName := range managedGates {
		if !current[gateName] {
			gate := &rolloutv1alpha1.RolloutGate{}
			err := c.Get(ctx, types.NamespacedName{
				Namespace: namespace,
				Name:      gateName,
			}, gate)

			if err == nil {
				// Gate exists, delete it
				if err := c.Delete(ctx, gate); err != nil && !errors.IsNotFound(err) {
					return fmt.Errorf("failed to delete orphaned gate %s: %w", gateName, err)
				}
			} else if !errors.IsNotFound(err) {
				return fmt.Errorf("failed to get gate %s for cleanup: %w", gateName, err)
			}
			// If already not found, that's fine
		}
	}

	return nil
}

// makeOwnerReference creates an owner reference for controller-owned objects.
func makeOwnerReference(obj client.Object, scheme *runtime.Scheme) (metav1.OwnerReference, error) {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return metav1.OwnerReference{}, err
	}

	return metav1.OwnerReference{
		APIVersion:         gvk.GroupVersion().String(),
		Kind:               gvk.Kind,
		Name:               obj.GetName(),
		UID:                obj.GetUID(),
		Controller:         pointer.Bool(true),
		BlockOwnerDeletion: pointer.Bool(true),
	}, nil
}
