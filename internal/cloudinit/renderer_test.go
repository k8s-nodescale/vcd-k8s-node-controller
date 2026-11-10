package cloudinit

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func testParams() Params {
	return Params{
		NodeName:       "pool-wrk-01",
		NodeIP:         "192.0.2.10",
		CIDRPrefix:     "24",
		GatewayIP:      "192.0.2.1",
		NICName:        "ens192",
		DNSServersJSON: `["192.0.2.53"]`,
		DNSServersLine: "192.0.2.53",
		AptMirror:      "https://mirror.example.com/ubuntu-noble",
		OSUser:         "ubuntu",
		K8sVersion:     "1.35.9-1.1",
		K8sMinorRepo:   "v1.35",
		JoinEndpoint:   "192.0.2.2:6443",
		JoinCommand:    "kubeadm join 192.0.2.2:6443 --token x --discovery-token-ca-cert-hash sha256:y",
		HostsLines: []string{
			"192.0.2.2 master-01",
			"192.0.2.10 pool-wrk-01",
			"192.0.2.11 pool-wrk-02",
		},
		Disks: []DiskParam{
			{
				Device: "/dev/sdb", VG: "data1_vg", LV: "data1_lv",
				TempMount: "/data1", Mount: "/var", FSType: "ext4", SizeGB: 10,
				RsyncExcludes: []string{"/lib"},
			},
			{
				Device: "/dev/sdc", VG: "data2_vg", LV: "data2_lv",
				TempMount: "/data2", Mount: "/var/lib", FSType: "ext4", SizeGB: 30,
			},
		},
		NTPServers:             []string{"ntp1.example.com", "ntp2.example.com"},
		CACerts:                []string{"-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIUTESTCERT\n-----END CERTIFICATE-----"},
		Sysctls:                []string{"net.core.rmem_max = 67108864", "net.netfilter.nf_conntrack_max = 2621440"},
		KernelModules:          []string{"nf_conntrack"},
		ContainerdSandboxImage: "registry.example.com/pause:3.9",
		ContainerdMirrors: []MirrorParam{
			{Host: "docker.io", Endpoints: []string{"http://mirror.example.com"}},
		},
	}
}

func mustRender(t *testing.T, p Params) (string, string) {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ud, is, err := r.Render(p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return ud, is
}

// userDataFiles returns the content of every write_files entry, keyed by path.
func userDataFiles(t *testing.T, userData string) map[string]string {
	t.Helper()
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(userData), &doc); err != nil {
		t.Fatalf("rendered user-data is not valid YAML: %v", err)
	}
	out := map[string]string{}
	files, _ := doc["write_files"].([]interface{})
	for _, item := range files {
		f, _ := item.(map[string]interface{})
		path, _ := f["path"].(string)
		content, _ := f["content"].(string)
		out[path] = content
	}
	return out
}

func TestRenderIsDeterministic(t *testing.T) {
	ud1, is1 := mustRender(t, testParams())
	ud2, is2 := mustRender(t, testParams())
	if ud1 != ud2 || is1 != is2 {
		t.Fatal("Render is not deterministic for identical Params")
	}
}

func TestRenderCarriesStamp(t *testing.T) {
	ud, is := mustRender(t, testParams())
	stamp := regexp.MustCompile(`render=[0-9.]+ hash=[0-9a-f]{12}`)
	for name, content := range map[string]string{"user-data": ud, "initscript": is} {
		if !strings.Contains(content, "render="+RenderVersion) {
			t.Errorf("%s does not carry render=%s", name, RenderVersion)
		}
		if !stamp.MatchString(content) {
			t.Errorf("%s does not carry a template hash", name)
		}
	}
}

// TestInitscriptIsStaged is the core of the phase contract: pre-customization
// only prepares the network and the seed, post-customization does the disk move
// and the instance epoch, and neither phase can abort vCD's customizer.
func TestInitscriptIsStaged(t *testing.T) {
	_, is := mustRender(t, testParams())

	if !strings.Contains(is, "PHASE=\"${1:-postCustomization}\"") {
		t.Error("initscript does not read the phase from $1")
	}
	preIdx := strings.Index(is, "preCustomization|precustomization)")
	if preIdx < 0 {
		t.Fatal("initscript has no precustomization branch")
	}
	pre := is[preIdx:]
	pre = pre[:strings.Index(pre, ";;")]
	postIdx := strings.Index(is, "postCustomization|postcustomization)")
	if postIdx < 0 {
		t.Fatal("initscript has no postcustomization branch")
	}
	post := is[postIdx:]
	post = post[:strings.Index(post, ";;")]

	if !strings.Contains(pre, "stage_net") || !strings.Contains(pre, "stage_seed") {
		t.Error("precustomization must run the net and seed stages")
	}
	if strings.Contains(pre, "stage_disks") {
		t.Error("precustomization must not touch the data disks")
	}
	for _, want := range []string{"stage_seed", "stage_disks", "stage_epoch"} {
		if !strings.Contains(post, want) {
			t.Errorf("postcustomization is missing %s", want)
		}
	}

	// R1: the guest never reboots; vCD owns the reboot.
	if regexp.MustCompile(`(?m)^\s*(/sbin/)?reboot\b`).MatchString(is) {
		t.Error("initscript must not reboot the guest")
	}
	// R2: every stage failure is swallowed, the phase itself always exits 0.
	if !strings.HasSuffix(strings.TrimSpace(is), "exit 0") {
		t.Error("initscript must end with exit 0")
	}
	if regexp.MustCompile(`(?m)^exit [1-9]`).MatchString(is) {
		t.Error("initscript must never exit non-zero inside a vCD phase")
	}
	if !strings.Contains(is, "STAGE_DIR=/etc/vcd-stage") {
		t.Error("initscript does not record stage state")
	}
	if !strings.Contains(is, "deliberately no -e") {
		t.Error("initscript must document that it does not use set -e")
	}
}

func TestInstanceIDCarriesSpecHash(t *testing.T) {
	_, is := mustRender(t, testParams())
	if !strings.Contains(is, "instance-id: $NODE_NAME-$SPEC_HASH") {
		t.Fatal("meta-data instance-id is not content-derived")
	}
	p1 := testParams()
	p2 := testParams()
	p2.JoinCommand = "kubeadm join 192.0.2.2:6443 --token OTHER --discovery-token-ca-cert-hash sha256:z"
	p3 := testParams()
	p3.Disks[1].SizeGB = 40

	if specHash8(p1) != specHash8(p2) {
		t.Error("the join token must not change the spec hash")
	}
	if specHash8(p1) == specHash8(p3) {
		t.Error("a different disk layout must change the spec hash")
	}
}

// TestUserDataSplitsPrereqsAndJoin checks the staged split: two units, ordered,
// with the layout gate in front of the join and separate markers.
func TestUserDataSplitsPrereqsAndJoin(t *testing.T) {
	ud, _ := mustRender(t, testParams())
	files := userDataFiles(t, ud)

	prereqs, ok := files["/etc/systemd/system/k8s-node-prereqs.service"]
	if !ok {
		t.Fatal("k8s-node-prereqs.service is missing")
	}
	join, ok := files["/etc/systemd/system/k8s-node-join.service"]
	if !ok {
		t.Fatal("k8s-node-join.service is missing")
	}
	if !strings.Contains(join, "After=network-online.target k8s-node-prereqs.service") {
		t.Error("join unit is not ordered after the prerequisites")
	}
	if !strings.Contains(join, "ExecStartPre=/usr/local/sbin/vcd-verify-layout.sh") {
		t.Error("join unit has no layout gate (R7)")
	}
	// The prereqs unit runs apt, which lives on /var/lib: it needs the same
	// gate, and both units need the repair step ordered before them.
	if !strings.Contains(prereqs, "ExecStartPre=/usr/local/sbin/vcd-verify-layout.sh") {
		t.Error("prereqs unit has no layout gate (R7)")
	}
	layoutUnit, ok := files["/etc/systemd/system/vcd-ensure-layout.service"]
	if !ok {
		t.Fatal("vcd-ensure-layout.service is missing")
	}
	if !strings.Contains(layoutUnit, "Before=k8s-node-prereqs.service k8s-node-join.service") {
		t.Error("the layout repair unit is not ordered before the stage units")
	}
	if !strings.Contains(layoutUnit, "ConditionPathExists=/etc/vcd-data-disks.done") {
		t.Error("the layout repair unit must only run when disks were prepared")
	}
	if !strings.Contains(join, "vcd-ensure-layout.service") || !strings.Contains(prereqs, "vcd-ensure-layout.service") {
		t.Error("the stage units do not depend on the layout repair unit")
	}
	if !strings.Contains(prereqs, "ConditionPathExists=!/etc/kubernetes/prereqs.done") {
		t.Error("prereqs unit is not marker-guarded")
	}
	if !strings.Contains(join, "ConditionPathExists=!/etc/kubernetes/kubeadm-joined") {
		t.Error("join unit is not marker-guarded")
	}
	if !strings.Contains(join, "RestartSec=60") || !strings.Contains(prereqs, "RestartSec=60") {
		t.Error("a unit does not retry")
	}
	// The old single unit must be gone: two readiness/prep mechanisms at once is
	// exactly the double-owner problem this refactor removes.
	if _, ok := files["/etc/systemd/system/k8s-node-bootstrap.service"]; ok {
		t.Error("the old k8s-node-bootstrap.service is still rendered")
	}
	if !strings.Contains(files["/usr/local/sbin/k8s-node-join.sh"], "confirmed") {
		// the join script must wait for the API rather than fail at once
		if !strings.Contains(files["/usr/local/sbin/k8s-node-join.sh"], "healthz") {
			t.Error("join script does not wait for the API")
		}
	}
	if strings.Contains(files["/usr/local/sbin/k8s-node-prereqs.sh"], "kubeadm join") {
		t.Error("prereqs script must not join the cluster")
	}
}

func TestLayoutGateChecksEveryDisk(t *testing.T) {
	ud, _ := mustRender(t, testParams())
	gate := userDataFiles(t, ud)["/usr/local/sbin/vcd-verify-layout.sh"]
	for _, want := range []string{
		`check "/var" "/dev/data1_vg/data1_lv" 10`,
		`check "/var/lib" "/dev/data2_vg/data2_lv" 30`,
		"findmnt",
		"refusing to join",
	} {
		if !strings.Contains(gate, want) {
			t.Errorf("layout gate does not contain %q", want)
		}
	}
	if strings.Contains(gate, " /var/lib/") {
		t.Error("the gate must compare the mount point, not its children")
	}

	ensure := userDataFiles(t, ud)["/usr/local/sbin/vcd-ensure-layout.sh"]
	for _, want := range []string{
		`ensure "/var" "/dev/data1_vg/data1_lv"`,
		`ensure "/var/lib" "/dev/data2_vg/data2_lv"`,
	} {
		if !strings.Contains(ensure, want) {
			t.Errorf("layout repair does not contain %q", want)
		}
	}

	noDisks := testParams()
	noDisks.Disks = nil
	ud, _ = mustRender(t, noDisks)
	gate = userDataFiles(t, ud)["/usr/local/sbin/vcd-verify-layout.sh"]
	if !strings.Contains(gate, "nothing to verify") {
		t.Error("with no data disks the gate must be a no-op")
	}
	ensure = userDataFiles(t, ud)["/usr/local/sbin/vcd-ensure-layout.sh"]
	if !strings.Contains(ensure, "no data disks configured") {
		t.Error("with no data disks the repair must be a no-op")
	}
}

// TestRenderedScriptsParse runs bash -n over everything that will run as a
// script, including the YAML-embedded ones.
func TestRenderedScriptsParse(t *testing.T) {
	ud, is := mustRender(t, testParams())
	scripts := map[string]string{"initscript": is}
	for path, content := range userDataFiles(t, ud) {
		if strings.HasSuffix(path, ".sh") {
			scripts[filepath.Base(path)] = content
		}
	}
	for _, want := range []string{"k8s-node-prereqs.sh", "k8s-node-join.sh", "vcd-verify-layout.sh", "vcd-ensure-layout.sh", "vcd-grow-root.sh", "vcd-grow-data-disks.sh"} {
		if _, ok := scripts[want]; !ok {
			t.Fatalf("%s was not rendered", want)
		}
	}
	// The disk script travels inside the initscript heredoc, not in write_files,
	// so it is rendered directly from its own template.
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	var disk bytes.Buffer
	if err := r.diskSetupTpl.Execute(&disk, testParams()); err != nil {
		t.Fatal(err)
	}
	scripts["prepare-data-disks.sh"] = disk.String()
	if !strings.Contains(is, "DISKSETUP") {
		t.Error("initscript does not embed the disk script")
	}
	for name, script := range scripts {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
			t.Errorf("%s syntax error: %v\n%s", name, err, out)
		}
	}
}

// TestUserDataMatchesTheTerraformBaseline pins the four settings item 17 found
// missing on the first controller-created workers, and the root-grow script
// that stands in for the Terraform image's growpart. Each one is a statement
// about a guest, not about a template: a worker built here must behave like one
// built by the baseline playbook.
func TestUserDataMatchesTheTerraformBaseline(t *testing.T) {
	ud, _ := mustRender(t, testParams())
	files := userDataFiles(t, ud)

	if !strings.Contains(ud, "ssh_pwauth: true") {
		t.Error("password login is off; the Terraform baseline allows it")
	}
	sshd, ok := files["/etc/ssh/sshd_config.d/01-k8s-auth.conf"]
	if !ok {
		t.Fatal("the sshd drop-in is missing")
	}
	for _, want := range []string{
		"PubkeyAuthentication yes",
		"PasswordAuthentication yes",
		"KbdInteractiveAuthentication yes",
		"PermitRootLogin no",
	} {
		if !strings.Contains(sshd, want) {
			t.Errorf("sshd drop-in lacks %q", want)
		}
	}
	if !strings.Contains(ud, "Timeout \"60\"") || !strings.Contains(ud, "Retries \"60\"") {
		t.Error("apt does not retry a slow mirror or wait for the dpkg lock")
	}
	if !strings.Contains(files["/usr/local/sbin/k8s-node-prereqs.sh"], "Unattended-Upgrade \"0\"") {
		t.Error("periodic apt is not disabled")
	}

	// /etc/hosts has to be appended, not replaced: cloud-init owns the guest's
	// own line, the pool contributes its peers.
	if !strings.Contains(ud, "path: /etc/hosts") || !strings.Contains(ud, "append: true") {
		t.Fatal("/etc/hosts is not written by the template")
	}
	for _, p := range testParams().HostsLines {
		if !strings.Contains(ud, p) {
			t.Errorf("/etc/hosts lacks %q", p)
		}
	}

	// The root filesystem: grown by a unit that runs on every boot, before
	// anything needs the space. The pre-requisites stage runs once and disables
	// itself with a marker, so a disk grown by a later resize would never reach
	// the guest if the growth lived there (found on the first live resize).
	growUnit, ok := files["/etc/systemd/system/vcd-grow-disks.service"]
	if !ok {
		t.Fatal("vcd-grow-disks.service is missing")
	}
	if !strings.Contains(growUnit, "ExecStart=/usr/local/sbin/vcd-grow-root.sh") {
		t.Error("the boot unit does not grow the root filesystem")
	}
	if !strings.Contains(growUnit, "Before=k8s-node-prereqs.service") {
		t.Error("the boot unit is not ordered before the stage that needs the space")
	}
	prereqs := files["/usr/local/sbin/k8s-node-prereqs.sh"]
	if !strings.Contains(prereqs, "unattended-upgrades") {
		t.Error("the pre-requisites stage does not remove unattended-upgrades")
	}
	if !strings.Contains(prereqs, "cloud-guest-utils") {
		t.Error("growpart is not installed")
	}

	grow, ok := files["/usr/local/sbin/vcd-grow-root.sh"]
	if !ok {
		t.Fatal("vcd-grow-root.sh is missing")
	}
	for _, want := range []string{"growpart", "pvresize", "lvextend", "resize2fs"} {
		if !strings.Contains(grow, want) {
			t.Errorf("grow script never calls %s", want)
		}
	}
	if !strings.Contains(grow, "vg_name=$vg") {
		// Only the VG behind / may be touched: the data VGs own their disks.
		t.Error("grow script does not confine itself to the root volume group")
	}

	if !strings.Contains(ud, "systemctl restart ssh ssh.socket ssh.service") {
		t.Error("the sshd drop-in is never picked up: ssh is not restarted")
	}
}

// The data disks are grown on every boot: prepare-data-disks.sh gives each disk
// one VG with the LV filling it, so a bigger vCD disk becomes bigger space only
// after pvresize, lvextend and a filesystem grow. The run happens before the
// pre-requisites stage installs anything, and it is idempotent.
func TestDataDisksAreGrownAtBoot(t *testing.T) {
	ud, _ := mustRender(t, testParams())
	files := userDataFiles(t, ud)

	grow, ok := files["/usr/local/sbin/vcd-grow-data-disks.sh"]
	if !ok {
		t.Fatal("vcd-grow-data-disks.sh is missing")
	}
	for _, want := range []string{"pvresize", "lvextend", "resize2fs"} {
		if !strings.Contains(grow, want) {
			t.Errorf("the data disk grow script never calls %s", want)
		}
	}
	for _, d := range testParams().Disks {
		if !strings.Contains(grow, d.VG) || !strings.Contains(grow, d.LV) || !strings.Contains(grow, d.Mount) {
			t.Errorf("the grow script does not mention %s (%s/%s)", d.Mount, d.VG, d.LV)
		}
	}

	unit, ok := files["/etc/systemd/system/vcd-grow-disks.service"]
	if !ok {
		t.Fatal("vcd-grow-disks.service is missing")
	}
	if !strings.Contains(unit, "ExecStart=/usr/local/sbin/vcd-grow-data-disks.sh") {
		t.Error("the boot unit does not grow the data disks")
	}
	rootAt := strings.Index(unit, "vcd-grow-root.sh")
	dataAt := strings.Index(unit, "vcd-grow-data-disks.sh")
	if rootAt < 0 || dataAt < rootAt {
		t.Error("the data disks are grown before the root, which is the wrong order")
	}
	if !strings.Contains(ud, "systemctl enable vcd-grow-disks.service") {
		t.Error("the boot unit is never enabled")
	}
}

func TestUserDataIsValidCloudConfig(t *testing.T) {
	ud, _ := mustRender(t, testParams())
	files := userDataFiles(t, ud)
	for _, want := range []string{
		"/etc/kubernetes/join-command.sh",
		"/etc/sysctl.d/k8s.conf",
		"/etc/modules-load.d/k8s.conf",
		"/etc/sysctl.d/99-zzz-nodescale.conf",
		"/etc/modules-load.d/nodescale.conf",
		"/usr/local/share/ca-certificates/nodescale-00.crt",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("%s is missing", want)
		}
	}
	for path, content := range files {
		if strings.Contains(content, "not implemented yet") {
			t.Errorf("%s still contains the placeholder stub", path)
		}
	}
	if strings.Contains(files["/usr/local/sbin/k8s-node-prereqs.sh"], "v1.35") == false {
		t.Error("prereqs script does not pin the Kubernetes minor repo")
	}
}

// TestGolden pins the rendered output. Run with UPDATE_GOLDEN=1 after an
// intentional template change; the diff is the review artefact.
// The mirror has to be in place before the first apt call: on a network with
// no internet access the very first install is the one that fails.
func TestAptMirrorPrecedesEveryAptCall(t *testing.T) {
	ud, _ := mustRender(t, testParams())
	source := strings.Index(ud, "/etc/apt/sources.list.d/nodescale.sources")
	if source < 0 {
		t.Fatal("the mirror is not written to a sources file")
	}
	first := strings.Index(ud, "apt-get")
	if first < 0 {
		t.Fatal("no apt call in the render")
	}
	if source > first {
		t.Errorf("the sources file is written after the first apt call (source=%d, apt=%d)", source, first)
	}
	if !strings.Contains(ud, "https://mirror.example.com/ubuntu-noble") {
		t.Error("the configured mirror is missing from the render")
	}
	// The image's own file must be taken out of the way, or apt reads two
	// URIs and one of them is unreachable.
	if !strings.Contains(ud, "disabled-by-nodescale") {
		t.Error("the image's sources are left active next to the mirror")
	}
	if !strings.Contains(ud, "$codename-updates") {
		t.Error("the suites are not derived from the release when none are given")
	}
}

// Without a mirror the guest keeps the image's sources: a pool that does not
// name a mirror must not have its apt configuration rewritten.
func TestNoAptMirrorKeepsTheImageSources(t *testing.T) {
	p := testParams()
	p.AptMirror = ""
	ud, _ := mustRender(t, p)
	if strings.Contains(ud, "nodescale.sources") {
		t.Error("a sources file was written without bootstrap.apt.mirror")
	}
	if !strings.Contains(ud, "Acquire {") {
		t.Error("the apt retry policy disappeared with the mirror")
	}
}

// The Terraform baseline writes the resolvers to netplan and to a resolved
// drop-in; the drop-in is what stops a second resolver source winning.
func TestResolvedDropInFollowsDNS(t *testing.T) {
	ud, _ := mustRender(t, testParams())
	if !strings.Contains(ud, "/etc/systemd/resolved.conf.d/nodescale.conf") {
		t.Fatal("no resolved drop-in")
	}
	if !strings.Contains(ud, "DNS=192.0.2.53") {
		t.Error("the drop-in does not carry the resolvers")
	}

	p := testParams()
	p.DNSServersLine = ""
	ud, _ = mustRender(t, p)
	if strings.Contains(ud, "resolved.conf.d/nodescale.conf") {
		t.Error("a resolved drop-in was written without DNS servers")
	}
}

// The runc apparmor profile is unloaded defensively like the baseline does, but
// only where apparmor exists: a generic image without it is not an error.
func TestRuncAppArmorIsDisabledWhenPresent(t *testing.T) {
	ud, _ := mustRender(t, testParams())
	if !strings.Contains(ud, "apparmor.d/disable/runc") {
		t.Error("the runc apparmor profile is not disabled")
	}
	if !strings.Contains(ud, "command -v apparmor_parser") {
		t.Error("the apparmor step is not guarded by the binary being present")
	}
}

func TestGolden(t *testing.T) {
	ud, is := mustRender(t, testParams())
	golden := filepath.Join("testdata", "golden")
	cases := map[string]string{
		filepath.Join(golden, "user-data.yaml"): ud,
		filepath.Join(golden, "initscript.sh"):  is,
	}
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(golden, 0o755); err != nil {
			t.Fatal(err)
		}
		for path, content := range cases {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for path, got := range cases {
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s is missing; run UPDATE_GOLDEN=1 go test ./internal/cloudinit", path)
		}
		if string(want) != got {
			t.Errorf("%s differs from the render; run UPDATE_GOLDEN=1 and review the diff", path)
		}
	}
}
