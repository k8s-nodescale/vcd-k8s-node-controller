// Command render renders the worker bootstrap for one node and writes the
// generated files to disk. It is the offline half of validation: the same
// Renderer the controller uses, so a golden diff here is a reviewable statement
// about what a guest will run (customization-phases.md §5.1).
//
//	render -spec node.json -out /tmp/render
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/werton13/vcd-k8s-node-controller/internal/cloudinit"
)

type diskSpec struct {
	Device        string   `json:"device"`
	VG            string   `json:"vg"`
	LV            string   `json:"lv"`
	TempMount     string   `json:"tempMount"`
	Mount         string   `json:"mount"`
	FSType        string   `json:"fsType"`
	SizeGB        int32    `json:"sizeGB"`
	RsyncExcludes []string `json:"rsyncExcludes"`
}

type nodeSpec struct {
	NodeName         string     `json:"nodeName"`
	NodeIP           string     `json:"nodeIP"`
	CIDRPrefix       string     `json:"cidrPrefix"`
	GatewayIP        string     `json:"gatewayIP"`
	NICName          string     `json:"nicName"`
	DNSServers       []string   `json:"dnsServers"`
	OSUser           string     `json:"osUser"`
	OSPasswordHash   string     `json:"osPasswordHash"`
	SSHAuthorizedKey string     `json:"sshAuthorizedKey"`
	K8sVersion       string     `json:"k8sVersion"`
	K8sMinorRepo     string     `json:"k8sMinorRepo"`
	JoinEndpoint     string     `json:"joinEndpoint"`
	JoinCommandFile  string     `json:"joinCommandFile"`
	Disks            []diskSpec `json:"disks"`
}

func main() {
	specPath := flag.String("spec", "", "path to the node spec JSON")
	outDir := flag.String("out", "", "directory for the rendered files")
	flag.Parse()
	if *specPath == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "usage: render -spec node.json -out DIR")
		os.Exit(2)
	}

	raw, err := os.ReadFile(*specPath)
	must(err)
	var s nodeSpec
	must(json.Unmarshal(raw, &s))

	join := ""
	if s.JoinCommandFile != "" {
		b, err := os.ReadFile(s.JoinCommandFile)
		must(err)
		join = string(b)
	}

	dns, err := json.Marshal(s.DNSServers)
	must(err)

	p := cloudinit.Params{
		NodeName:         s.NodeName,
		NodeIP:           s.NodeIP,
		CIDRPrefix:       s.CIDRPrefix,
		GatewayIP:        s.GatewayIP,
		NICName:          s.NICName,
		DNSServersJSON:   string(dns),
		OSUser:           s.OSUser,
		OSPasswordHash:   s.OSPasswordHash,
		SSHAuthorizedKey: s.SSHAuthorizedKey,
		K8sVersion:       s.K8sVersion,
		K8sMinorRepo:     s.K8sMinorRepo,
		JoinEndpoint:     s.JoinEndpoint,
		JoinCommand:      join,
	}
	for _, d := range s.Disks {
		p.Disks = append(p.Disks, cloudinit.DiskParam{
			Device: d.Device, VG: d.VG, LV: d.LV, TempMount: d.TempMount,
			Mount: d.Mount, FSType: d.FSType, SizeGB: d.SizeGB,
			RsyncExcludes: d.RsyncExcludes,
		})
	}

	r, err := cloudinit.New()
	must(err)
	userData, initscript, err := r.Render(p)
	must(err)

	must(os.MkdirAll(*outDir, 0o755))
	must(os.WriteFile(filepath.Join(*outDir, "user-data.yaml"), []byte(userData), 0o644))
	must(os.WriteFile(filepath.Join(*outDir, "initscript.sh"), []byte(initscript), 0o755))
	must(os.WriteFile(filepath.Join(*outDir, "stamp.txt"), []byte(r.Stamp()+"\n"), 0o644))

	fmt.Printf("rendered %s\n  user-data: %d bytes\n  initscript: %d bytes\n",
		r.Stamp(), len(userData), len(initscript))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
