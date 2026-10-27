package mgmt

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := infrav1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func testClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&infrav1.K8sWorkerPool{}).
		Build()
}

func testPool(replicas int32) *infrav1.K8sWorkerPool {
	return &infrav1.K8sWorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: "kion-web-dev", Namespace: "default"},
		Spec: infrav1.K8sWorkerPoolSpec{
			Replicas:   replicas,
			NamePrefix: "kion-web-dev",
			Flavor:     infrav1.NodeFlavor{CPUs: 2, MemoryMB: 4096, OSDiskGB: 20},
			ScaleDown:  infrav1.ScaleDownPolicy{MinReplicas: 0, AllowLastWorker: true},
		},
	}
}

func testNode(name string, milli int64, memMB int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(milli, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(memMB<<20, resource.BinarySI),
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func testPod(node, name string, reqCPU, reqMem, limCPU, limMem int64) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "work"},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewMilliQuantity(reqCPU, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(reqMem<<20, resource.BinarySI),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewMilliQuantity(limCPU, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(limMem<<20, resource.BinarySI),
					},
				},
			}},
		},
	}
}

// The operator asked for three colours with these boundaries, and the page and
// the API must not disagree about them.
func TestLevelBoundaries(t *testing.T) {
	cases := []struct {
		pct   float64
		level string
	}{
		{0, LevelOK},
		{79.9, LevelOK},
		{80, LevelWarn},
		{89.9, LevelWarn},
		{90, LevelHigh},
		{100, LevelHigh},
	}
	for _, c := range cases {
		p := c.pct
		if got := Level(&p); got != c.level {
			t.Errorf("%v%%: level %q, want %q", c.pct, got, c.level)
		}
	}
	if got := Level(nil); got != "" {
		t.Errorf("unknown share has level %q, want none", got)
	}
}

// A node that has not reported its capacity is not a node at zero percent.
func TestNewShareWithoutCapacityIsUnknown(t *testing.T) {
	s := NewShare(500, 0)
	if s.Percent != nil {
		t.Fatalf("share %v, want unknown", *s.Percent)
	}
	if s.Level != "" {
		t.Fatalf("level %q, want none", s.Level)
	}
	if got := NewShare(500, 2000); got.Percent == nil || *got.Percent != 25 || got.Level != LevelOK {
		t.Fatalf("share %+v, want 25%% ok", got)
	}
}

// An init container runs alone, so a big one raises the pod's floor instead of
// adding to the sum of the containers that run together.
func TestAddPodTakesTheInitContainerFloor(t *testing.T) {
	p := testPod("node-a", "job", 100, 128, 200, 256)
	p.Spec.InitContainers = []corev1.Container{{
		Name: "init",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(512<<20, resource.BinarySI),
			},
		},
	}}
	used := map[string]*nodeUsage{}
	addPod(used, p)
	addPod(used, testPod("node-a", "web", 100, 256, 100, 256))

	u := used["node-a"]
	if u == nil {
		t.Fatal("no usage recorded")
	}
	if u.requests.Millicores != 600 {
		t.Errorf("requests %dm, want 600m", u.requests.Millicores)
	}
	if u.requests.MemoryMB != 768 {
		t.Errorf("requests %dMB, want 768MB", u.requests.MemoryMB)
	}
	if u.requests.Pods != 2 {
		t.Errorf("pods %d, want 2", u.requests.Pods)
	}
	if u.limits.Millicores != 300 {
		t.Errorf("limits %dm, want 300m", u.limits.Millicores)
	}
}

// A finished pod holds nothing, and a pod that is not scheduled holds nothing
// on any node.
func TestAddPodSkipsFinishedAndUnscheduled(t *testing.T) {
	done := testPod("node-a", "done", 100, 100, 100, 100)
	done.Status.Phase = corev1.PodSucceeded
	pending := testPod("", "pending", 100, 100, 100, 100)
	used := map[string]*nodeUsage{}
	addPod(used, done)
	addPod(used, pending)
	if len(used) != 0 {
		t.Fatalf("usage %+v, want nothing", used)
	}
}

const exporterPage = `# HELP node_cpu_seconds_total Seconds the CPUs spent in each mode.
# TYPE node_cpu_seconds_total counter
node_cpu_seconds_total{cpu="0",mode="idle"} 1000.5
node_cpu_seconds_total{cpu="0",mode="user"} 300
node_cpu_seconds_total{cpu="1",mode="idle"} 1000.5
node_memory_MemAvailable_bytes 1.073741824e+09
node_memory_MemTotal_bytes 4.294967296e+09
node_filesystem_avail_bytes{mountpoint="/"} 123`

func TestParseExporterKeepsThreeSeries(t *testing.T) {
	var s exporterSample
	if err := parseExporter(strings.NewReader(exporterPage), &s); err != nil {
		t.Fatal(err)
	}
	if s.cpus != 2 {
		t.Errorf("cpus %d, want 2", s.cpus)
	}
	if s.idle != 2001 {
		t.Errorf("idle %v, want 2001", s.idle)
	}
	if !s.haveMem || s.total == 0 {
		t.Fatalf("memory not read: %+v", s)
	}
	if got := s.avail / s.total * 100; got < 24.9 || got > 25.1 {
		t.Errorf("available %v%% of total, want 25%%", got)
	}
}

// CPU is a counter: the first scrape can only answer for memory, and the share
// appears once there is a second sample to subtract.
func TestSharesNeedsTwoSamplesForCPU(t *testing.T) {
	base := time.Unix(1000, 0)
	c := &Collector{Now: func() time.Time { return base }}
	first := exporterSample{at: base, idle: 100, cpus: 2, avail: 1 << 30, total: 4 << 30, haveMem: true}

	cpu, mem := c.shares("node-a", first)
	if cpu != nil {
		t.Errorf("first sample gave cpu %v, want unknown", *cpu)
	}
	if mem == nil || *mem < 74.9 || *mem > 75.1 {
		t.Errorf("memory %v, want 75%%", mem)
	}

	// Two CPUs, ten seconds, four idle seconds: 80% busy.
	second := exporterSample{at: base.Add(10 * time.Second), idle: 104, cpus: 2, avail: 1 << 30, total: 4 << 30, haveMem: true}
	cpu, _ = c.shares("node-a", second)
	if cpu == nil {
		t.Fatal("second sample gave no cpu share")
	}
	if *cpu < 79.9 || *cpu > 80.1 {
		t.Errorf("cpu %v, want 80%%", *cpu)
	}
}

// A counter that went backwards (a rebooted exporter) must not produce a
// negative or nonsense share.
func TestSharesIgnoresACounterReset(t *testing.T) {
	base := time.Unix(2000, 0)
	c := &Collector{Now: func() time.Time { return base }}
	c.shares("node-a", exporterSample{at: base, idle: 5000, cpus: 2})
	cpu, _ := c.shares("node-a", exporterSample{at: base.Add(10 * time.Second), idle: 10, cpus: 2})
	if cpu != nil {
		t.Errorf("cpu %v after a reset, want unknown", *cpu)
	}
}

func TestScrapeReadsAnEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(exporterPage))
	}))
	defer srv.Close()

	c := &Collector{Timeout: 2 * time.Second}
	s, err := c.scrape(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if s.cpus != 2 || !s.haveMem {
		t.Fatalf("scrape %+v", s)
	}
}

func TestScrapeReportsABadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := (&Collector{}).scrape(context.Background(), srv.URL); err == nil {
		t.Fatal("a 503 was accepted")
	}
}

// The table is the product: one row per slot, three column groups, each share
// against the node's allocatable capacity.
func TestCollectOnceFillsTheTable(t *testing.T) {
	pool := testPool(1)
	pool.Status.ObservedReplicas = 1
	pool.Status.Nodes = []infrav1.WorkerNodeStatus{{
		Name:     "KION-WEB-DEV-wrk-01",
		NodeName: "kion-web-dev-wrk-01",
		Phase:    infrav1.NodePhaseReady,
		IP:       "192.168.100.7",
	}}
	node := testNode("kion-web-dev-wrk-01", 2000, 4096)
	pod := testPod("kion-web-dev-wrk-01", "web", 500, 1024, 1000, 2048)

	c := &Collector{Client: testClient(t, pool, node, pod), ExporterLabels: DefaultExporterLabels}
	pools, err := c.CollectOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 {
		t.Fatalf("%d pools, want 1", len(pools))
	}
	p := pools[0]
	if p.Namespace != "default" || p.Name != "kion-web-dev" {
		t.Fatalf("pool %s/%s", p.Namespace, p.Name)
	}
	if p.Ready != 1 {
		t.Errorf("ready %d, want 1", p.Ready)
	}
	if len(p.Nodes) != 1 {
		t.Fatalf("%d rows, want 1", len(p.Nodes))
	}
	n := p.Nodes[0]
	if !n.Ready || n.IP != "192.168.100.7" {
		t.Errorf("row %+v", n)
	}
	if n.Requests.CPU.Percent == nil || *n.Requests.CPU.Percent != 25 {
		t.Errorf("requests cpu %+v, want 25%%", n.Requests.CPU)
	}
	if n.Requests.Memory.Percent == nil || *n.Requests.Memory.Percent != 25 {
		t.Errorf("requests memory %+v, want 25%%", n.Requests.Memory)
	}
	if n.Limits.CPU.Percent == nil || *n.Limits.CPU.Percent != 50 {
		t.Errorf("limits cpu %+v, want 50%%", n.Limits.CPU)
	}
	// No exporter and no metrics-server: the third group is a dash, not a zero.
	if n.Real.CPU.Percent != nil || n.Real.Memory.Percent != nil {
		t.Errorf("real %+v, want unknown", n.Real)
	}
	if n.RealSource != "" {
		t.Errorf("real source %q, want none", n.RealSource)
	}
}

func TestSnapshotKeepsTheLastGoodPass(t *testing.T) {
	c := &Collector{Client: testClient(t, testPool(0))}
	if _, err := c.CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Break the client: the snapshot must survive the failed pass.
	c.Client = fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return errors.New("apiserver is down")
			},
		}).
		Build()
	if _, err := c.CollectOnce(context.Background()); err == nil {
		t.Fatal("the broken client was accepted by a pass")
	}
	pools, at, err := c.Snapshot()
	if err == nil {
		t.Fatal("a broken client was accepted")
	}
	if len(pools) != 1 || at.IsZero() {
		t.Fatalf("snapshot lost: %d pools at %v", len(pools), at)
	}
}

func TestSetReplicasWritesTheSpec(t *testing.T) {
	fc := testClient(t, testPool(1))
	s := &Server{Client: fc}
	if err := s.SetReplicas(context.Background(), "default", "kion-web-dev", 4); err != nil {
		t.Fatal(err)
	}
	got := &infrav1.K8sWorkerPool{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "kion-web-dev"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Replicas != 4 {
		t.Fatalf("replicas %d, want 4", got.Spec.Replicas)
	}
	if err := s.SetReplicas(context.Background(), "default", "kion-web-dev", -1); err == nil {
		t.Fatal("a negative replica count was accepted")
	}
	if err := s.SetReplicas(context.Background(), "default", "missing", 1); err == nil {
		t.Fatal("a missing pool was accepted")
	}
}

// Only the fields the operator filled in are written, so a scale-up does not
// silently reset the disk size or the template.
func TestSetFlavorWritesOnlyWhatWasGiven(t *testing.T) {
	fc := testClient(t, testPool(1))
	s := &Server{Client: fc}
	if err := s.SetFlavor(context.Background(), "default", "kion-web-dev", Flavor{CPUs: 4}); err != nil {
		t.Fatal(err)
	}
	got := &infrav1.K8sWorkerPool{}
	_ = fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "kion-web-dev"}, got)
	if got.Spec.Flavor.CPUs != 4 {
		t.Errorf("cpus %d, want 4", got.Spec.Flavor.CPUs)
	}
	if got.Spec.Flavor.MemoryMB != 4096 || got.Spec.Flavor.OSDiskGB != 20 {
		t.Errorf("untouched fields changed: %+v", got.Spec.Flavor)
	}
	if err := s.SetFlavor(context.Background(), "default", "kion-web-dev", Flavor{}); err == nil {
		t.Fatal("an empty flavor was accepted")
	}
}

// A name already queued for removal is not queued twice: the entry drives one
// removal and one decrement of spec.replicas.
func TestDeleteWorkersDedupsAndAppends(t *testing.T) {
	pool := testPool(2)
	pool.Spec.ScaleDown.DeleteWorkers = []string{"KION-WEB-DEV-wrk-02"}
	fc := testClient(t, pool)
	s := &Server{Client: fc}
	if err := s.DeleteWorkers(context.Background(), "default", "kion-web-dev",
		[]string{"KION-WEB-DEV-wrk-02", "KION-WEB-DEV-wrk-01"}); err != nil {
		t.Fatal(err)
	}
	got := &infrav1.K8sWorkerPool{}
	_ = fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "kion-web-dev"}, got)
	want := []string{"KION-WEB-DEV-wrk-02", "KION-WEB-DEV-wrk-01"}
	if len(got.Spec.ScaleDown.DeleteWorkers) != len(want) {
		t.Fatalf("queued %v, want %v", got.Spec.ScaleDown.DeleteWorkers, want)
	}
	for i := range want {
		if got.Spec.ScaleDown.DeleteWorkers[i] != want[i] {
			t.Fatalf("queued %v, want %v", got.Spec.ScaleDown.DeleteWorkers, want)
		}
	}
	if err := s.DeleteWorkers(context.Background(), "default", "kion-web-dev", nil); err == nil {
		t.Fatal("an empty request was accepted")
	}
}

func testServer(t *testing.T) (*Server, client.Client) {
	t.Helper()
	pool := testPool(1)
	pool.Status.ObservedReplicas = 1
	pool.Status.Nodes = []infrav1.WorkerNodeStatus{{
		Name:     "KION-WEB-DEV-wrk-01",
		NodeName: "kion-web-dev-wrk-01",
		Phase:    infrav1.NodePhaseReady,
	}}
	fc := testClient(t, pool, testNode("kion-web-dev-wrk-01", 2000, 4096))
	c := &Collector{Client: fc}
	if _, err := c.CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &Server{Client: fc, Collector: c, PathPrefix: "/nodescale-ui"}, fc
}

// Everything the page asks for lives under the ingress path, so the ingress
// needs no rewrite rule.
func TestHandlersLiveUnderThePrefix(t *testing.T) {
	s, _ := testServer(t)
	h := s.Handler()

	cases := []struct {
		method, path string
		code         int
		body         string
	}{
		{http.MethodGet, "/nodescale-ui/healthz", http.StatusOK, "ok"},
		{http.MethodGet, "/nodescale-ui/", http.StatusOK, "/nodescale-ui"},
		{http.MethodGet, "/nodescale-ui/api/pools", http.StatusOK, "kion-web-dev"},
		{http.MethodGet, "/nodescale-ui/api/pools/default/kion-web-dev", http.StatusOK, "KION-WEB-DEV-wrk-01"},
		{http.MethodGet, "/nodescale-ui/api/pools/default/missing", http.StatusNotFound, "no such pool"},
		{http.MethodGet, "/api/pools", http.StatusNotFound, ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.code {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, rec.Code, c.code)
			continue
		}
		if c.body != "" && !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("%s %s: body %q does not contain %q", c.method, c.path, rec.Body.String(), c.body)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nodescale-ui", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/nodescale-ui/" {
		t.Errorf("bare prefix: %d %q, want 302 to /nodescale-ui/", rec.Code, rec.Header().Get("Location"))
	}
}

func TestPostReplicasAndDeleteWorkers(t *testing.T) {
	s, fc := testServer(t)
	h := s.Handler()

	post := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec
	}

	if rec := post("/nodescale-ui/api/pools/default/kion-web-dev/replicas", `{"replicas":3}`); rec.Code != http.StatusOK {
		t.Fatalf("replicas: %d %s", rec.Code, rec.Body.String())
	}
	got := &infrav1.K8sWorkerPool{}
	_ = fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "kion-web-dev"}, got)
	if got.Spec.Replicas != 3 {
		t.Errorf("replicas %d, want 3", got.Spec.Replicas)
	}

	if rec := post("/nodescale-ui/api/pools/default/kion-web-dev/delete-workers",
		`{"workers":["KION-WEB-DEV-wrk-01"]}`); rec.Code != http.StatusOK {
		t.Fatalf("delete-workers: %d %s", rec.Code, rec.Body.String())
	}
	_ = fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "kion-web-dev"}, got)
	if len(got.Spec.ScaleDown.DeleteWorkers) != 1 || got.Spec.ScaleDown.DeleteWorkers[0] != "KION-WEB-DEV-wrk-01" {
		t.Errorf("queued %v", got.Spec.ScaleDown.DeleteWorkers)
	}

	if rec := post("/nodescale-ui/api/pools/default/kion-web-dev/flavor", `{"cpus":4,"memoryMB":8192}`); rec.Code != http.StatusOK {
		t.Fatalf("flavor: %d %s", rec.Code, rec.Body.String())
	}
	_ = fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "kion-web-dev"}, got)
	if got.Spec.Flavor.CPUs != 4 || got.Spec.Flavor.MemoryMB != 8192 {
		t.Errorf("flavor %+v", got.Spec.Flavor)
	}

	// A body the server cannot read, and one it reads but refuses.
	if rec := post("/nodescale-ui/api/pools/default/kion-web-dev/replicas", `{"replicas":`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: %d, want 400", rec.Code)
	}
	if rec := post("/nodescale-ui/api/pools/default/kion-web-dev/replicas", `{"replicas":-2}`); rec.Code != http.StatusBadRequest {
		t.Errorf("negative replicas: %d, want 400", rec.Code)
	}
	if rec := post("/nodescale-ui/api/pools/default/missing/replicas", `{"replicas":1}`); rec.Code != http.StatusNotFound {
		t.Errorf("missing pool: %d, want 404", rec.Code)
	}
}

// The page is served with its prefix baked in, so the same binary works at /
// and under an ingress path.
func TestIndexUsesItsPrefix(t *testing.T) {
	s, _ := testServer(t)
	s.PathPrefix = "/"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `var BASE = "";`) {
		t.Fatalf("page does not carry the empty prefix: %s", rec.Body.String()[:200])
	}
}
