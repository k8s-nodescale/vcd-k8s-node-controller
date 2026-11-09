package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
)

const testCABundle = `# environment roots
-----BEGIN CERTIFICATE-----
MIIBszCCAVmgAwIBAgIUTESTROOT
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIBszCCAVmgAwIBAgIUTESTROOTG2
-----END CERTIFICATE-----
`

func bootstrapReconciler(t *testing.T, objs ...runtime.Object) *WorkerPoolReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &WorkerPoolReconciler{
		Client:             fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build(),
		Scheme:             scheme,
		PodNamespace:       "nodescale-system",
		BootstrapConfigMap: "nodescale-bootstrap",
	}
}

func bootstrapPool(bootstrap infrav1.BootstrapConfig) *infrav1.K8sWorkerPool {
	return &infrav1.K8sWorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "cluster-a"},
		Spec:       infrav1.K8sWorkerPoolSpec{Bootstrap: bootstrap},
	}
}

func installationConfigMap(t *testing.T, body string) *corev1.ConfigMap {
	t.Helper()
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "nodescale-bootstrap", Namespace: "nodescale-system"},
		Data:       map[string]string{"bootstrap.yaml": body},
	}
}

func TestMergeContainerdIsPerField(t *testing.T) {
	base := &infrav1.ContainerdConfig{
		SandboxImage:       "registry.example.com/pause:3.9",
		Mirrors:            map[string][]string{"docker.io": {"http://mirror.example.com"}},
		InsecureSkipVerify: true,
	}
	override := &infrav1.ContainerdConfig{SandboxImage: "registry.example.com/pause:3.10"}
	got := mergeContainerd(base, override)
	if got.SandboxImage != "registry.example.com/pause:3.10" {
		t.Errorf("the pool's pause image did not win: %q", got.SandboxImage)
	}
	if len(got.Mirrors) != 1 || got.Mirrors["docker.io"][0] != "http://mirror.example.com" {
		t.Errorf("the installation mirrors were dropped: %#v", got.Mirrors)
	}
	if !got.InsecureSkipVerify {
		t.Error("the installation's insecure_skip_verify was dropped")
	}

	if got := mergeContainerd(base, nil); got == nil || got.SandboxImage != base.SandboxImage {
		t.Errorf("a pool without containerd config lost the defaults: %#v", got)
	}
	if got := mergeContainerd(nil, override); got == nil || got.SandboxImage != override.SandboxImage {
		t.Errorf("a pool without installation defaults lost its own value: %#v", got)
	}
	if got := mergeContainerd(nil, nil); got != nil {
		t.Errorf("no configuration at all must stay nil, got %#v", got)
	}
	// The base is not mutated: it is read again for the next pool.
	if base.SandboxImage != "registry.example.com/pause:3.9" {
		t.Error("mergeContainerd modified the installation defaults")
	}
}

func TestValidateGuestPolicy(t *testing.T) {
	cases := []struct {
		name string
		in   infrav1.BootstrapConfig
		want string
	}{
		{name: "empty", in: infrav1.BootstrapConfig{}},
		{
			name: "valid",
			in: infrav1.BootstrapConfig{
				NTPServers:    []string{"ntp1.example.com", "192.0.2.123"},
				Sysctls:       map[string]string{"net.core.rmem_max": "67108864", "vm.max_map_count": "262144"},
				KernelModules: []string{"nf_conntrack"},
				Containerd: &infrav1.ContainerdConfig{
					Mirrors: map[string][]string{"docker.io": {"http://mirror.example.com"}},
				},
			},
		},
		{
			name: "sysctl key with a space",
			in:   infrav1.BootstrapConfig{Sysctls: map[string]string{"net.core.rmem max": "1"}},
			want: "not a sysctl name",
		},
		{
			name: "sysctl key with a newline",
			in:   infrav1.BootstrapConfig{Sysctls: map[string]string{"net.core.rmem_max\nvm.swappiness": "1"}},
			want: "not a sysctl name",
		},
		{
			name: "sysctl empty value",
			in:   infrav1.BootstrapConfig{Sysctls: map[string]string{"net.core.rmem_max": "  "}},
			want: "one non-empty line",
		},
		{
			name: "sysctl value with a newline",
			in:   infrav1.BootstrapConfig{Sysctls: map[string]string{"net.core.rmem_max": "1\nvm.swappiness = 0"}},
			want: "one non-empty line",
		},
		{
			name: "ntp server with a space",
			in:   infrav1.BootstrapConfig{NTPServers: []string{"ntp1.example.com iburst"}},
			want: "spec.bootstrap.ntpServers",
		},
		{
			name: "empty ntp server",
			in:   infrav1.BootstrapConfig{NTPServers: []string{""}},
			want: "spec.bootstrap.ntpServers",
		},
		{
			name: "module with a newline",
			in:   infrav1.BootstrapConfig{KernelModules: []string{"nf_conntrack\nbr_netfilter"}},
			want: "spec.bootstrap.kernelModules",
		},
		{
			name: "mirror host with a quote",
			in: infrav1.BootstrapConfig{Containerd: &infrav1.ContainerdConfig{
				Mirrors: map[string][]string{`doc"ker.io`: {"http://mirror.example.com"}},
			}},
			want: "not a registry host",
		},
		{
			name: "mirror without endpoints",
			in: infrav1.BootstrapConfig{Containerd: &infrav1.ContainerdConfig{
				Mirrors: map[string][]string{"docker.io": {}},
			}},
			want: "at least one endpoint",
		},
		{
			name: "endpoint with a space",
			in: infrav1.BootstrapConfig{Containerd: &infrav1.ContainerdConfig{
				Mirrors: map[string][]string{"docker.io": {"http://mirror.example.com extra"}},
			}},
			want: "not an endpoint",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateGuestPolicy(tc.in)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("unexpected rejection: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("got %q, want a message containing %q", got, tc.want)
			}
		})
	}
}

// TestEffectiveBootstrapMergesInstallationDefaults is the precedence rule: a
// field the pool names is the pool's, everything else comes from the ConfigMap
// the chart renders.
func TestEffectiveBootstrapMergesInstallationDefaults(t *testing.T) {
	r := bootstrapReconciler(t, installationConfigMap(t, `
ntpServers: [ntp1.example.com, ntp2.example.com]
sysctls:
  net.netfilter.nf_conntrack_max: "2621440"
kernelModules: [nf_conntrack]
containerd:
  sandboxImage: registry.example.com/pause:3.9
  insecureSkipVerify: true
  mirrors:
    docker.io: ["http://mirror.example.com"]
`))
	pool := bootstrapPool(infrav1.BootstrapConfig{
		NTPServers: []string{"ntp3.example.com"},
		Containerd: &infrav1.ContainerdConfig{SandboxImage: "registry.example.com/pause:3.10"},
	})

	got, err := r.effectiveBootstrap(context.Background(), pool)
	if err != nil {
		t.Fatalf("effectiveBootstrap: %v", err)
	}
	if len(got.NTPServers) != 1 || got.NTPServers[0] != "ntp3.example.com" {
		t.Errorf("the pool's NTP servers did not win: %#v", got.NTPServers)
	}
	if len(got.Sysctls) != 1 || got.Sysctls[0] != "net.netfilter.nf_conntrack_max = 2621440" {
		t.Errorf("the installation sysctls were not applied: %#v", got.Sysctls)
	}
	if len(got.KernelModules) != 1 || got.KernelModules[0] != "nf_conntrack" {
		t.Errorf("the installation kernel modules were not applied: %#v", got.KernelModules)
	}
	if got.ContainerdSandboxImage != "registry.example.com/pause:3.10" {
		t.Errorf("the pool's pause image did not win: %q", got.ContainerdSandboxImage)
	}
	if len(got.ContainerdMirrors) != 1 || got.ContainerdMirrors[0].Host != "docker.io" ||
		got.ContainerdMirrors[0].Endpoints[0] != "http://mirror.example.com" {
		t.Errorf("the installation mirrors were not carried: %#v", got.ContainerdMirrors)
	}
	if !got.ContainerdInsecure {
		t.Error("the installation's insecure_skip_verify was dropped")
	}
}

// TestEffectiveBootstrapResolvesCARefs covers both scopes: a pool reference
// resolves in the pool's namespace, an installation default in the controller's.
func TestEffectiveBootstrapResolvesCARefs(t *testing.T) {
	poolCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-roots", Namespace: "cluster-a"},
		Data:       map[string]string{"ca.pem": testCABundle},
	}
	installCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "nodescale-ca", Namespace: "nodescale-system"},
		Data:       map[string]string{"ca.pem": testCABundle},
	}
	installDefaults := installationConfigMap(t, `
caBundleRef:
  name: nodescale-ca
  key: ca.pem
`)

	t.Run("pool reference", func(t *testing.T) {
		r := bootstrapReconciler(t, poolCM, installCM, installDefaults)
		got, err := r.effectiveBootstrap(context.Background(), bootstrapPool(infrav1.BootstrapConfig{
			CABundleRef: &infrav1.ConfigKeyRef{Name: "cluster-roots", Key: "ca.pem"},
		}))
		if err != nil {
			t.Fatalf("effectiveBootstrap: %v", err)
		}
		if len(got.CACerts) != 2 {
			t.Fatalf("got %d certificates, want 2", len(got.CACerts))
		}
	})

	t.Run("installation reference resolves in the controller namespace", func(t *testing.T) {
		r := bootstrapReconciler(t, poolCM, installCM, installDefaults)
		got, err := r.effectiveBootstrap(context.Background(), bootstrapPool(infrav1.BootstrapConfig{}))
		if err != nil {
			t.Fatalf("effectiveBootstrap: %v", err)
		}
		if len(got.CACerts) != 2 {
			t.Fatalf("got %d certificates, want 2", len(got.CACerts))
		}
	})

	t.Run("installation reference does not resolve in the pool namespace", func(t *testing.T) {
		// The same name exists only in nodescale-system; a pool namespace lookup
		// would fail and report the wrong object.
		r := bootstrapReconciler(t, installCM, installDefaults)
		if _, err := r.effectiveBootstrap(context.Background(), bootstrapPool(infrav1.BootstrapConfig{})); err != nil {
			t.Fatalf("effectiveBootstrap: %v", err)
		}
	})

	t.Run("secret reference", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-roots", Namespace: "cluster-a"},
			Data:       map[string][]byte{"ca.pem": []byte(testCABundle)},
		}
		r := bootstrapReconciler(t, secret)
		r.BootstrapConfigMap = ""
		got, err := r.effectiveBootstrap(context.Background(), bootstrapPool(infrav1.BootstrapConfig{
			CABundleRef: &infrav1.ConfigKeyRef{Kind: "Secret", Name: "cluster-roots", Key: "ca.pem"},
		}))
		if err != nil {
			t.Fatalf("effectiveBootstrap: %v", err)
		}
		if len(got.CACerts) != 2 {
			t.Fatalf("got %d certificates, want 2", len(got.CACerts))
		}
	})

	t.Run("missing key is reported with the reference", func(t *testing.T) {
		r := bootstrapReconciler(t, poolCM, installCM, installDefaults)
		_, err := r.effectiveBootstrap(context.Background(), bootstrapPool(infrav1.BootstrapConfig{
			CABundleRef: &infrav1.ConfigKeyRef{Name: "cluster-roots", Key: "nope.pem"},
		}))
		if err == nil || !strings.Contains(err.Error(), `no key "nope.pem"`) {
			t.Fatalf("got %v, want a missing-key error", err)
		}
	})

	t.Run("bundle without a certificate is refused", func(t *testing.T) {
		empty := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-roots", Namespace: "cluster-a"},
			Data:       map[string]string{"ca.pem": "not a certificate\n"},
		}
		r := bootstrapReconciler(t, empty)
		r.BootstrapConfigMap = ""
		_, err := r.effectiveBootstrap(context.Background(), bootstrapPool(infrav1.BootstrapConfig{
			CABundleRef: &infrav1.ConfigKeyRef{Name: "cluster-roots", Key: "ca.pem"},
		}))
		if err == nil || !strings.Contains(err.Error(), "holds no PEM certificate") {
			t.Fatalf("got %v, want a refused bundle", err)
		}
	})
}

func TestEffectiveBootstrapRequiresTheNamedConfigMap(t *testing.T) {
	r := bootstrapReconciler(t)
	_, err := r.effectiveBootstrap(context.Background(), bootstrapPool(infrav1.BootstrapConfig{}))
	if err == nil || !strings.Contains(err.Error(), "nodescale-bootstrap") {
		t.Fatalf("got %v, want an error naming the missing ConfigMap", err)
	}
}

func TestEffectiveBootstrapWithoutInstallationDefaults(t *testing.T) {
	r := bootstrapReconciler(t)
	r.BootstrapConfigMap = ""
	got, err := r.effectiveBootstrap(context.Background(), bootstrapPool(infrav1.BootstrapConfig{}))
	if err != nil {
		t.Fatalf("effectiveBootstrap: %v", err)
	}
	if got.ContainerdSandboxImage != "" || len(got.ContainerdMirrors) != 0 || got.ContainerdInsecure {
		t.Errorf("unexpected container config without defaults: %#v", got)
	}
	if len(got.CACerts) != 0 || len(got.NTPServers) != 0 || len(got.Sysctls) != 0 {
		t.Errorf("unexpected policy without defaults: %#v", got)
	}
}

// TestEffectiveBootstrapFullConfigRef: the escape hatch owns the whole file and
// no drop-in is generated, so the guest keeps one source of truth.
func TestEffectiveBootstrapFullConfigRef(t *testing.T) {
	full := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a-containerd", Namespace: "cluster-a"},
		Data:       map[string]string{"config.toml": "version = 2\n# site file\n"},
	}
	r := bootstrapReconciler(t, full)
	r.BootstrapConfigMap = ""
	got, err := r.effectiveBootstrap(context.Background(), bootstrapPool(infrav1.BootstrapConfig{
		Containerd: &infrav1.ContainerdConfig{
			ConfigRef: &infrav1.ConfigKeyRef{Name: "cluster-a-containerd", Key: "config.toml"},
		},
	}))
	if err != nil {
		t.Fatalf("effectiveBootstrap: %v", err)
	}
	if got.ContainerdFull == "" {
		t.Fatal("the full config was not read")
	}
	if got.ContainerdSandboxImage != "" || len(got.ContainerdMirrors) != 0 {
		t.Error("a full config still produced site values; the guest would load both")
	}
}
