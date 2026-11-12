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

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kuberik/rollout-controller/test/utils"
)

const (
	approvalNS = "e2e-approvals"

	byPolicyName     = "kuberik-rolloutapproval-by"
	gateGuardName    = "kuberik-approval-gate-guard"
	approverRoleName = "kuberik-rollout-approver"

	// The username the gate guard exempts: the service account that config/default produces.
	controllerUser = "system:serviceaccount:" + namespace + ":" + serviceAccountName

	policyLabel = "gate.kuberik.com/approval-policy-name"
)

// identity is a user to impersonate with kubectl --as / --as-group.
type identity struct {
	user   string
	groups []string
}

var (
	controllerID = identity{user: controllerUser}
	alice        = identity{user: "alice"}
	bob          = identity{user: "bob", groups: []string{"e2e-devs"}}
	carol        = identity{user: "carol"} // may update rolloutgates, may not approve
)

// kubectlAs runs kubectl as the given identity (nil is the cluster admin of the kind cluster).
func kubectlAs(id *identity, stdin string, args ...string) (string, error) {
	if id != nil {
		args = append([]string{"--as", id.user}, args...)
		for _, g := range id.groups {
			args = append(args, "--as-group", g)
		}
	}
	cmd := exec.Command("kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	return utils.Run(cmd)
}

func approvalYAML(name, rollout, version, by string) string {
	ann := ""
	if by != "" {
		ann = fmt.Sprintf("\n  annotations:\n    approval.kuberik.com/by: %q", by)
	}
	return fmt.Sprintf(`apiVersion: kuberik.com/v1alpha1
kind: RolloutApproval
metadata:
  name: %s
  namespace: %s%s
spec:
  rolloutRef:
    name: %s
  version: %s
`, name, approvalNS, ann, rollout, version)
}

// gateYAML renders a RolloutGate. A non-empty policyUID makes it owned by the ApprovalPolicy
// "policy-owner"; label adds the approval-policy-name label.
func gateYAML(name, policyUID string, label bool) string {
	meta := ""
	if label {
		meta += fmt.Sprintf("\n  labels:\n    %s: policy-owner", policyLabel)
	}
	if policyUID != "" {
		meta += fmt.Sprintf(`
  ownerReferences:
  - apiVersion: kuberik.com/v1alpha1
    kind: ApprovalPolicy
    name: policy-owner
    uid: %s
    controller: true`, policyUID)
	}
	return fmt.Sprintf(`apiVersion: kuberik.com/v1alpha1
kind: RolloutGate
metadata:
  name: %s
  namespace: %s%s
spec:
  rolloutRef:
    name: some-rollout
  allowedVersions: []
`, name, approvalNS, meta)
}

func policyYAML(name string) string {
	// The selector matches nothing, so a running reconciler creates no gates of its own for it.
	return fmt.Sprintf(`apiVersion: kuberik.com/v1alpha1
kind: ApprovalPolicy
metadata:
  name: %s
  namespace: %s
spec:
  rolloutSelector:
    matchLabels:
      e2e.kuberik.com/never-matches: "true"
`, name, approvalNS)
}

var _ = Describe("Approval admission", Ordered, func() {
	var policyUID string

	mustAdmin := func(stdin string, args ...string) string {
		out, err := kubectlAs(nil, stdin, args...)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return out
	}
	// expectDenied asserts that an identity's request was rejected by the named admission policy.
	expectDenied := func(policy string, id *identity, stdin string, args ...string) {
		out, err := kubectlAs(id, stdin, args...)
		ExpectWithOffset(1, err).To(HaveOccurred(), "expected admission to deny, got: %s", out)
		ExpectWithOffset(1, err.Error()).To(ContainSubstring("ValidatingAdmissionPolicy '"+policy+"'"),
			"denied, but not by %s", policy)
	}
	// expectForbidden asserts an RBAC rejection (not an admission one).
	expectForbidden := func(id *identity, stdin string, args ...string) {
		out, err := kubectlAs(id, stdin, args...)
		ExpectWithOffset(1, err).To(HaveOccurred(), "expected RBAC to forbid, got: %s", out)
		ExpectWithOffset(1, err.Error()).To(ContainSubstring("forbidden"))
		ExpectWithOffset(1, err.Error()).NotTo(ContainSubstring("ValidatingAdmissionPolicy"))
	}
	// apply creates objects as an identity, retrying while freshly created RBAC propagates.
	createEventually := func(id *identity, stdin string) {
		EventuallyWithOffset(1, func() error {
			_, err := kubectlAs(id, stdin, "create", "-f", "-")
			return err
		}, 30*time.Second, time.Second).Should(Succeed())
	}
	gateGone := func(name string) func() bool {
		return func() bool {
			out, _ := kubectlAs(nil, "", "get", "rolloutgate", name, "-n", approvalNS, "--ignore-not-found", "-o", "name")
			return strings.TrimSpace(out) == ""
		}
	}

	BeforeAll(func() {
		SetDefaultEventuallyTimeout(2 * time.Minute)
		SetDefaultEventuallyPollingInterval(time.Second)

		By("creating the manager and test namespaces")
		for _, ns := range []string{namespace, approvalNS} {
			mustAdmin(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", ns), "apply", "-f", "-")
		}

		By("installing CRDs, the Flux CRDs and deploying the controller-manager (config/default)")
		_, err := utils.Run(exec.Command("make", "install"))
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("kubectl", "apply", "-f", "vendir/kustomizations", "-f", "vendir/ocirepositories"))
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage)))
		Expect(err).NotTo(HaveOccurred())

		By("granting RBAC for the test identities")
		mustAdmin(fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: alice-approver
  namespace: %[1]s
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: %[2]s}
subjects: [{apiGroup: rbac.authorization.k8s.io, kind: User, name: alice}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: devs-edit
  namespace: %[1]s
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: edit}
subjects: [{apiGroup: rbac.authorization.k8s.io, kind: Group, name: e2e-devs}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gate-editor
  namespace: %[1]s
rules:
- apiGroups: [kuberik.com]
  resources: [rolloutgates]
  verbs: [get, list, create, update, patch, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: carol-gate-editor
  namespace: %[1]s
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: gate-editor}
subjects: [{apiGroup: rbac.authorization.k8s.io, kind: User, name: carol}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: sa-approver
  namespace: %[1]s
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: %[2]s}
subjects: [{kind: ServiceAccount, name: ci, namespace: %[1]s}]
`, approvalNS, approverRoleName), "apply", "-f", "-")

		By("creating an ApprovalPolicy that policy-owned gates point at")
		mustAdmin(policyYAML("policy-owner"), "apply", "-f", "-")
		policyUID = strings.TrimSpace(mustAdmin("", "get", "approvalpolicy", "policy-owner", "-n", approvalNS,
			"-o", "jsonpath={.metadata.uid}"))
		Expect(policyUID).NotTo(BeEmpty())
	})

	AfterAll(func() {
		By("cleaning up the approval admission test resources")
		for _, c := range [][]string{
			{"kubectl", "delete", "ns", approvalNS, "--ignore-not-found"},
			{"make", "undeploy", "ignore-not-found=true"},
			{"make", "uninstall", "ignore-not-found=true"},
			{"kubectl", "delete", "ns", namespace, "--ignore-not-found"},
		} {
			_, _ = utils.Run(exec.Command(c[0], c[1:]...))
		}
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			out, _ := kubectlAs(nil, "", "get", "rolloutgates,rolloutapprovals,approvalpolicies", "-n", approvalNS, "-o", "wide")
			_, _ = fmt.Fprintf(GinkgoWriter, "objects in %s:\n%s\n", approvalNS, out)
		}
	})

	Context("what config/default installs", func() {
		It("keeps the exact, unprefixed names and bindings that point at their policy", func() {
			for _, name := range []string{byPolicyName, gateGuardName} {
				mustAdmin("", "get", "validatingadmissionpolicy", name)
				got := strings.TrimSpace(mustAdmin("", "get", "validatingadmissionpolicybinding", name,
					"-o", "jsonpath={.spec.policyName}"))
				Expect(got).To(Equal(name))
			}
			mustAdmin("", "get", "clusterrole", approverRoleName)
		})

		It("compiles the CEL without type-checking warnings", func() {
			for _, name := range []string{byPolicyName, gateGuardName} {
				Eventually(func() string {
					return strings.TrimSpace(mustAdmin("", "get", "validatingadmissionpolicy", name,
						"-o", "jsonpath={.status.observedGeneration}"))
				}).ShouldNot(BeEmpty())
				warnings := strings.TrimSpace(mustAdmin("", "get", "validatingadmissionpolicy", name,
					"-o", "jsonpath={.status.typeChecking.expressionWarnings}"))
				Expect(warnings).To(BeEmpty(), "type checking warnings for %s", name)
			}
		})

		It("guards the service account the deployment actually runs as", func() {
			got := strings.TrimSpace(mustAdmin("", "get", "deployment", "-n", namespace, "-l", "control-plane=controller-manager",
				"-o", "jsonpath={.items[0].spec.template.spec.serviceAccountName}"))
			Expect(got).To(Equal(serviceAccountName))
			guard := mustAdmin("", "get", "validatingadmissionpolicy", gateGuardName, "-o", "yaml")
			Expect(guard).To(ContainSubstring("'" + controllerUser + "'"))
		})

		It("does not aggregate the approver role into edit or admin", func() {
			labels := mustAdmin("", "get", "clusterrole", approverRoleName, "-o", "jsonpath={.metadata.labels}")
			Expect(labels).NotTo(ContainSubstring("aggregate-to"))
		})
	})

	Context("approval.kuberik.com/by", func() {
		It("allows a user to set by to their own name", func() {
			createEventually(&alice, approvalYAML("by-own", "web", "v1.0.0", "alice"))
		})

		It("denies by set to somebody else", func() {
			expectDenied(byPolicyName, &alice, approvalYAML("by-other", "web", "v1.0.1", "bob"), "create", "-f", "-")
		})

		It("allows an approval with no by", func() {
			createEventually(&alice, approvalYAML("by-none", "web", "v1.0.2", ""))
		})

		It("denies changing, removing or adding by afterwards, and still allows other metadata changes", func() {
			By("changing it")
			expectDenied(byPolicyName, nil, "", "annotate", "rolloutapproval", "by-own", "-n", approvalNS,
				"--overwrite", "approval.kuberik.com/by=bob")
			By("removing it")
			expectDenied(byPolicyName, nil, "", "annotate", "rolloutapproval", "by-own", "-n", approvalNS,
				"approval.kuberik.com/by-")
			By("adding it to an approval that had none")
			expectDenied(byPolicyName, nil, "", "annotate", "rolloutapproval", "by-none", "-n", approvalNS,
				"approval.kuberik.com/by=alice")
			By("changing only a label")
			mustAdmin("", "label", "rolloutapproval", "by-own", "-n", approvalNS, "e2e=ok")
			Expect(mustAdmin("", "get", "rolloutapproval", "by-own", "-n", approvalNS,
				"-o", "jsonpath={.metadata.annotations.approval\\.kuberik\\.com/by}")).To(Equal("alice"))
		})

		It("does not let a service account set by for a user", func() {
			sa := &identity{user: "system:serviceaccount:" + approvalNS + ":ci"}
			expectDenied(byPolicyName, sa, approvalYAML("by-sa-forged", "web", "v1.0.3", "alice"), "create", "-f", "-")
			By("a service account may only record itself")
			createEventually(sa, approvalYAML("by-sa-own", "web", "v1.0.4", sa.user))
		})
	})

	Context("gates owned by an approval policy", func() {
		BeforeEach(func() {
			// The controller creates the gate. Each spec gets its own, so specs do not depend on order.
			DeferCleanup(func() {
				_, _ = kubectlAs(&controllerID, "", "delete", "rolloutgate", "owned", "-n", approvalNS, "--ignore-not-found")
			})
			createEventually(&controllerID, gateYAML("owned", policyUID, true))
		})

		It("denies a user changing allowedVersions", func() {
			expectDenied(gateGuardName, &carol, "", "patch", "rolloutgate", "owned", "-n", approvalNS,
				"--type=merge", "-p", `{"spec":{"allowedVersions":["v9.9.9"]}}`)
		})

		It("denies a user changing labels", func() {
			expectDenied(gateGuardName, &carol, "", "label", "rolloutgate", "owned", "-n", approvalNS, "--overwrite",
				policyLabel+"=other")
			expectDenied(gateGuardName, &carol, "", "label", "rolloutgate", "owned", "-n", approvalNS, policyLabel+"-")
		})

		It("denies a user changing or removing owner references", func() {
			expectDenied(gateGuardName, &carol, "", "patch", "rolloutgate", "owned", "-n", approvalNS,
				"--type=json", "-p", `[{"op":"remove","path":"/metadata/ownerReferences"}]`)
			expectDenied(gateGuardName, &carol, "", "patch", "rolloutgate", "owned", "-n", approvalNS,
				"--type=json", "-p", `[{"op":"replace","path":"/metadata/ownerReferences/0/name","value":"someone-else"}]`)
		})

		It("denies a user deleting it", func() {
			expectDenied(gateGuardName, &carol, "", "delete", "rolloutgate", "owned", "-n", approvalNS)
			mustAdmin("", "get", "rolloutgate", "owned", "-n", approvalNS)
		})

		It("still lets a user change an annotation", func() {
			_, err := kubectlAs(&carol, "", "annotate", "rolloutgate", "owned", "-n", approvalNS, "e2e.kuberik.com/note=hi")
			Expect(err).NotTo(HaveOccurred())
		})

		It("lets the controller service account change it", func() {
			_, err := kubectlAs(&controllerID, "", "patch", "rolloutgate", "owned", "-n", approvalNS,
				"--type=merge", "-p", `{"spec":{"allowedVersions":["v1.2.3"]}}`)
			Expect(err).NotTo(HaveOccurred())
		})

		It("denies a user creating a gate with the approval-policy label", func() {
			expectDenied(gateGuardName, &carol, gateYAML("forged-label", "", true), "create", "-f", "-")
		})

		It("denies a user creating a gate owned by an ApprovalPolicy", func() {
			expectDenied(gateGuardName, &carol, gateYAML("forged-owner", policyUID, false), "create", "-f", "-")
		})
	})

	Context("gates that no policy owns", func() {
		It("denies adopting an ownerless gate into a policy", func() {
			mustAdmin(gateYAML("legacy-adopt", "", false), "apply", "-f", "-")
			expectDenied(gateGuardName, &carol, "", "label", "rolloutgate", "legacy-adopt", "-n", approvalNS, policyLabel+"=policy-owner")
			expectDenied(gateGuardName, &carol, "", "patch", "rolloutgate", "legacy-adopt", "-n", approvalNS, "--type=merge", "-p",
				fmt.Sprintf(`{"metadata":{"ownerReferences":[{"apiVersion":"kuberik.com/v1alpha1","kind":"ApprovalPolicy","name":"policy-owner","uid":%q}]}}`, policyUID))
		})

		It("still lets a user patch and delete a legacy ownerless gate", func() {
			mustAdmin(gateYAML("legacy", "", false), "apply", "-f", "-")
			_, err := kubectlAs(&carol, "", "patch", "rolloutgate", "legacy", "-n", approvalNS,
				"--type=merge", "-p", `{"spec":{"allowedVersions":["v1.0.0"]}}`)
			Expect(err).NotTo(HaveOccurred())
			_, err = kubectlAs(&carol, "", "delete", "rolloutgate", "legacy", "-n", approvalNS)
			Expect(err).NotTo(HaveOccurred())
		})

		It("lets the garbage collector remove a policy's gates when the policy is deleted", func() {
			mustAdmin(policyYAML("policy-gc"), "apply", "-f", "-")
			uid := strings.TrimSpace(mustAdmin("", "get", "approvalpolicy", "policy-gc", "-n", approvalNS,
				"-o", "jsonpath={.metadata.uid}"))
			gate := strings.ReplaceAll(strings.ReplaceAll(gateYAML("gc-gate", uid, true), "policy-owner", "policy-gc"), "some-rollout", "gc")
			createEventually(&controllerID, gate)

			By("a user cannot delete it, which proves the guard is on")
			expectDenied(gateGuardName, &carol, "", "delete", "rolloutgate", "gc-gate", "-n", approvalNS)

			By("deleting the policy")
			mustAdmin("", "delete", "approvalpolicy", "policy-gc", "-n", approvalNS)
			Eventually(gateGone("gc-gate")).Should(BeTrue(), "the garbage collector must be able to delete the policy-owned gate")
		})
	})

	Context("the approver role", func() {
		It("lets a bound user create and delete approvals", func() {
			createEventually(&alice, approvalYAML("approver-1", "web", "v2.0.0", "alice"))
			_, err := kubectlAs(&alice, "", "delete", "rolloutapproval", "approver-1", "-n", approvalNS)
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not let that user touch gates, rollouts or approvals' specs", func() {
			mustAdmin(gateYAML("approver-gate", "", false), "apply", "-f", "-")
			for _, verb := range []string{"get", "list", "create", "update", "patch", "delete"} {
				out, _ := kubectlAs(&alice, "", "auth", "can-i", verb, "rolloutgates", "-n", approvalNS)
				Expect(strings.TrimSpace(out)).To(Equal("no"), "alice may %s rolloutgates", verb)
			}
			expectForbidden(&alice, "", "patch", "rolloutgate", "approver-gate", "-n", approvalNS,
				"--type=merge", "-p", `{"spec":{"allowedVersions":["v1"]}}`)
			expectForbidden(&alice, "", "delete", "rolloutgate", "approver-gate", "-n", approvalNS)
			for _, res := range []string{"rollouts", "rolloutapprovals"} {
				out, _ := kubectlAs(&alice, "", "auth", "can-i", "update", res, "-n", approvalNS)
				Expect(strings.TrimSpace(out)).To(Equal("no"), "alice may update %s", res)
			}
		})

		It("is not granted by edit: a user with only edit cannot create approvals", func() {
			expectForbidden(&bob, approvalYAML("bob-approval", "web", "v2.0.1", ""), "create", "-f", "-")
		})
	})
})
