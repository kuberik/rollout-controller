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
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

func TestOnlyConflicts(t *testing.T) {
	gr := schema.GroupResource{Group: "kuberik.com", Resource: "rolloutgates"}
	conflict := fmt.Errorf("rollout a/b: %w", apierrors.NewConflict(gr, "g", errors.New("stale")))
	other := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"wrapped conflict", conflict, true},
		{"joined conflicts", errors.Join(conflict, conflict), true},
		{"conflict joined with another error", errors.Join(conflict, other), false},
		{"other error", other, false},
	}
	for _, c := range cases {
		if got := onlyConflicts(c.err); got != c.want {
			t.Errorf("%s: onlyConflicts = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSetControllerOwnerRefDropsStaleController(t *testing.T) {
	stale := metav1.OwnerReference{APIVersion: "kuberik.com/v1alpha1", Kind: "ApprovalPolicy", Name: "p", UID: "old", Controller: ptr.To(true)}
	other := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "cm", UID: "cm"}
	fresh := metav1.OwnerReference{APIVersion: "kuberik.com/v1alpha1", Kind: "ApprovalPolicy", Name: "p", UID: "new", Controller: ptr.To(true)}
	gate := &rolloutv1alpha1.RolloutGate{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{stale, other}}}

	setControllerOwnerRef(gate, fresh)

	if len(gate.OwnerReferences) != 2 || gate.OwnerReferences[0].UID != "cm" || gate.OwnerReferences[1].UID != "new" {
		t.Fatalf("owner references = %+v, want [cm, new]", gate.OwnerReferences)
	}
}
