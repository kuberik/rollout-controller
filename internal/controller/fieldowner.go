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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	// legacyFieldManager is the single manager earlier versions used for every rollout.
	legacyFieldManager = "rollout-controller"
	fieldManagerMaxLen = 128
	// writeConflictRequeueSecs is how long a refused deploy waits before trying again.
	writeConflictRequeueSecs = 60
)

var conflictManagerPattern = regexp.MustCompile(`conflict with "([^"]+)"`)

// rolloutFieldManager returns the server-side-apply field manager for one rollout, shared
// by its Kustomization and OCIRepository writes. Each rollout owns its own leaves, so one rollout's apply never drops another's. Field
// manager names are limited to 128 characters; a longer name is truncated and suffixed
// with a hash of the full name so it stays unique.
func rolloutFieldManager(namespace, name string) string {
	full := "rollout-controller/" + namespace + "/" + name
	if len(full) <= fieldManagerMaxLen {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	return full[:fieldManagerMaxLen-len(suffix)] + suffix
}

// onlyLegacyManagerConflict reports whether every conflict in err is with the shared
// manager earlier versions used. Those fields are ours, so the apply may take them over.
func onlyLegacyManagerConflict(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil {
		return false
	}
	n := 0
	for _, c := range status.Status().Details.Causes {
		m := conflictManagerPattern.FindStringSubmatch(c.Message)
		if m == nil || m[1] != legacyFieldManager {
			return false
		}
		n++
	}
	return n > 0
}

// deployWriteIssue is a deploy-blocking problem with an annotated Kustomization or OCIRepository. The
// deploy writes nothing and the Rollout reports Reason on its Ready condition.
type deployWriteIssue struct {
	Reason  string
	Message string
}

func (e *deployWriteIssue) Error() string { return e.Reason + ": " + e.Message }

// describeFieldConflict names the object, the contested path and the other manager, and says
// the same thing for every manager: kuberik does not take the field over, so whoever else sets
// it has to stop (kind is e.g. "Kustomization"; id is namespace/name).
func describeFieldConflict(kind, id string, paths []string, err error) string {
	path := paths[0]
	name := ""
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Details != nil {
		// Read the first cause: the error text says "conflicts with" when there are several.
		for _, c := range status.Status().Details.Causes {
			if c.Field != "" {
				path = strings.TrimPrefix(c.Field, ".")
			}
			if m := conflictManagerPattern.FindStringSubmatch(c.Message); m != nil {
				name = m[1]
			}
			if c.Field != "" || name != "" {
				break
			}
		}
	}
	if name == "" {
		if m := conflictManagerPattern.FindStringSubmatch(err.Error()); m != nil {
			name = m[1]
		}
	}
	manager, stopWho := "another field manager", "that manager"
	if name != "" {
		manager = fmt.Sprintf("%q", name)
		stopWho = manager
	}
	return fmt.Sprintf("%s %s: %s is also set by %s. kuberik does not take over fields another tool sets; stop %s from setting %s and the deploy continues (kuberik retries every minute).", kind, id, path, manager, stopWho, path)
}
