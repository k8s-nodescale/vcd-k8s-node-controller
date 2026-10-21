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
		OSUser:         "ubuntu",
		K8sVersion:     "1.35.9-1.1",
		K8sMinorRepo:   "v1.35",
		JoinEndpoint:   "192.0.2.2:6443",
		JoinCommand:    "kubeadm join 192.0.2.2:6443 --token x --discovery-token-ca-cert-hash sha256:y",
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
	for _, want := range []string{"k8s-node-prereqs.sh", "k8s-node-join.sh", "vcd-verify-layout.sh", "vcd-ensure-layout.sh"} {
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

func TestUserDataIsValidCloudConfig(t *testing.T) {
	ud, _ := mustRender(t, testParams())
	files := userDataFiles(t, ud)
	for _, want := range []string{
		"/etc/kubernetes/join-command.sh",
		"/etc/sysctl.d/k8s.conf",
		"/etc/modules-load.d/k8s.conf",
		"/etc/containerd/config.toml",
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
