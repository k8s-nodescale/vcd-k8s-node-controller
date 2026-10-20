package cloudinit

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRenderWithDataDisks(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	userData, initscript, err := r.Render(Params{
		NodeName:       "pool-wrk-01",
		NodeIP:         "192.0.2.10",
		CIDRPrefix:     "24",
		GatewayIP:      "192.0.2.1",
		NICName:        "ens192",
		DNSServersJSON: `["192.0.2.53"]`,
		OSUser:         "ubuntu",
		JoinCommand:    "kubeadm join example",
		Disks: []DiskParam{
			{
				Device:        "/dev/sdb",
				VG:            "data1_vg",
				LV:            "data1_lv",
				TempMount:     "/data1",
				Mount:         "/var",
				FSType:        "ext4",
				SizeGB:        10,
				RsyncExcludes: []string{"/lib"},
			},
			{
				Device:    "/dev/sdc",
				VG:        "data2_vg",
				LV:        "data2_lv",
				TempMount: "/data2",
				Mount:     "/var/lib",
				FSType:    "ext4",
				SizeGB:    30,
			},
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, want := range []string{
		"prepare-data-disks.sh",
		`sync_data "/var" "/data1" "/lib"`,
		`sync_data "/var/lib" "/data2"`,
		`prepare_disk "/dev/sdb" "data1_vg" "data1_lv" "/data1" "ext4" 10`,
		`write_fstab "/var/lib" "/data2" "data2_vg" "data2_lv" "ext4"`,
	} {
		if !strings.Contains(initscript, want) {
			t.Errorf("initscript does not contain %q", want)
		}
	}
	if !strings.Contains(userData, "bootcmd:") {
		t.Error("user-data does not contain bootcmd")
	}
}

func TestRenderWithoutDataDisks(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	userData, initscript, err := r.Render(Params{
		NodeName:       "pool-wrk-01",
		NodeIP:         "192.0.2.10",
		CIDRPrefix:     "24",
		GatewayIP:      "192.0.2.1",
		NICName:        "ens192",
		DNSServersJSON: `["192.0.2.53"]`,
		OSUser:         "ubuntu",
		JoinCommand:    "kubeadm join example",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(initscript, "prepare-data-disks.sh") {
		t.Error("initscript should not prepare disks when none are configured")
	}
	if strings.Contains(userData, "prepare-data-disks.sh") {
		t.Error("user-data should not reference the disk script when no disks are configured")
	}
	if !strings.Contains(userData, "bootcmd:") {
		t.Error("user-data must enable the bootstrap unit even without data disks")
	}
}

func TestRenderedScriptsParse(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p := Params{
		NodeName:       "pool-wrk-01",
		NodeIP:         "192.0.2.10",
		CIDRPrefix:     "24",
		GatewayIP:      "192.0.2.1",
		NICName:        "ens192",
		DNSServersJSON: `["192.0.2.53"]`,
		OSUser:         "ubuntu",
		JoinCommand:    "kubeadm join example",
		Disks: []DiskParam{{
			Device:        "/dev/sdb",
			VG:            "data1_vg",
			LV:            "data1_lv",
			TempMount:     "/data1",
			Mount:         "/var",
			FSType:        "ext4",
			SizeGB:        10,
			RsyncExcludes: []string{"/lib"},
		}},
	}

	var diskScript bytes.Buffer
	if err := r.diskSetupTpl.Execute(&diskScript, p); err != nil {
		t.Fatalf("render disk setup: %v", err)
	}
	_, initscript, err := r.Render(p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for name, script := range map[string]string{
		"disk-setup": diskScript.String(),
		"initscript": initscript,
	} {
		path := filepath.Join(t.TempDir(), name+".sh")
		if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		out, err := exec.Command("bash", "-n", path).CombinedOutput()
		if err != nil {
			t.Fatalf("%s syntax error: %v\n%s", name, err, out)
		}
	}
}

func TestRenderedUserDataIsValidCloudConfig(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	userData, initscript, err := r.Render(Params{
		NodeName:       "pool-wrk-01",
		NodeIP:         "192.0.2.10",
		CIDRPrefix:     "24",
		GatewayIP:      "192.0.2.1",
		NICName:        "ens192",
		DNSServersJSON: "['192.0.2.53']",
		OSUser:         "ubuntu",
		K8sVersion:     "1.35.9-1.1",
		K8sMinorRepo:   "v1.35",
		JoinCommand:    "kubeadm join 192.0.2.2:6443 --token x --discovery-token-ca-cert-hash sha256:y",
		Disks: []DiskParam{{
			Device: "/dev/sdb", VG: "data1_vg", LV: "data1_lv",
			TempMount: "/data1", Mount: "/var", FSType: "ext4", SizeGB: 10,
		}},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(userData), &doc); err != nil {
		t.Fatalf("rendered user-data is not valid YAML: %v", err)
	}
	if bootcmd, _ := doc["bootcmd"].([]interface{}); len(bootcmd) == 0 {
		t.Fatal("user-data has no bootcmd")
	}
	if runcmd, _ := doc["runcmd"].([]interface{}); len(runcmd) == 0 {
		t.Fatal("user-data has no runcmd")
	}

	var bootstrap string
	unit := false
	files, _ := doc["write_files"].([]interface{})
	for _, item := range files {
		f, _ := item.(map[string]interface{})
		path, _ := f["path"].(string)
		content, _ := f["content"].(string)
		if strings.Contains(content, "not implemented yet") {
			t.Errorf("%s still contains the placeholder stub", path)
		}
		switch path {
		case "/usr/local/sbin/k8s-node-bootstrap.sh":
			bootstrap = content
		case "/etc/systemd/system/k8s-node-bootstrap.service":
			unit = true
		}
	}
	if bootstrap == "" {
		t.Fatal("bootstrap script was not rendered")
	}
	if !unit {
		t.Fatal("bootstrap systemd unit was not rendered")
	}
	for _, want := range []string{"v1.35", "swapoff -a", "modprobe br_netfilter", "sysctl --system", "pkgs.k8s.io", "join-command.sh", "/etc/kubernetes/kubeadm-joined"} {
		if !strings.Contains(bootstrap, want) {
			t.Errorf("bootstrap script does not contain %q", want)
		}
	}
	if strings.Contains(initscript, "cloud-init init --local || true") {
		t.Error("initscript still swallows cloud-init failures")
	}

	path := filepath.Join(t.TempDir(), "bootstrap.sh")
	if err := os.WriteFile(path, []byte(bootstrap), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bootstrap script syntax error: %v\n%s", err, out)
	}
}
