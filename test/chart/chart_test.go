// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// Package chart renders the Helm chart and asserts what comes out.
//
// The assertions are about the rendered manifests, not about the template
// source, so a refactor of the templates that preserves behaviour keeps them
// green and a change that quietly drops a security context or widens an RBAC
// grant does not.
package chart

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const chartPath = "deploy/helm/halo-operator"

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("read working directory: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repository root")
	return ""
}

// render runs helm template against the chart and returns the manifests.
func render(t *testing.T, args ...string) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed, skipping chart render test")
	}
	root := repoRoot(t)
	full := append([]string{"template", "halo", filepath.Join(root, chartPath), "--include-crds"}, args...)
	out, err := exec.Command(helm, full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// documents splits rendered output into individual YAML documents.
func documents(rendered string) []string {
	var docs []string
	for _, d := range strings.Split(rendered, "\n---") {
		if strings.TrimSpace(d) != "" {
			docs = append(docs, d)
		}
	}
	return docs
}

// documentOfKind returns the first rendered document declaring the given kind.
func documentOfKind(t *testing.T, rendered, kind string) string {
	t.Helper()
	docs := documentsOfKind(rendered, kind)
	if len(docs) == 0 {
		t.Fatalf("no %s in the rendered chart", kind)
	}
	return docs[0]
}

// documentsOfKind returns every rendered document declaring the given kind.
// Only a top-level, unindented kind line counts: a ClusterRoleBinding's
// roleRef also says "kind: ClusterRole", two spaces in.
func documentsOfKind(rendered, kind string) []string {
	var out []string
	for _, doc := range documents(rendered) {
		for _, line := range strings.Split(doc, "\n") {
			if line == "kind: "+kind {
				out = append(out, doc)
				break
			}
		}
	}
	return out
}

// documentContaining returns the rendered document of the given kind whose
// text contains the marker, for charts that render several of one kind.
func documentContaining(t *testing.T, rendered, kind, marker string) string {
	t.Helper()
	for _, doc := range documentsOfKind(rendered, kind) {
		if strings.Contains(doc, marker) {
			return doc
		}
	}
	t.Fatalf("no %s containing %q in the rendered chart", kind, marker)
	return ""
}

func hasKind(rendered, kind string) bool {
	return len(documentsOfKind(rendered, kind)) > 0
}

func TestDefaultInstallRendersEveryObject(t *testing.T) {
	rendered := render(t)
	for _, kind := range []string{
		"CustomResourceDefinition",
		"ServiceAccount",
		"ClusterRole",
		"ClusterRoleBinding",
		"Role",
		"RoleBinding",
		"Deployment",
		"DaemonSet",
		"ValidatingAdmissionPolicy",
		"ValidatingAdmissionPolicyBinding",
	} {
		if !hasKind(rendered, kind) {
			t.Errorf("a default install renders no %s", kind)
		}
	}
}

// TestClusterRolesGrantOnlyTheOwnedAPIGroup guards the defect that shipped
// once already: generated RBAC picked up a stale API group from a nested
// checkout and granted access to a group this operator does not own. Every
// ClusterRole in the chart is held to it, not just the first.
func TestClusterRolesGrantOnlyTheOwnedAPIGroup(t *testing.T) {
	roles := documentsOfKind(render(t), "ClusterRole")
	if len(roles) == 0 {
		t.Fatal("no ClusterRole in the rendered chart")
	}
	for _, role := range roles {
		if !strings.Contains(role, "amd.halo.glimmer.ai") {
			t.Errorf("a ClusterRole does not grant the API group the operator owns:\n%s", role)
		}
		for _, forbidden := range []string{
			"- halo.glimmer.ai",
			`"*"`,
			"- '*'",
		} {
			if strings.Contains(role, forbidden) {
				t.Errorf("a ClusterRole contains %q, which grants more than this operator owns:\n%s",
					forbidden, role)
			}
		}
	}
}

// TestChartCRDMatchesGeneratedCRD holds the chart's copy of the CRD to the one
// controller-gen produces. They are separate files, so nothing but this test
// stops the shipped chart installing a stale schema.
func TestChartCRDMatchesGeneratedCRD(t *testing.T) {
	root := repoRoot(t)
	const name = "amd.halo.glimmer.ai_halodeviceconfigs.yaml"

	generated, err := os.ReadFile(filepath.Join(root, "config/crd/bases", name))
	if err != nil {
		t.Fatalf("read the generated CRD: %v", err)
	}
	shipped, err := os.ReadFile(filepath.Join(root, chartPath, "crds", name))
	if err != nil {
		t.Fatalf("read the CRD shipped in the chart: %v", err)
	}
	if string(generated) != string(shipped) {
		t.Errorf("the CRD in %v/crds has drifted from config/crd/bases; "+
			"copy the generated one over it after running make manifests", chartPath)
	}
}

func TestLeaderElection(t *testing.T) {
	t.Run("enabled by default", func(t *testing.T) {
		rendered := render(t)
		if !strings.Contains(documentOfKind(t, rendered, "Deployment"), "--leader-elect") {
			t.Errorf("the manager is not started with --leader-elect")
		}
		if !hasKind(rendered, "Role") {
			t.Errorf("leader election is on but no namespaced Role was rendered for the lease")
		}
	})

	t.Run("disabling it removes both the flag and the lease permissions", func(t *testing.T) {
		rendered := render(t, "--set", "leaderElection.enabled=false")
		if strings.Contains(documentOfKind(t, rendered, "Deployment"), "--leader-elect") {
			t.Errorf("--leader-elect survives leaderElection.enabled=false")
		}
		if hasKind(rendered, "Role") {
			t.Errorf("the lease Role is still granted with leader election disabled")
		}
	})
}

func TestImageTagDefaultsToAppVersion(t *testing.T) {
	deployment := documentOfKind(t, render(t), "Deployment")
	if !strings.Contains(deployment, "ghcr.io/baldwinspc/halo-operator:0.1.0") {
		t.Errorf("the image tag does not default to the chart appVersion:\n%s", deployment)
	}

	pinned := documentOfKind(t, render(t, "--set", "image.tag=v9.9.9"), "Deployment")
	if !strings.Contains(pinned, "halo-operator:v9.9.9") {
		t.Errorf("image.tag does not override the appVersion default")
	}
}

func TestOptOuts(t *testing.T) {
	t.Run("rbac.create=false renders no cluster role", func(t *testing.T) {
		rendered := render(t, "--set", "rbac.create=false")
		if hasKind(rendered, "ClusterRole") || hasKind(rendered, "ClusterRoleBinding") {
			t.Errorf("RBAC is still rendered with rbac.create=false")
		}
		if !hasKind(rendered, "Deployment") {
			t.Errorf("opting out of RBAC also removed the Deployment")
		}
	})

	t.Run("serviceAccount.create=false uses the supplied name", func(t *testing.T) {
		rendered := render(t,
			"--set", "serviceAccount.create=false",
			"--set", "serviceAccount.name=byo-account")
		if hasKind(rendered, "ServiceAccount") {
			t.Errorf("a ServiceAccount is still created with serviceAccount.create=false")
		}
		if !strings.Contains(documentOfKind(t, rendered, "Deployment"), "serviceAccountName: byo-account") {
			t.Errorf("the Deployment does not use the supplied service account name")
		}
	})
}

// TestManagerRunsUnprivileged asserts the security posture reaches the pod
// rather than only existing in values.yaml. This operator reads the API server
// and nothing else, so any privilege here would be unexplained.
func TestManagerRunsUnprivileged(t *testing.T) {
	deployment := documentOfKind(t, render(t), "Deployment")
	for _, want := range []string{
		"runAsNonRoot: true",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"- ALL",
	} {
		if !strings.Contains(deployment, want) {
			t.Errorf("the rendered Deployment is missing %q:\n%s", want, deployment)
		}
	}
	for _, forbidden := range []string{"privileged: true", "hostPID: true", "hostNetwork: true"} {
		if strings.Contains(deployment, forbidden) {
			t.Errorf("the rendered Deployment contains %q", forbidden)
		}
	}
}

// TestAgentReadsTheHostWithoutPrivilege is the pod-level form of the read-only
// contract: every host path the agent touches is read-only, and it carries no
// privilege that could mutate a host even if it wanted to.
func TestAgentReadsTheHostWithoutPrivilege(t *testing.T) {
	agent := documentOfKind(t, render(t), "DaemonSet")

	for _, want := range []string{
		"runAsNonRoot: true",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"- ALL",
		"path: /proc/cmdline",
		"path: /proc/sys/kernel/osrelease",
		"type: File",
		"path: /sys",
		"readOnly: true",
	} {
		if !strings.Contains(agent, want) {
			t.Errorf("the rendered DaemonSet is missing %q:\n%s", want, agent)
		}
	}
	for _, forbidden := range []string{"privileged: true", "hostPID: true", "hostNetwork: true"} {
		if strings.Contains(agent, forbidden) {
			t.Errorf("the rendered DaemonSet contains %q", forbidden)
		}
	}
	// Every host mount must be read-only — a writable one would contradict
	// the operator's whole posture. Counted against the number of hostPath
	// volumes rather than a fixed number: a fixed floor stops meaning anything
	// the moment a mount is added, because the existing read-only ones already
	// satisfy it and a new writable one slips through.
	hostPaths := strings.Count(agent, "hostPath:")
	if hostPaths == 0 {
		t.Fatalf("the rendered DaemonSet has no hostPath volumes, so this test checks nothing:\n%s", agent)
	}
	if got := strings.Count(agent, "readOnly: true"); got < hostPaths {
		t.Errorf("%d hostPath volumes but only %d read-only mounts:\n%s", hostPaths, got, agent)
	}
}

// TestAgentTargetsAMDGPUNodesByDefault pins the default placement to the NFD
// label AMD's GPU Operator applies, the same label the controller's default
// selector targets — the agent should run exactly where observations are
// expected from.
func TestAgentTargetsAMDGPUNodesByDefault(t *testing.T) {
	agent := documentOfKind(t, render(t), "DaemonSet")
	if !strings.Contains(agent, "feature.node.kubernetes.io/amd-gpu") {
		t.Errorf("the DaemonSet does not target the AMD GPU node label:\n%s", agent)
	}
	if !strings.Contains(agent, "fieldPath: spec.nodeName") {
		t.Errorf("the DaemonSet does not pass the node name down to the agent:\n%s", agent)
	}
}

// TestAgentIdentityCanOnlyPublishObservations holds the agent's ClusterRole to
// the least privilege the design claims: create/patch on observations, get on
// nodes, and nothing else — no writes to Nodes, no reach into the config CRD.
func TestAgentIdentityCanOnlyPublishObservations(t *testing.T) {
	rendered := render(t)
	role := documentContaining(t, rendered, "ClusterRole", "halonodeobservations")
	// Two ClusterRoles mention halonodeobservations; pick the agent's by name.
	if !strings.Contains(role, "-agent") {
		role = ""
		for _, doc := range documentsOfKind(rendered, "ClusterRole") {
			if strings.Contains(doc, "name: halo-halo-operator-agent") {
				role = doc
			}
		}
		if role == "" {
			t.Fatal("no agent ClusterRole in the rendered chart")
		}
	}

	for _, want := range []string{"halonodeobservations", "create", "patch"} {
		if !strings.Contains(role, want) {
			t.Errorf("the agent ClusterRole is missing %q:\n%s", want, role)
		}
	}
	for _, forbidden := range []string{
		"halodeviceconfigs", // the agent has no business reading or writing configs
		"- update",
		"- delete",
		"- deletecollection",
	} {
		if strings.Contains(role, forbidden) {
			t.Errorf("the agent ClusterRole grants %q, which a read-only observer never needs:\n%s",
				forbidden, role)
		}
	}
	// nodes: get only — the agent reads its own Node for ownership and may
	// never list the fleet or write anything.
	if !strings.Contains(role, "nodes") {
		t.Errorf("the agent ClusterRole cannot read its own Node for ownership:\n%s", role)
	}
	for _, forbidden := range []string{"- list", "- watch"} {
		if strings.Contains(role, forbidden) {
			t.Errorf("the agent ClusterRole grants %q on nodes; get alone suffices:\n%s",
				forbidden, role)
		}
	}
}

// TestAgentOptOut proves agent.enabled=false removes the DaemonSet and the
// agent's identity but leaves the manager intact — the conditions then report
// Unknown, which is the honest state of an unobserved fleet.
func TestAgentOptOut(t *testing.T) {
	rendered := render(t, "--set", "agent.enabled=false")
	if hasKind(rendered, "DaemonSet") {
		t.Error("the DaemonSet is still rendered with agent.enabled=false")
	}
	for _, doc := range documentsOfKind(rendered, "ClusterRole") {
		if strings.Contains(doc, "name: halo-halo-operator-agent") {
			t.Error("the agent ClusterRole is still rendered with agent.enabled=false")
		}
	}
	if !hasKind(rendered, "Deployment") {
		t.Error("disabling the agent also removed the manager")
	}
	if hasKind(rendered, "ValidatingAdmissionPolicy") || hasKind(rendered, "ValidatingAdmissionPolicyBinding") {
		t.Error("the agent's admission policy is still rendered with agent.enabled=false")
	}
}

// The policy that pins each observation to its writer's node (#10) must
// constrain exactly the identity the agent runs as. A username that drifts from
// the agent's service account leaves the policy matching nobody, and it would
// render, install and report nothing wrong. So the expected username is read
// off the ClusterRoleBinding that grants the agent its write, not restated.
func TestAgentObservationNamesArePinned(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"default install", []string{"--namespace", "halo-system"}},
		{"a supplied service account", []string{"--namespace", "halo-system",
			"--set", "serviceAccount.create=false", "--set", "serviceAccount.name=byo-account"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := render(t, tc.args...)

			var grant *rbacv1.ClusterRoleBinding
			for _, doc := range documentsOfKind(rendered, "ClusterRoleBinding") {
				var b rbacv1.ClusterRoleBinding
				if err := yaml.Unmarshal([]byte(doc), &b); err != nil {
					t.Fatalf("decode ClusterRoleBinding: %v", err)
				}
				if strings.HasSuffix(b.Name, "-agent") {
					grant = &b
				}
			}
			if grant == nil || len(grant.Subjects) != 1 || grant.Subjects[0].Kind != "ServiceAccount" {
				t.Fatalf("no single-ServiceAccount agent ClusterRoleBinding in the rendered chart: %+v", grant)
			}
			wantUser := "system:serviceaccount:" + grant.Subjects[0].Namespace + ":" + grant.Subjects[0].Name

			var policy admissionregistrationv1.ValidatingAdmissionPolicy
			if err := yaml.Unmarshal([]byte(documentOfKind(t, rendered, "ValidatingAdmissionPolicy")), &policy); err != nil {
				t.Fatalf("decode ValidatingAdmissionPolicy: %v", err)
			}
			var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := yaml.Unmarshal([]byte(documentOfKind(t, rendered, "ValidatingAdmissionPolicyBinding")), &binding); err != nil {
				t.Fatalf("decode ValidatingAdmissionPolicyBinding: %v", err)
			}

			if len(policy.Spec.MatchConditions) != 1 ||
				!strings.Contains(policy.Spec.MatchConditions[0].Expression, `"`+wantUser+`"`) {
				t.Errorf("the policy does not match exactly the agent's identity %q: %+v", wantUser, policy.Spec.MatchConditions)
			}
			if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
				t.Error("the policy does not fail closed")
			}
			rules := policy.Spec.MatchConstraints.ResourceRules
			if len(rules) != 1 || strings.Join(rules[0].Resources, ",") != "halonodeobservations" ||
				strings.Join(rules[0].APIGroups, ",") != "amd.halo.glimmer.ai" {
				t.Errorf("the policy does not target halonodeobservations alone: %+v", rules)
			}
			var ops []string
			if len(rules) == 1 {
				for _, op := range rules[0].Operations {
					ops = append(ops, string(op))
				}
			}
			// UPDATE as well as CREATE: the agent writes with server-side apply,
			// so every write after the first is an update.
			if strings.Join(ops, ",") != "CREATE,UPDATE" {
				t.Errorf("the policy covers %v, want CREATE and UPDATE", ops)
			}
			if binding.Spec.PolicyName != policy.Name {
				t.Errorf("the binding binds %q, not the policy %q", binding.Spec.PolicyName, policy.Name)
			}
			if len(binding.Spec.ValidationActions) != 1 || binding.Spec.ValidationActions[0] != admissionregistrationv1.Deny {
				t.Errorf("the binding's actions are %v; a policy that only audits pins nothing", binding.Spec.ValidationActions)
			}
		})
	}

	t.Run("agent.pinObservationNames=false renders neither object", func(t *testing.T) {
		rendered := render(t, "--set", "agent.pinObservationNames=false")
		if hasKind(rendered, "ValidatingAdmissionPolicy") || hasKind(rendered, "ValidatingAdmissionPolicyBinding") {
			t.Error("the admission policy is still rendered with agent.pinObservationNames=false")
		}
		if !hasKind(rendered, "DaemonSet") {
			t.Error("opting out of the policy also removed the agent")
		}
	})
}

// TestManagerRoleReadsObservations extends the RBAC drift guard to the new
// resources: the manager must be able to read observations and nodes, and
// must not be able to write either.
func TestManagerRoleReadsObservations(t *testing.T) {
	rendered := render(t)
	var manager string
	for _, doc := range documentsOfKind(rendered, "ClusterRole") {
		if strings.Contains(doc, "halodeviceconfigs") {
			manager = doc
			break
		}
	}
	if manager == "" {
		t.Fatal("no manager ClusterRole in the rendered chart")
	}
	for _, want := range []string{"halonodeobservations", "nodes"} {
		if !strings.Contains(manager, want) {
			t.Errorf("the manager ClusterRole cannot read %s; conditions could never leave Unknown:\n%s",
				want, manager)
		}
	}
	for _, forbidden := range []string{"- create", "- delete"} {
		if strings.Contains(manager, forbidden) {
			t.Errorf("the manager ClusterRole grants %q; the manager only reads nodes and observations and writes config status:\n%s",
				forbidden, manager)
		}
	}
}

const testDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000abc"

// A released chart pulls by digest, so the version it names is the bytes it
// runs. Two-sided: with a digest BOTH workloads reference it, and without one
// neither carries an "@" — so a digest silently dropped by the helper, or one
// leaking into an unpinned install, both fail.
func TestImageIsPinnedByDigestWhenOneIsGiven(t *testing.T) {
	pinned := render(t, "--set", "image.digest="+testDigest)
	for _, kind := range []string{"Deployment", "DaemonSet"} {
		doc := documentOfKind(t, pinned, kind)
		want := "ghcr.io/baldwinspc/halo-operator:0.1.0@" + testDigest
		if !strings.Contains(doc, want) {
			t.Errorf("the %s does not pull by digest; want image %q:\n%s", kind, want, doc)
		}
	}

	unpinned := render(t)
	for _, kind := range []string{"Deployment", "DaemonSet"} {
		doc := documentOfKind(t, unpinned, kind)
		if strings.Contains(doc, "@sha256:") {
			t.Errorf("the %s carries a digest with none configured:\n%s", kind, doc)
		}
		if !strings.Contains(doc, "ghcr.io/baldwinspc/halo-operator:0.1.0") {
			t.Errorf("the %s does not reference repository:appVersion:\n%s", kind, doc)
		}
	}
}

// A malformed digest must fail the render, not ship. "@garbage" in a pod spec
// fails only at pull time, on the cluster, far from the release that produced it.
func TestMalformedDigestFailsTheRender(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed, skipping chart render test")
	}
	out, err := exec.Command(helm, "template", "halo", filepath.Join(repoRoot(t), chartPath),
		"--set", "image.digest=latest").CombinedOutput()
	if err == nil {
		t.Fatalf("a non-sha256 digest rendered without error:\n%s", out)
	}
	if !strings.Contains(string(out), "sha256") {
		t.Errorf("the failure does not say what a digest must look like: %s", out)
	}
}
