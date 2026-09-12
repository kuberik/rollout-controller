/*
Copyright 2026.

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

package main

import (
	"strings"
	"testing"

	imagev1 "github.com/fluxcd/image-reflector-controller/api/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	kuberikcomv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

func TestCheckRequiredKinds(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{kuberikcomv1alpha1.GroupVersion, imagev1.GroupVersion})
	mapper.Add(kuberikcomv1alpha1.GroupVersion.WithKind("Rollout"), meta.RESTScopeNamespace)
	mapper.Add(imagev1.GroupVersion.WithKind("ImagePolicy"), meta.RESTScopeNamespace)

	if err := checkRequiredKinds(mapper, scheme, &kuberikcomv1alpha1.Rollout{}, &imagev1.ImagePolicy{}); err != nil {
		t.Fatalf("all kinds served, expected nil, got %v", err)
	}

	err := checkRequiredKinds(mapper, scheme,
		&kuberikcomv1alpha1.Rollout{}, &kuberikcomv1alpha1.RolloutDependency{}, &kuberikcomv1alpha1.RolloutGate{})
	if err == nil {
		t.Fatal("RolloutDependency and RolloutGate are not served, expected an error")
	}
	for _, want := range []string{"RolloutDependency.kuberik.com/v1alpha1", "RolloutGate.kuberik.com/v1alpha1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "Rollout.kuberik") {
		t.Errorf("error %q names Rollout, which is served", err)
	}
}
