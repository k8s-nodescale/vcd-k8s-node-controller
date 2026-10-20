package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSetLabelsAddsEmptyValueLabel(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "wrk-03"}}
	cs := fake.NewSimpleClientset(node)
	c := NewClient(cs)
	labels := map[string]string{"node-role.kubernetes.io/worker": ""}

	if err := c.SetLabels(context.Background(), "wrk-03", labels); err != nil {
		t.Fatalf("SetLabels: %v", err)
	}
	got, err := cs.CoreV1().Nodes().Get(context.Background(), "wrk-03", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if value, ok := got.Labels["node-role.kubernetes.io/worker"]; !ok {
		t.Fatalf("worker role label missing, labels: %v", got.Labels)
	} else if value != "" {
		t.Fatalf("worker role label = %q, want empty", value)
	}

	// Re-applying must be a no-op rather than a failing or duplicate patch.
	if err := c.SetLabels(context.Background(), "wrk-03", labels); err != nil {
		t.Fatalf("second SetLabels: %v", err)
	}
}

func TestSetLabelsOverwritesChangedValue(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "wrk-01",
		Labels: map[string]string{"node-role.kubernetes.io/worker": "", "keep": "me"},
	}}
	cs := fake.NewSimpleClientset(node)

	if err := NewClient(cs).SetLabels(context.Background(), "wrk-01", map[string]string{"node-role.kubernetes.io/worker": "true"}); err != nil {
		t.Fatalf("SetLabels: %v", err)
	}
	got, err := cs.CoreV1().Nodes().Get(context.Background(), "wrk-01", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Labels["node-role.kubernetes.io/worker"] != "true" {
		t.Fatalf("label not updated: %v", got.Labels)
	}
	if got.Labels["keep"] != "me" {
		t.Fatalf("unrelated label was dropped: %v", got.Labels)
	}
}
