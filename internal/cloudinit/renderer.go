// Package cloudinit renders the vCD initscript and worker cloud-init user-data.
//
// The render is a pure function of Params: two calls with the same Params
// produce byte-identical output, which is what makes a guest reproducible and
// diffable. Every generated file carries a render stamp (version + template
// hash) so a VM can be traced back to the build that created it, and the
// NoCloud instance-id carries a spec hash so cloud-init re-runs exactly when
// the spec changed (see customization-phases.md §5).
package cloudinit

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"text/template"
)

// RenderVersion is bumped whenever the rendered output changes in a way that
// matters to a running guest. It is stamped into every generated file.
const RenderVersion = "0.2.0"

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
	JoinEndpoint     string // e.g. "192.168.100.2:6443"
	JoinCommand      string
	Disks            []DiskParam
}

// Renderer renders the templates.
type Renderer struct {
	userDataTpl   *template.Template
	initscriptTpl *template.Template
	diskSetupTpl  *template.Template
	layoutTpl     *template.Template
	ensureTpl     *template.Template
	stamp         string
}

// New parses the embedded templates and hashes them into the render stamp.
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
	layout, err := template.ParseFS(templatesFS, "templates/verify-layout.sh.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse layout template: %w", err)
	}
	ensure, err := template.ParseFS(templatesFS, "templates/ensure-layout.sh.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse layout repair template: %w", err)
	}
	hash, err := templateHash()
	if err != nil {
		return nil, err
	}
	return &Renderer{
		userDataTpl:   ud,
		initscriptTpl: is,
		diskSetupTpl:  diskSetup,
		layoutTpl:     layout,
		ensureTpl:     ensure,
		stamp:         fmt.Sprintf("render=%s hash=%s", RenderVersion, hash),
	}, nil
}

// Stamp identifies the render, e.g. "render=0.2.0 hash=1a2b3c4d5e6f". It is
// written into the generated files and into VM metadata by the controller.
func (r *Renderer) Stamp() string { return r.stamp }

// Render returns the cloud-init user-data and the vCD customization initscript.
func (r *Renderer) Render(p Params) (userData string, initscript string, err error) {
	specHash := specHash8(p)

	var layout bytes.Buffer
	if err := r.layoutTpl.Execute(&layout, p); err != nil {
		return "", "", fmt.Errorf("render layout check: %w", err)
	}
	var ensure bytes.Buffer
	if err := r.ensureTpl.Execute(&ensure, p); err != nil {
		return "", "", fmt.Errorf("render layout repair: %w", err)
	}

	var ud bytes.Buffer
	if err := r.userDataTpl.Execute(&ud, struct {
		Params
		RenderStamp  string
		LayoutScript string
		EnsureScript string
	}{p, r.stamp, indentLines(layout.String(), 6), indentLines(ensure.String(), 6)}); err != nil {
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
		RenderStamp       string
		SpecHash8         string
		CloudInitBase64Gz string
		DiskSetupScript   string
	}{p, r.stamp, specHash, gz, ds.String()}); err != nil {
		return "", "", fmt.Errorf("render initscript: %w", err)
	}
	return ud.String(), is.String(), nil
}

// specHash8 identifies the *spec* a guest was built from, deliberately
// excluding secrets (the join token) and the credentials: a re-created VM with
// a fresh token must not look like "the same instance" to cloud-init, and a
// changed disk layout or image must.
func specHash8(p Params) string {
	var b strings.Builder
	for _, f := range []string{
		p.NodeName, p.NodeIP, p.CIDRPrefix, p.GatewayIP, p.NICName,
		p.DNSServersJSON, p.OSUser, p.K8sVersion, p.K8sMinorRepo, p.JoinEndpoint,
	} {
		b.WriteString(f)
		b.WriteByte('|')
	}
	for _, d := range p.Disks {
		fmt.Fprintf(&b, "%s/%s/%s/%s/%s/%s/%d/%s|",
			d.Device, d.VG, d.LV, d.TempMount, d.Mount, d.FSType, d.SizeGB,
			strings.Join(d.RsyncExcludes, ","))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:8]
}

// templateHash fingerprints the embedded templates so a rendered guest can be
// traced to the exact build that produced it.
func templateHash() (string, error) {
	names, err := fs.Glob(templatesFS, "templates/*.tmpl")
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		content, err := templatesFS.ReadFile(name)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s:%d\n", name, len(content))
		h.Write(content)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// indentLines prefixes every non-empty line with n spaces so a rendered script
// can be embedded in a YAML block scalar.
func indentLines(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[i] = ""
			continue
		}
		lines[i] = pad + line
	}
	return strings.Join(lines, "\n")
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
