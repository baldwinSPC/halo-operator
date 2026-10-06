// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package nodeagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// nodeNameClaim is the user-info extra key the API server fills from a bound
// service-account token's node claim.
const nodeNameClaim = "authentication.kubernetes.io/node-name"

// releaseNamespace is the namespace the chart is rendered into. Passed
// explicitly, never left to helm: helm otherwise takes the ambient one — the
// kube context's, or inside a pod its service account's — so on the ARC
// runner the chart rendered into arc-runners, a namespace this API server does
// not have, and the test failed applying the ServiceAccount before it reached
// the policy. The same test passed on a workstation, where the ambient
// namespace is default.
const releaseNamespace = "halo-pin-test"

// renderAgentAdmission renders the chart's agent RBAC and admission policy.
// The objects under test are the ones the chart ships, not a copy of them: a
// test holding its own policy would keep passing after the chart's drifted.
func renderAgentAdmission(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed, so the chart's admission policy cannot be rendered")
	}
	out, err := exec.Command(helm, "template", "halo", filepath.Join("..", "..", "deploy", "helm", "halo-operator"),
		"--namespace", releaseNamespace,
		"--show-only", "templates/agent-rbac.yaml",
		"--show-only", "templates/agent-admission-policy.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	var objs []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode the rendered chart: %v", err)
		}
		if len(u.Object) > 0 {
			objs = append(objs, u)
		}
	}
	return objs
}

// agentIdentity is the username the rendered chart binds the agent's write
// permission to — read from its ClusterRoleBinding, so the test impersonates
// exactly who the chart says the agent is.
func agentIdentity(t *testing.T, objs []*unstructured.Unstructured) (username, namespace string) {
	t.Helper()
	for _, u := range objs {
		if u.GetKind() != "ClusterRoleBinding" || !strings.HasSuffix(u.GetName(), "-agent") {
			continue
		}
		subjects, _, _ := unstructured.NestedSlice(u.Object, "subjects")
		if len(subjects) == 1 {
			s := subjects[0].(map[string]any)
			ns, name := s["namespace"].(string), s["name"].(string)
			return "system:serviceaccount:" + ns + ":" + name, ns
		}
	}
	t.Fatal("the rendered chart has no agent ClusterRoleBinding with one subject")
	return "", ""
}

// asAgent is a client that acts as the agent's service account, carrying the
// node claim a bound token on that node would carry — or none, when node is "".
func asAgent(t *testing.T, username, namespace, node string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(testCfg)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: username,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"},
	}
	if node != "" {
		cfg.Impersonate.Extra = map[string][]string{nodeNameClaim: {node}}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("build the impersonating client: %v", err)
	}
	return c
}

// The chart's admission policy pins each observation to the node that writes
// it (halo-operator#10), through the agent's real publish path: server-side
// apply, as the agent's own service account, against a real API server.
func TestTheChartPinsEachObservationToItsWritersNode(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	objs := renderAgentAdmission(t)
	// The release namespace the chart's ServiceAccount lives in. Not deleted
	// afterwards: envtest runs no namespace controller, so a deletion would
	// only leave it Terminating, and nothing else in this package uses it.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: releaseNamespace}}
	if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create the release namespace: %v", err)
	}
	for _, u := range objs {
		if err := k8sClient.Create(ctx, u); err != nil {
			t.Fatalf("apply the chart's %s %s: %v", u.GetKind(), u.GetName(), err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), u) })
	}
	username, namespace := agentIdentity(t, objs)
	createNode(t, "pin-a")
	createNode(t, "pin-b")

	reading := Reading{KernelCmdline: str("quiet")}
	onA := asAgent(t, username, namespace, "pin-a")

	// The policy is enforced once the API server's admission plugin has
	// observed it, which is not immediate. Wait for the refusal this test is
	// about; a policy that never refuses fails here, by name.
	var err error
	for deadline := time.Now().Add(30 * time.Second); ; {
		err = Publisher{Client: onA, NodeName: "pin-b"}.Publish(ctx, reading, metav1.Now())
		if err != nil && strings.Contains(err.Error(), "may write only its own HaloNodeObservation") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent on pin-a was never refused writing pin-b's observation; last result: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	t.Run("the agent writes its own node's observation", func(t *testing.T) {
		if err := (Publisher{Client: onA, NodeName: "pin-a"}).Publish(ctx, reading, metav1.Now()); err != nil {
			t.Fatalf("the agent on pin-a could not write pin-a's observation: %v", err)
		}
	})

	t.Run("a token with no node claim is refused, not waved through", func(t *testing.T) {
		unpinned := asAgent(t, username, namespace, "")
		err := Publisher{Client: unpinned, NodeName: "pin-a"}.Publish(ctx, reading, metav1.Now())
		if err == nil || !strings.Contains(err.Error(), "carries no node claim") {
			t.Fatalf("an agent write without a node claim was not refused for it: %v", err)
		}
	})

	t.Run("an administrator is not constrained", func(t *testing.T) {
		if err := (Publisher{Client: k8sClient, NodeName: "pin-b"}).Publish(ctx, reading, metav1.Now()); err != nil {
			t.Fatalf("the policy constrained a caller that is not the agent: %v", err)
		}
	})

	// pin-b's observation exists now, so this write is an UPDATE. The wait
	// above cannot say which operation it was refused on — the policy may have
	// become active before or after pin-b was first created — and a policy
	// that covered only CREATE would let an agent overwrite every observation
	// that already exists, which is all of them in a running fleet.
	t.Run("the agent cannot overwrite another node's existing observation", func(t *testing.T) {
		err := Publisher{Client: onA, NodeName: "pin-b"}.Publish(ctx, reading, metav1.Now())
		if err == nil || !strings.Contains(err.Error(), "may write only its own HaloNodeObservation") {
			t.Fatalf("the agent on pin-a overwrote pin-b's existing observation: %v", err)
		}
	})
}
