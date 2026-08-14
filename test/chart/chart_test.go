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
	for _, doc := range documents(rendered) {
		for _, line := range strings.Split(doc, "\n") {
			if strings.TrimSpace(line) == "kind: "+kind {
				return doc
			}
		}
	}
	t.Fatalf("no %s in the rendered chart", kind)
	return ""
}

func hasKind(rendered, kind string) bool {
	for _, doc := range documents(rendered) {
		for _, line := range strings.Split(doc, "\n") {
			if strings.TrimSpace(line) == "kind: "+kind {
				return true
			}
		}
	}
	return false
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
	} {
		if !hasKind(rendered, kind) {
			t.Errorf("a default install renders no %s", kind)
		}
	}
}

// TestClusterRoleGrantsOnlyTheOwnedAPIGroup guards the defect that shipped
// once already: generated RBAC picked up a stale API group from a nested
// checkout and granted access to a group this operator does not own.
func TestClusterRoleGrantsOnlyTheOwnedAPIGroup(t *testing.T) {
	role := documentOfKind(t, render(t), "ClusterRole")

	if !strings.Contains(role, "amd.halo.glimmer.ai") {
		t.Errorf("the ClusterRole does not grant the API group the operator owns")
	}
	for _, forbidden := range []string{
		"- halo.glimmer.ai",
		`"*"`,
		"- '*'",
	} {
		if strings.Contains(role, forbidden) {
			t.Errorf("the ClusterRole contains %q, which grants more than this operator owns:\n%s",
				forbidden, role)
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
