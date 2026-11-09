package cloudinit

import (
	"strings"
	"testing"
)

// TestContainerdDropInIsWrittenForTheInstalledLayout pins the contract with
// the guest: the base config comes from the installed binary, and the fragment
// carries this installation's values in the section names of *that* version.
// containerd 2.x renamed the CRI plugin, and a key in the wrong layout is
// ignored with a warning while SystemdCgroup silently stays false.
func TestContainerdDropInIsWrittenForTheInstalledLayout(t *testing.T) {
	p := testParams()
	p.ContainerdMirrors = []MirrorParam{
		{Host: "docker.io", Endpoints: []string{"http://mirror.example.com", "https://docker.io"}},
		{Host: "registry.k8s.io", Endpoints: []string{"http://mirror.example.com"}},
	}
	p.ContainerdInsecure = true
	ud, _ := mustRender(t, p)
	prereqs := userDataFiles(t, ud)["/usr/local/sbin/k8s-node-prereqs.sh"]

	for _, want := range []string{
		// The layout is detected from the base file, not assumed.
		"grep -qE '^[[:space:]]*version[[:space:]]*=[[:space:]]*3' /etc/containerd/config.toml",
		"grep -q 'io.containerd.cri.v1.images' /etc/containerd/config.toml",
		"runtime_plugin=io.containerd.cri.v1.runtime",
		"sandbox_section=\"[plugins.\\\"io.containerd.cri.v1.images\\\".pinned_images]\"",
		"sandbox_key=sandbox",
		// 1.x keys, and 2.x keys for the same values.
		"echo \"[plugins.\\\"${runtime_plugin}\\\".containerd.runtimes.runc.options]\"",
		`echo "[plugins.\"${images_plugin}\".registry.mirrors.\"docker.io\"]"`,
		`echo '  endpoint = ["http://mirror.example.com", "https://docker.io"]'`,
		// 2.x per-registry TLS lives in certs.d, not registry.configs.
		`[host."http://mirror.example.com"]`,
		"skip_verify = true",
		"/etc/containerd/certs.d/docker.io/hosts.toml",
		// One file, written once.
		"} > /etc/containerd/conf.d/10-nodescale.toml",
	} {
		if !strings.Contains(prereqs, want) {
			t.Errorf("the prerequisites script is missing %q", want)
		}
	}

	// The registries are sorted, so two renders of the same pool are identical.
	if strings.Index(prereqs, `"docker.io"`) > strings.Index(prereqs, `"registry.k8s.io"`) {
		t.Error("the registries are not written in sorted order")
	}
	again, _ := mustRender(t, p)
	if again != ud {
		t.Error("two renders of the same pool differ")
	}

	// A full config owns the file: no drop-in is generated.
	p.ContainerdFullConfig = "version = 2\n# site\n"
	full, _ := mustRender(t, p)
	if !strings.Contains(full, "path: /etc/containerd/config.toml") {
		t.Error("the full config is not written to /etc/containerd/config.toml")
	}
	if strings.Contains(full, "10-nodescale.toml") {
		t.Error("a full config and a drop-in were both generated; containerd would load both")
	}
}

func TestMirrorsIsSortedAndCopied(t *testing.T) {
	in := map[string][]string{
		"registry.k8s.io": {"http://mirror.example.com"},
		"docker.io":       {"http://mirror.example.com"},
		"empty.example":   {},
	}
	got := Mirrors(in)
	if len(got) != 2 || got[0].Host != "docker.io" || got[1].Host != "registry.k8s.io" {
		t.Fatalf("got %#v, want docker.io then registry.k8s.io and no empty entry", got)
	}
	got[0].Endpoints[0] = "https://changed"
	if in["docker.io"][0] != "http://mirror.example.com" {
		t.Error("Mirrors aliased the caller's slice")
	}
}

func TestCACertBlocksSplitsBundle(t *testing.T) {
	bundle := "text before\n" +
		"-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n" +
		"  \n" +
		"-----BEGIN CERTIFICATE-----\nBBB\n-----END CERTIFICATE-----\n" +
		"trailing\n"
	got := CACertBlocks(bundle)
	if len(got) != 2 {
		t.Fatalf("got %d certificates, want 2: %#v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "-----BEGIN CERTIFICATE-----") || !strings.HasSuffix(got[0], "-----END CERTIFICATE-----") {
		t.Errorf("the first block is not a certificate: %q", got[0])
	}
	if strings.Contains(got[0], "text before") || strings.Contains(got[1], "trailing") {
		t.Error("the block includes text outside the PEM markers")
	}
	if len(CACertBlocks("no pem here")) != 0 {
		t.Error("a bundle without certificates must yield nothing, not a partial block")
	}
	if len(CACertBlocks("-----BEGIN CERTIFICATE-----\nAAA\n")) != 0 {
		t.Error("an unterminated block must be dropped")
	}
}

func TestSysctlLinesAreSorted(t *testing.T) {
	got := SysctlLines(map[string]string{
		"net.netfilter.nf_conntrack_max": "2621440",
		"net.core.rmem_max":              "67108864",
	})
	want := []string{"net.core.rmem_max = 67108864", "net.netfilter.nf_conntrack_max = 2621440"}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}
	if lines := SysctlLines(nil); len(lines) != 0 {
		t.Errorf("an empty map must render no lines, got %#v", lines)
	}
}

// TestPrereqsGeneratesContainerdBase covers the guest side of the drop-in
// contract: the base config comes from the installed binary, imports are
// enabled once, and the effective configuration is verified after a restart.
func TestPrereqsGeneratesContainerdBase(t *testing.T) {
	ud, _ := mustRender(t, testParams())
	prereqs := userDataFiles(t, ud)["/usr/local/sbin/k8s-node-prereqs.sh"]
	for _, want := range []string{
		"containerd config default > /etc/containerd/config.toml",
		`imports = ["/etc/containerd/conf.d/*.toml"]`,
		"mkdir -p /etc/containerd/conf.d",
		"containerd config dump",
		"SystemdCgroup",
		"modprobe nf_conntrack",
	} {
		if !strings.Contains(prereqs, want) {
			t.Errorf("the prerequisites script is missing %q", want)
		}
	}
	// The image-provided config must not be overwritten: the file is generated
	// only when the image has none.
	if !strings.Contains(prereqs, "if [[ ! -f /etc/containerd/config.toml ]]; then") {
		t.Error("the base file is generated unconditionally; an image-supplied config.toml would be destroyed")
	}
	if !strings.Contains(prereqs, "grep -q '/etc/containerd/conf.d/\\*.toml' /etc/containerd/config.toml") {
		t.Error("the imports line is patched without checking whether it is already there; a re-run would duplicate it")
	}
}
