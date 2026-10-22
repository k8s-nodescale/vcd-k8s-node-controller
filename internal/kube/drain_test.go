package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testClient(objects ...runtime.Object) *fake.Clientset {
	return fake.NewSimpleClientset(objects...)
}

// evictWith deletes the evicted pod from the tracker, or returns err.
func evictWith(cs *fake.Clientset, events *[]string, err error) {
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		create := action.(k8stesting.CreateAction)
		eviction, ok := create.GetObject().(*policyv1.Eviction)
		if !ok {
			return false, nil, nil
		}
		if err != nil {
			return true, nil, err
		}
		*events = append(*events, eviction.Namespace+"/"+eviction.Name)
		_ = cs.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, eviction.Namespace, eviction.Name)
		return true, eviction, nil
	})
}

func pod(name, node, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

func TestCordonMarksNodeUnschedulable(t *testing.T) {
	cs := testClient(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "wrk-01"}})
	c := NewClient(cs)

	if err := c.Cordon(context.Background(), "wrk-01"); err != nil {
		t.Fatalf("cordon: %v", err)
	}
	node, err := cs.CoreV1().Nodes().Get(context.Background(), "wrk-01", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !node.Spec.Unschedulable {
		t.Fatal("node was not cordoned")
	}
}

func TestDrainEvictsWorkloadPodsAndSkipsDaemonSetAndMirror(t *testing.T) {
	old := drainPollEvery
	drainPollEvery = time.Millisecond
	defer func() { drainPollEvery = old }()

	workload := pod("web-0", "wrk-03", "default")
	daemon := pod("kube-proxy-x", "wrk-03", "kube-system")
	daemon.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "kube-proxy"}}
	mirror := pod("etcd-wrk-03", "wrk-03", "kube-system")
	mirror.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "true"}

	cs := testClient(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "wrk-03"}},
		workload, daemon, mirror,
	)
	var evicted []string
	evictWith(cs, &evicted, nil)

	if err := NewClient(cs).Drain(context.Background(), "wrk-03", 100*time.Millisecond); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != "default/web-0" {
		t.Fatalf("evicted = %v, want [default/web-0]", evicted)
	}
}

func TestDrainReportsBlockingPodDisruptionBudget(t *testing.T) {
	old := drainPollEvery
	drainPollEvery = time.Millisecond
	defer func() { drainPollEvery = old }()

	workload := pod("web-0", "wrk-03", "default")
	workload.Labels = map[string]string{"app": "web"}
	budget := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "web-pdb", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		},
		Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
	}
	cs := testClient(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "wrk-03"}}, workload, budget)
	var evicted []string
	evictWith(cs, &evicted, apierrors.NewTooManyRequests("cannot evict pod as it would violate the pod's disruption budget", 1))

	err := NewClient(cs).Drain(context.Background(), "wrk-03", 5*time.Millisecond)
	if !errors.Is(err, ErrDrainIncomplete) {
		t.Fatalf("err = %v, want ErrDrainIncomplete", err)
	}
	if !strings.Contains(err.Error(), "web-pdb") {
		t.Fatalf("error does not name the blocking budget: %v", err)
	}
	if len(evicted) != 0 {
		t.Fatalf("pod was deleted behind the budget: %v", evicted)
	}
}

func TestWaitVolumesDetachedTimesOutWithTheAttachmentNames(t *testing.T) {
	old := drainPollEvery
	drainPollEvery = time.Millisecond
	defer func() { drainPollEvery = old }()

	attachment := &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "csi-123"},
		Spec:       storagev1.VolumeAttachmentSpec{NodeName: "wrk-03"},
	}
	cs := testClient(attachment)
	c := NewClient(cs)

	err := c.WaitVolumesDetached(context.Background(), "wrk-03", 5*time.Millisecond)
	if !errors.Is(err, ErrVolumesAttached) || !strings.Contains(err.Error(), "csi-123") {
		t.Fatalf("err = %v, want ErrVolumesAttached naming csi-123", err)
	}

	if err := cs.StorageV1().VolumeAttachments().Delete(context.Background(), "csi-123", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitVolumesDetached(context.Background(), "wrk-03", time.Second); err != nil {
		t.Fatalf("detached node still reports attachments: %v", err)
	}
}

func TestNodeLocalVolumesFindsHostPathAndPinnedVolumes(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "wrk-03"}}
	workload := pod("web-0", "wrk-03", "default")
	workload.Spec.Volumes = []corev1.Volume{{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"},
		},
	}}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "default"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-hostpath"},
	}
	bound := corev1.VolumeBound
	hostPathPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-hostpath"},
		Spec:       corev1.PersistentVolumeSpec{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/data"}},
		Status:     corev1.PersistentVolumeStatus{Phase: bound},
	}
	pinnedPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-local"},
		Spec: corev1.PersistentVolumeSpec{
			NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"wrk-03"},
					}},
				}},
			}},
		},
		Status: corev1.PersistentVolumeStatus{Phase: bound},
	}
	otherPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-local-other"},
		Spec: corev1.PersistentVolumeSpec{
			NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"wrk-01"},
					}},
				}},
			}},
		},
		Status: corev1.PersistentVolumeStatus{Phase: bound},
	}
	sharedPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-csi"},
		Spec:       corev1.PersistentVolumeSpec{},
		Status:     corev1.PersistentVolumeStatus{Phase: bound},
	}

	cs := testClient(node, workload, claim, hostPathPV, pinnedPV, otherPV, sharedPV)
	volumes, err := NewClient(cs).NodeLocalVolumes(context.Background(), "wrk-03")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pv-hostpath", "pv-local"}
	if len(volumes) != len(want) {
		t.Fatalf("volumes = %v, want %v", volumes, want)
	}
	for i := range want {
		if volumes[i] != want[i] {
			t.Fatalf("volumes = %v, want %v", volumes, want)
		}
	}
}
