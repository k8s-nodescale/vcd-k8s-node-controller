// Guest bootstrap for vSphere: the annotation codec, the guestinfo documents
// cloud-init reads on the first boot, and the input checks the create needs.
//
// vSphere has no NoCloud seed device here (the controller account cannot upload
// an ISO: Datastore.FileManagement is not granted), so everything the guest
// needs travels in the VM extraConfig: guestinfo.userdata carries the rendered
// cloud-config and guestinfo.metadata carries the network document. Both are
// base64 with the matching .encoding property, which is exactly what the
// Terraform module writes and what cloud-init's VMware datasource expects.
package vsphere

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vmware/govmomi/vim25/types"

	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// annotationAPIVersion marks the VM annotation as ours. Anything else in the
// annotation field - a note a human left, or the annotation of a VM Terraform
// created - is not decoded, so an adopted worker carries no opinion.
const annotationAPIVersion = "nodescale.k8s/v1"

// metadataKeyTemplate mirrors the vCD provider: the image a worker was cloned
// from, so a rollout replaces a worker whose spec.flavor.template changed.
const metadataKeyTemplate = "template"

// metadataKeyIP is the address the slot was given. The guest reports one only
// once it boots, and a worker that never boots is exactly the case an operator
// needs to see the address for.
const metadataKeyIP = "ip"

// vmAnnotation is the JSON document stored in VirtualMachine.Config.Annotation.
type vmAnnotation struct {
	APIVersion string            `json:"apiVersion"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	IP         string            `json:"ip,omitempty"`
	Template   string            `json:"template,omitempty"`
}

// encodeAnnotation renders the annotation for a new worker. An empty result
// means there is nothing worth recording.
func encodeAnnotation(metadata map[string]string, ip, template string) string {
	if len(metadata) == 0 && ip == "" && template == "" {
		return ""
	}
	b, err := json.Marshal(vmAnnotation{APIVersion: annotationAPIVersion, Metadata: metadata, IP: ip, Template: template})
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeAnnotation returns the document, or the zero value when the annotation
// is empty or was not written by this controller.
func decodeAnnotation(raw string) vmAnnotation {
	s := strings.TrimSpace(raw)
	if s == "" || !strings.HasPrefix(s, "{") {
		return vmAnnotation{}
	}
	var a vmAnnotation
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		return vmAnnotation{}
	}
	if a.APIVersion != annotationAPIVersion {
		return vmAnnotation{}
	}
	return a
}

// guestExtraConfig builds the extraConfig entries of a clone: the user-data,
// the metadata document and disk.EnableUUID. An empty document is skipped,
// which keeps a template with its own bootstrap from being overwritten by an
// empty one.
//
// The documents are gzip-compressed before base64: a rendered user-data is
// ~35 KB and grows with the CA bundle a pool may add, while a guestinfo value
// is limited (64 KB) and the base64 layer alone would spend a third of it.
// cloud-init's VMware datasource understands "gzip+base64" (DataSourceVMware,
// enc_type), so the guest decodes it without help.
func guestExtraConfig(userData, metadata string, enableUUID bool) ([]types.BaseOptionValue, error) {
	var out []types.BaseOptionValue
	add := func(key, value string) error {
		if value == "" {
			return nil
		}
		enc, err := gzipBase64(value)
		if err != nil {
			return err
		}
		out = append(out,
			&types.OptionValue{Key: key, Value: enc},
			&types.OptionValue{Key: key + ".encoding", Value: "gzip+base64"},
		)
		return nil
	}
	if err := add("guestinfo.userdata", userData); err != nil {
		return nil, err
	}
	if err := add("guestinfo.metadata", metadata); err != nil {
		return nil, err
	}
	if enableUUID {
		out = append(out, &types.OptionValue{Key: "disk.EnableUUID", Value: "TRUE"})
	}
	return out, nil
}

// gzipBase64 compresses a guestinfo document and encodes it for the .encoding
// property cloud-init reads.
func gzipBase64(in string) (string, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(in)); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// metadataDocument is the cloud-init network document for one worker. It has to
// be there: without it the first boot comes up with no address and the only
// way in is the console.
//
// The interface is matched by name prefix (ens*) with set-name so the guest
// name ends up as the one spec.bootstrap.nicName names, even when the adapter
// enumerates differently on this hardware. That is the same match the Terraform
// module uses.
func metadataDocument(p cloud.VSpherePlacement, name, ip string) string {
	if ip == "" || p.Gateway == "" || p.PrefixLen <= 0 || p.NICName == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("instance-id: " + name + "\n")
	b.WriteString("local-hostname: " + name + "\n")
	b.WriteString("network:\n  version: 2\n  ethernets:\n")
	b.WriteString("    " + p.NICName + ":\n")
	match := p.NICName
	if strings.HasPrefix(p.NICName, "ens") {
		match = "ens*"
	}
	b.WriteString("      match:\n        name: \"" + match + "\"\n")
	if match != p.NICName {
		b.WriteString("      set-name: " + p.NICName + "\n")
	}
	b.WriteString("      dhcp4: false\n")
	b.WriteString(fmt.Sprintf("      addresses: [%s/%d]\n", ip, p.PrefixLen))
	b.WriteString("      routes:\n        - to: default\n          via: " + p.Gateway + "\n")
	if len(p.DNSServers) > 0 {
		b.WriteString("      nameservers:\n        addresses: [" + strings.Join(p.DNSServers, ", ") + "]\n")
	}
	return b.String()
}

// validatePlacement checks the inputs a create cannot invent: without a network
// the guest has no address, and without the prefix the address has no mask.
func (p *Provider) validatePlacement() error {
	switch {
	case strings.TrimSpace(p.placement.Cluster) == "":
		return errors.New("vsphere: spec.vsphere.cluster is empty")
	case strings.TrimSpace(p.placement.Folder) == "":
		return errors.New("vsphere: spec.vsphere.folder is empty")
	case strings.TrimSpace(p.placement.Datastore) == "":
		return errors.New("vsphere: spec.vsphere.datastore is empty")
	case strings.TrimSpace(p.placement.Network) == "":
		return errors.New("vsphere: spec.vsphere.network is empty")
	case strings.TrimSpace(p.placement.Gateway) == "":
		return errors.New("vsphere: spec.ipPlan.cidr has no gateway")
	case p.placement.PrefixLen <= 0:
		return errors.New("vsphere: spec.ipPlan.cidr has no prefix")
	}
	return nil
}

// validateCreateVMSpec mirrors the vCD checks: a worker without a name, an
// address or a template cannot be cloned.
func validateCreateVMSpec(spec cloud.CreateVMSpec) error {
	switch {
	case strings.TrimSpace(spec.Name) == "":
		return errors.New("VM name is empty")
	case strings.TrimSpace(spec.IP) == "":
		return errors.New("VM IP is empty")
	case strings.TrimSpace(spec.Template) == "":
		return errors.New("template is empty")
	case spec.CPUs <= 0:
		return errors.New("CPUs must be greater than zero")
	case spec.MemoryMB <= 0:
		return errors.New("memoryMB must be greater than zero")
	}
	for i, disk := range spec.Disks {
		if disk.SizeGB <= 0 {
			return fmt.Errorf("disk %d: sizeGB must be greater than zero", i)
		}
		if bus, err := scsiBusType(disk.BusType); err != nil || bus == nil {
			return fmt.Errorf("disk %d: %w", i, err)
		}
	}
	return nil
}
