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

// Package hack tests hack/migrate-approval-gates.sh against envtest, using the kubectl
// that setup-envtest ships next to the API server binaries.
package hack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	rolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

const (
	nsShop  = "shop"
	nsOther = "other"
	nsClash = "clash"
)

type fixture struct {
	t          *testing.T
	c          client.Client
	kubectl    string
	kubeconfig string
	script     string
}

// run executes the script and returns combined output and the exit code.
func (f *fixture) run(args ...string) (string, int) {
	f.t.Helper()
	cmd := exec.Command("bash", append([]string{f.script}, args...)...)
	cmd.Env = append(os.Environ(), "KUBECTL="+f.kubectl, "KUBECONFIG="+f.kubeconfig)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			f.t.Fatalf("running script: %v", err)
		}
		code = ee.ExitCode()
	}
	return out.String(), code
}

func (f *fixture) approvals(ns string) map[string]rolloutv1alpha1.RolloutApproval {
	f.t.Helper()
	list := &rolloutv1alpha1.RolloutApprovalList{}
	if err := f.c.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		f.t.Fatal(err)
	}
	m := map[string]rolloutv1alpha1.RolloutApproval{}
	for _, a := range list.Items {
		m[a.Name] = a
	}
	return m
}

func names(m map[string]rolloutv1alpha1.RolloutApproval) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (f *fixture) gate(ns, name, rollout string, versions *[]string, mutate func(*rolloutv1alpha1.RolloutGate)) {
	f.t.Helper()
	g := &rolloutv1alpha1.RolloutGate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: rolloutv1alpha1.RolloutGateSpec{
			RolloutRef:      &corev1.LocalObjectReference{Name: rollout},
			AllowedVersions: versions,
		},
	}
	if mutate != nil {
		mutate(g)
	}
	if err := f.c.Create(context.Background(), g); err != nil {
		f.t.Fatal(err)
	}
}

type migrationEnv struct {
	*fixture
	longVersion string
	longName    string
	wantShop    []string
}

func setupMigrationEnv(t *testing.T) *migrationEnv {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run through `make test`")
	}
	kubectl := filepath.Join(assets, "kubectl")
	if _, err := os.Stat(kubectl); err != nil {
		t.Skipf("no kubectl in %s: %v", assets, err)
	}
	for _, tool := range []string{"jq", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required by the script: %v", tool, err)
		}
	}
	script, err := filepath.Abs("migrate-approval-gates.sh")
	if err != nil {
		t.Fatal(err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: assets,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	user, err := env.AddUser(envtest.User{Name: "admin", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := user.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, kc, 0o600); err != nil {
		t.Fatal(err)
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = rolloutv1alpha1.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	m := &migrationEnv{
		fixture: &fixture{t: t, c: c, kubectl: kubectl, kubeconfig: kubeconfig, script: script},
	}
	ctx := context.Background()
	for _, ns := range []string{nsShop, nsOther, nsClash} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}

	m.longVersion = "v" + strings.Repeat("a", 300)
	sum := sha256.Sum256([]byte(m.longVersion))
	m.longName = "checkout-v" + strings.Repeat("a", 44) + "-" + hex.EncodeToString(sum[:])[:8]
	if len(m.longName) > 63 {
		t.Fatalf("test bug: long name is %d chars", len(m.longName))
	}
	m.seedGates(t)
	m.wantShop = []string{"checkout-1-0-0", "checkout-v1-42-0", "checkout-v1-42-0-build-7", m.longName}
	sort.Strings(m.wantShop)
	return m
}

func (m *migrationEnv) seedGates(t *testing.T) {
	t.Helper()
	f := m.fixture
	ctx := context.Background()
	// Legacy gate in shop: the four CONTRACT naming examples, plus a duplicate tag.
	f.gate(nsShop, "manual-approval", "checkout",
		&[]string{"v1.42.0", "V1.42.0+Build_7", "1.0.0-", m.longVersion, "v1.42.0"}, nil)
	// Never listed: nil allowedVersions, schedule-owned, labelled, and a policy-owned gate.
	f.gate(nsShop, "no-opinion", "checkout", nil, nil)
	f.gate(nsShop, "schedule-owned", "checkout", &[]string{"v9.9.9"}, func(g *rolloutv1alpha1.RolloutGate) {
		g.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "kuberik.com/v1alpha1", Kind: "RolloutSchedule", Name: "s",
			UID: types.UID("11111111-1111-1111-1111-111111111111"),
		}}
	})
	f.gate(nsShop, "labelled", "checkout", &[]string{"v8.8.8"}, func(g *rolloutv1alpha1.RolloutGate) {
		g.Labels = map[string]string{"gate.kuberik.com/schedule-name": "s"}
	})
	f.gate(nsShop, "policy-owned", "checkout", &[]string{"v7.7.7"}, func(g *rolloutv1alpha1.RolloutGate) {
		g.Labels = map[string]string{"gate.kuberik.com/approval-policy-name": "p"}
	})
	// Legacy gate with an empty list: nothing was approved.
	f.gate(nsOther, "kill-switch", "billing", &[]string{}, nil)
	// Legacy gate whose names collide with an existing object that has a different spec.
	f.gate(nsClash, "manual-approval", "api", &[]string{"v1.0.0", "v2.0.0"}, nil)
	if err := f.c.Create(ctx, &rolloutv1alpha1.RolloutApproval{
		ObjectMeta: metav1.ObjectMeta{Name: clashName, Namespace: nsClash},
		Spec: rolloutv1alpha1.RolloutApprovalSpec{
			RolloutRef: corev1.LocalObjectReference{Name: "api"}, Version: "V1.0.0",
		},
	}); err != nil {
		t.Fatal(err)
	}
}

const clashName = "api-v1-0-0"

func TestMigrateApprovalGates(t *testing.T) {
	m := setupMigrationEnv(t)
	t.Run("dry run lists gates and changes nothing", m.dryRun)
	t.Run("empty allowedVersions creates nothing", m.emptyList)
	t.Run("emit-manifests writes YAML the API server accepts", m.emitManifests)
	t.Run("apply creates exactly the expected approvals", m.apply)
	t.Run("second run creates nothing", m.secondRun)
	t.Run("a name clash with a different spec exits non-zero after the rest", m.nameClash)
	t.Run("all namespaces by default", m.allNamespaces)
}

func (m *migrationEnv) dryRun(t *testing.T) {
	f := m.fixture
	out, code := f.run("-n", nsShop)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wants := []string{
		"legacy gate shop/manual-approval", "would create checkout-v1-42-0 ", "would create " + m.longName,
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	bads := []string{"schedule-owned", "labelled", "policy-owned", "no-opinion", "v9.9.9", "v8.8.8", "v7.7.7"}
	for _, bad := range bads {
		if strings.Contains(out, bad) {
			t.Errorf("output mentions %q:\n%s", bad, out)
		}
	}
	if got := names(f.approvals(nsShop)); len(got) != 0 {
		t.Errorf("dry run created %v", got)
	}
}

func (m *migrationEnv) emptyList(t *testing.T) {
	f := m.fixture
	out, code := f.run("-n", nsOther, "--apply")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "legacy gate other/kill-switch") {
		t.Errorf("gate not reported:\n%s", out)
	}
	if got := names(f.approvals(nsOther)); len(got) != 0 {
		t.Errorf("created %v", got)
	}
}

func (m *migrationEnv) emitManifests(t *testing.T) {
	f := m.fixture
	dir := filepath.Join(t.TempDir(), "out")
	out, code := f.run("-n", nsShop, "--emit-manifests", dir)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	entries, err := os.ReadDir(filepath.Join(dir, nsShop))
	if err != nil {
		t.Fatal(err)
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		files = append(files, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(files)
	if !equal(files, m.wantShop) {
		t.Fatalf("files %v, want %v", files, m.wantShop)
	}
	cmd := exec.Command(f.kubectl, "apply", "--dry-run=server", "-R", "-f", dir)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+f.kubeconfig)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply --dry-run=server: %v\n%s", err, b)
	}
	if got := names(f.approvals(nsShop)); len(got) != 0 {
		t.Errorf("emit created %v", got)
	}
}

func (m *migrationEnv) apply(t *testing.T) {
	f := m.fixture
	out, code := f.run("-n", nsShop, "--apply")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	got := f.approvals(nsShop)
	if !equal(names(got), m.wantShop) {
		t.Fatalf("approvals %v, want %v", names(got), m.wantShop)
	}
	wantVersion := map[string]string{
		"checkout-v1-42-0":         "v1.42.0",
		"checkout-v1-42-0-build-7": "V1.42.0+Build_7",
		"checkout-1-0-0":           "1.0.0-",
		m.longName:                 m.longVersion,
	}
	for n, v := range wantVersion {
		a := got[n]
		if a.Spec.Version != v || a.Spec.RolloutRef.Name != "checkout" {
			t.Errorf("%s: spec %+v", n, a.Spec)
		}
		if _, ok := a.Annotations["approval.kuberik.com/by"]; ok {
			t.Errorf("%s: by annotation must not be set", n)
		}
	}
}

func (m *migrationEnv) secondRun(t *testing.T) {
	f := m.fixture
	before := f.approvals(nsShop)
	out, code := f.run("-n", nsShop, "--apply")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "created 0 approval(s)") {
		t.Errorf("expected nothing created:\n%s", out)
	}
	after := f.approvals(nsShop)
	for n, a := range before {
		if after[n].ResourceVersion != a.ResourceVersion {
			t.Errorf("%s changed", n)
		}
	}
	if len(after) != len(before) {
		t.Errorf("count changed: %d -> %d", len(before), len(after))
	}
}

func (m *migrationEnv) nameClash(t *testing.T) {
	f := m.fixture
	out, code := f.run("-n", nsClash, "--apply")
	if code == 0 {
		t.Fatalf("expected non-zero exit:\n%s", out)
	}
	if !strings.Contains(out, "CONFLICT "+clashName) {
		t.Errorf("conflict not reported:\n%s", out)
	}
	got := f.approvals(nsClash)
	if got[clashName].Spec.Version != "V1.0.0" {
		t.Errorf("existing object was changed: %+v", got[clashName].Spec)
	}
	if _, ok := got["api-v2-0-0"]; !ok {
		t.Errorf("the non-clashing approval was not created: %v", names(got))
	}
}

func (m *migrationEnv) allNamespaces(t *testing.T) {
	f := m.fixture
	out, _ := f.run()
	if !strings.Contains(out, "legacy gate other/kill-switch") ||
		!strings.Contains(out, "legacy gate shop/manual-approval") {
		t.Errorf("expected gates from several namespaces:\n%s", out)
	}
}
