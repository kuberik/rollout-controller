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

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	kuberikcomv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

func TestCheckRequiredKinds(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{kuberikcomv1alpha1.GroupVersion, kustomizev1.GroupVersion})
	mapper.Add(kuberikcomv1alpha1.GroupVersion.WithKind("Rollout"), meta.RESTScopeNamespace)
	mapper.Add(kustomizev1.GroupVersion.WithKind("Kustomization"), meta.RESTScopeNamespace)

	if err := checkRequiredKinds(mapper, scheme, &kuberikcomv1alpha1.Rollout{}, &kustomizev1.Kustomization{}); err != nil {
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

func TestRequiredKindsNeedNoImageReflector(t *testing.T) {
	for _, o := range requiredKinds() {
		gvks, _, err := scheme.ObjectKinds(o)
		if err != nil {
			t.Fatal(err)
		}
		for _, gvk := range gvks {
			if gvk.Group == "image.toolkit.fluxcd.io" {
				t.Errorf("%s requires the Flux image-reflector API; the controller must start without it", gvk)
			}
			if gvk.Group == "kuberik.com" && (gvk.Kind == "ReleaseSource" || gvk.Kind == "ReleaseSlice") {
				t.Errorf("%s must stay optional at startup (lazy watches)", gvk)
			}
		}
	}
}
