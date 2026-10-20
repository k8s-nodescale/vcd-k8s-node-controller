// Package cloudinit renders the vCD initscript and worker cloud-init user-data.
package cloudinit

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/base64"
	"fmt"
	"text/template"
)

//go:embed templates/*.tmpl
var templatesFS embed.FS

// DiskParam describes one additional data disk for the Option A layout.
//
// Device is the guest device name (e.g. /dev/sdb). TempMount is the temporary
// mount used while copying the old contents; Mount is the final fstab mount
// point (e.g. /var or /var/lib). RsyncExcludes are relative to Mount and keep
// nested final mounts out of the parent copy (e.g. /lib for /var).
type DiskParam struct {
	Device        string
	VG            string
	LV            string
	TempMount     string
	Mount         string
	FSType        string
	SizeGB        int32
	RsyncExcludes []string
}

// Params is the full input for rendering a worker bootstrap.
type Params struct {
	NodeName         string
	NodeIP           string
	CIDRPrefix       string // e.g. "24"
	GatewayIP        string
	NICName          string
	DNSServersJSON   string // JSON array, e.g. ["8.8.8.8","1.1.1.1"]
	OSUser           string
	OSPasswordHash   string
	SSHAuthorizedKey string
	K8sVersion       string // apt version, e.g. "1.35.9-1.1"
	K8sMinorRepo     string // e.g. "v1.35"
	JoinCommand      string
	Disks            []DiskParam
}

// Renderer renders the templates.
type Renderer struct {
	userDataTpl   *template.Template
	initscriptTpl *template.Template
	diskSetupTpl  *template.Template
}

// New parses the embedded templates.
func New() (*Renderer, error) {
	ud, err := template.ParseFS(templatesFS, "templates/worker-userdata.yaml.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse user-data template: %w", err)
	}
	is, err := template.ParseFS(templatesFS, "templates/node-initscript.sh.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse initscript template: %w", err)
	}
	diskSetup, err := template.ParseFS(templatesFS, "templates/prepare-data-disks.sh.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse disk setup template: %w", err)
	}
	return &Renderer{userDataTpl: ud, initscriptTpl: is, diskSetupTpl: diskSetup}, nil
}

// Render returns the cloud-init user-data and the vCD customization initscript.
func (r *Renderer) Render(p Params) (userData string, initscript string, err error) {
	var ud bytes.Buffer
	if err := r.userDataTpl.Execute(&ud, p); err != nil {
		return "", "", fmt.Errorf("render user-data: %w", err)
	}
	gz, err := base64Gzip(ud.Bytes())
	if err != nil {
		return "", "", err
	}
	var ds bytes.Buffer
	if err := r.diskSetupTpl.Execute(&ds, p); err != nil {
		return "", "", fmt.Errorf("render disk setup: %w", err)
	}
	var is bytes.Buffer
	if err := r.initscriptTpl.Execute(&is, struct {
		Params
		CloudInitBase64Gz string
		DiskSetupScript   string
	}{Params: p, CloudInitBase64Gz: gz, DiskSetupScript: ds.String()}); err != nil {
		return "", "", fmt.Errorf("render initscript: %w", err)
	}
	return ud.String(), is.String(), nil
}

func base64Gzip(in []byte) (string, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(in); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
