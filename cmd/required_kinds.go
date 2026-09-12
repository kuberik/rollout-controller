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
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// checkRequiredKinds verifies that every kind the controllers watch is served
// by the API server before the manager starts.
//
// controller-runtime does not fail when a watched CRD is missing: the source
// polls for its informer until the cache-sync timeout (two minutes by
// default) while /readyz keeps answering OK. A `helm upgrade --wait` that did
// not install a new CRD therefore reports success and the pod crashloops
// afterwards. Checking up front turns that into an immediate exit with the
// missing kinds named.
func checkRequiredKinds(mapper meta.RESTMapper, scheme *runtime.Scheme, objs ...client.Object) error {
	var missing []string
	for _, obj := range objs {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			return err
		}
		if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			if meta.IsNoMatchError(err) {
				missing = append(missing, gvk.Kind+"."+gvk.GroupVersion().String())
				continue
			}
			return fmt.Errorf("looking up %s: %w", gvk, err)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("required CRDs are not installed: %s", strings.Join(missing, ", "))
	}
	return nil
}
