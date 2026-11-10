package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloudinit"
)

// installationBootstrap is the shape of the bootstrap.yaml key in the
// installation ConfigMap (values.bootstrap.defaults of the chart). It carries
// environment policy only: the account, password hash and SSH key describe a
// cluster and are always read from the pool.
type installationBootstrap struct {
	NTPServers    []string                  `json:"ntpServers,omitempty"`
	CABundleRef   *infrav1.ConfigKeyRef     `json:"caBundleRef,omitempty"`
	Sysctls       map[string]string         `json:"sysctls,omitempty"`
	KernelModules []string                  `json:"kernelModules,omitempty"`
	Containerd    *infrav1.ContainerdConfig `json:"containerd,omitempty"`
	Apt           *infrav1.AptConfig        `json:"apt,omitempty"`
}

// guestBootstrap is the resolved guest policy: the pool's spec with the
// installation defaults filled in and every ConfigMap/Secret reference replaced
// by its content, ready for the renderer.
type guestBootstrap struct {
	NTPServers             []string
	CACerts                []string
	Sysctls                []string
	KernelModules          []string
	ContainerdSandboxImage string
	ContainerdMirrors      []cloudinit.MirrorParam
	ContainerdInsecure     bool
	ContainerdFull         string
	AptMirror              string
	AptSuites              []string
	AptComponents          []string
}

// effectiveBootstrap merges spec.bootstrap with the installation-wide defaults
// and resolves both references.
//
// Precedence is per field: a pool that names its own NTP servers, CA bundle or
// containerd fragment keeps them, and the installation ConfigMap fills only
// what the pool left empty. A reference from the pool resolves in the pool's
// namespace; a reference from the installation defaults resolves where the
// controller runs, because that is where the chart put the ConfigMap.
func (r *WorkerPoolReconciler) effectiveBootstrap(ctx context.Context, pool *infrav1.K8sWorkerPool) (guestBootstrap, error) {
	spec := pool.Spec.Bootstrap

	ntp := append([]string(nil), spec.NTPServers...)
	sysctls := copyStringMap(spec.Sysctls)
	modules := append([]string(nil), spec.KernelModules...)
	containerd := copyContainerd(spec.Containerd)
	apt := copyApt(spec.Apt)
	caRef := copyRef(spec.CABundleRef)
	caNamespace := pool.Namespace

	if r.BootstrapConfigMap != "" {
		def, err := r.readInstallationBootstrap(ctx)
		if err != nil {
			return guestBootstrap{}, err
		}
		if len(ntp) == 0 {
			ntp = def.NTPServers
		}
		if len(sysctls) == 0 {
			sysctls = copyStringMap(def.Sysctls)
		}
		if len(modules) == 0 {
			modules = def.KernelModules
		}
		if caRef == nil && def.CABundleRef != nil {
			caRef = copyRef(def.CABundleRef)
			caNamespace = r.PodNamespace
		}
		containerd = mergeContainerd(def.Containerd, containerd)
		apt = mergeApt(def.Apt, apt)
	}

	var certs []string
	if caRef != nil {
		bundle, err := r.readRef(ctx, caRef, caNamespace)
		if err != nil {
			return guestBootstrap{}, fmt.Errorf("spec.bootstrap.caBundleRef: %w", err)
		}
		certs = cloudinit.CACertBlocks(bundle)
		if len(certs) == 0 {
			return guestBootstrap{}, fmt.Errorf("spec.bootstrap.caBundleRef: %s/%s key %q holds no PEM certificate",
				refNamespace(caRef, caNamespace), caRef.Name, caRef.Key)
		}
	}

	out := guestBootstrap{
		NTPServers:    ntp,
		CACerts:       certs,
		Sysctls:       cloudinit.SysctlLines(sysctls),
		KernelModules: modules,
		AptMirror:     aptMirror(apt),
		AptSuites:     aptSuites(apt),
		AptComponents: aptComponents(apt),
	}
	if containerd != nil && containerd.ConfigRef != nil {
		full, err := r.readRef(ctx, containerd.ConfigRef, pool.Namespace)
		if err != nil {
			return guestBootstrap{}, fmt.Errorf("spec.bootstrap.containerd.configRef: %w", err)
		}
		out.ContainerdFull = full
		return out, nil
	}
	// The drop-in is always written: SystemdCgroup=true is not optional with the
	// systemd cgroup driver kubelet runs, and a generic image ships no config at
	// all. Mirrors and the pause image are added when the pool or the
	// installation defaults name them.
	if containerd == nil {
		containerd = &infrav1.ContainerdConfig{}
	}
	out.ContainerdSandboxImage = containerd.SandboxImage
	out.ContainerdMirrors = cloudinit.Mirrors(containerd.Mirrors)
	out.ContainerdInsecure = containerd.InsecureSkipVerify
	return out, nil
}

// readInstallationBootstrap reads the bootstrap.yaml key of the installation
// ConfigMap. A missing ConfigMap is an error when the chart named one: silently
// dropping the CA bundle and the mirrors would surface much later, as a worker
// that cannot pull an image.
func (r *WorkerPoolReconciler) readInstallationBootstrap(ctx context.Context) (installationBootstrap, error) {
	namespace := r.PodNamespace
	if strings.TrimSpace(namespace) == "" {
		return installationBootstrap{}, fmt.Errorf("NODESCALE_BOOTSTRAP_CONFIGMAP=%q is set but POD_NAMESPACE is empty",
			r.BootstrapConfigMap)
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: namespace, Name: r.BootstrapConfigMap}
	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return installationBootstrap{}, fmt.Errorf("the bootstrap ConfigMap %s/%s is missing (create it with the chart, or unset NODESCALE_BOOTSTRAP_CONFIGMAP)",
				namespace, r.BootstrapConfigMap)
		}
		return installationBootstrap{}, fmt.Errorf("read the bootstrap ConfigMap %s/%s: %w", namespace, r.BootstrapConfigMap, err)
	}
	raw := strings.TrimSpace(cm.Data["bootstrap.yaml"])
	if raw == "" {
		return installationBootstrap{}, nil
	}
	var out installationBootstrap
	if err := yaml.Unmarshal([]byte(raw), &out); err != nil {
		return installationBootstrap{}, fmt.Errorf("parse %s/%s key bootstrap.yaml: %w", namespace, r.BootstrapConfigMap, err)
	}
	return out, nil
}

// readRef resolves one ConfigMap/Secret key. An empty namespace means
// defaultNamespace.
func (r *WorkerPoolReconciler) readRef(ctx context.Context, ref *infrav1.ConfigKeyRef, defaultNamespace string) (string, error) {
	if ref == nil {
		return "", nil
	}
	if strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.Key) == "" {
		return "", fmt.Errorf("a ConfigMap/Secret reference needs name and key")
	}
	namespace := refNamespace(ref, defaultNamespace)
	key := types.NamespacedName{Namespace: namespace, Name: ref.Name}

	switch strings.ToLower(strings.TrimSpace(ref.Kind)) {
	case "", "configmap":
		var cm corev1.ConfigMap
		if err := r.Get(ctx, key, &cm); err != nil {
			return "", fmt.Errorf("ConfigMap %s/%s: %w", namespace, ref.Name, err)
		}
		value, ok := cm.Data[ref.Key]
		if !ok {
			return "", fmt.Errorf("ConfigMap %s/%s has no key %q", namespace, ref.Name, ref.Key)
		}
		return value, nil
	case "secret":
		var secret corev1.Secret
		if err := r.Get(ctx, key, &secret); err != nil {
			return "", fmt.Errorf("Secret %s/%s: %w", namespace, ref.Name, err)
		}
		value, ok := secret.Data[ref.Key]
		if !ok {
			return "", fmt.Errorf("Secret %s/%s has no key %q", namespace, ref.Name, ref.Key)
		}
		return string(value), nil
	default:
		return "", fmt.Errorf("kind %q is not ConfigMap or Secret", ref.Kind)
	}
}

func refNamespace(ref *infrav1.ConfigKeyRef, defaultNamespace string) string {
	if ref != nil && strings.TrimSpace(ref.Namespace) != "" {
		return ref.Namespace
	}
	return defaultNamespace
}

// mergeContainerd layers a pool's fragment over the installation defaults.
// Field by field, so a pool that only overrides the pause image keeps the
// installation's mirrors.
func mergeContainerd(base, override *infrav1.ContainerdConfig) *infrav1.ContainerdConfig {
	if base == nil && override == nil {
		return nil
	}
	out := &infrav1.ContainerdConfig{}
	if base != nil {
		*out = *base
	}
	if override != nil {
		if override.SandboxImage != "" {
			out.SandboxImage = override.SandboxImage
		}
		if len(override.Mirrors) > 0 {
			out.Mirrors = override.Mirrors
		}
		if override.InsecureSkipVerify {
			out.InsecureSkipVerify = true
		}
		if override.ConfigRef != nil {
			out.ConfigRef = override.ConfigRef
		}
	}
	return out
}

func copyContainerd(in *infrav1.ContainerdConfig) *infrav1.ContainerdConfig {
	if in == nil {
		return nil
	}
	out := *in
	out.ConfigRef = copyRef(in.ConfigRef)
	if len(in.Mirrors) > 0 {
		out.Mirrors = make(map[string][]string, len(in.Mirrors))
		for host, endpoints := range in.Mirrors {
			out.Mirrors[host] = append([]string(nil), endpoints...)
		}
	}
	return &out
}

// copyApt deep-copies the package-source policy so a merge cannot write into
// the pool's object.
func copyApt(in *infrav1.AptConfig) *infrav1.AptConfig {
	if in == nil {
		return nil
	}
	out := *in
	out.Suites = append([]string(nil), in.Suites...)
	out.Components = append([]string(nil), in.Components...)
	return &out
}

// mergeApt fills what the pool left empty, per field, like the other policy
// values: a pool that names its own mirror keeps its own suites too.
func mergeApt(base, override *infrav1.AptConfig) *infrav1.AptConfig {
	if base == nil {
		return override
	}
	if override == nil {
		return copyApt(base)
	}
	out := copyApt(base)
	if strings.TrimSpace(override.Mirror) != "" {
		out.Mirror = override.Mirror
	}
	if len(override.Suites) > 0 {
		out.Suites = append([]string(nil), override.Suites...)
	}
	if len(override.Components) > 0 {
		out.Components = append([]string(nil), override.Components...)
	}
	return out
}

func aptMirror(in *infrav1.AptConfig) string {
	if in == nil {
		return ""
	}
	return strings.TrimSpace(in.Mirror)
}

func aptSuites(in *infrav1.AptConfig) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in.Suites...)
}

func aptComponents(in *infrav1.AptConfig) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in.Components...)
}

func copyRef(in *infrav1.ConfigKeyRef) *infrav1.ConfigKeyRef {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// validateGuestPolicy rejects values that would produce a broken guest instead
// of a clear message: a sysctl key that is not a kernel parameter name, a value
// that would break out of the single-line file, an NTP server or a mirror
// endpoint with whitespace. The rules are deliberately simple: everything here
// is written into a shell-free file, so only the separators matter.
func validateGuestPolicy(b infrav1.BootstrapConfig) string {
	for key, value := range b.Sysctls {
		if !validSysctlKey(key) {
			return fmt.Sprintf("spec.bootstrap.sysctls: %q is not a sysctl name (letters, digits, '.', '_', '-', '/')", key)
		}
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\n\r") {
			return fmt.Sprintf("spec.bootstrap.sysctls[%q]: the value must be one non-empty line", key)
		}
	}
	for _, server := range b.NTPServers {
		if strings.TrimSpace(server) == "" || strings.ContainsAny(server, " \t\n\r") {
			return fmt.Sprintf("spec.bootstrap.ntpServers: %q is not a host name or address", server)
		}
	}
	for _, module := range b.KernelModules {
		if strings.TrimSpace(module) == "" || strings.ContainsAny(module, " \t\n\r") {
			return fmt.Sprintf("spec.bootstrap.kernelModules: %q is not a module name", module)
		}
	}
	if a := b.Apt; a != nil {
		mirror := strings.TrimSpace(a.Mirror)
		if mirror != "" {
			if strings.ContainsAny(mirror, " \t\n\r\"") {
				return fmt.Sprintf("spec.bootstrap.apt.mirror: %q is not a URI", mirror)
			}
			if !strings.HasPrefix(mirror, "http://") && !strings.HasPrefix(mirror, "https://") {
				return fmt.Sprintf("spec.bootstrap.apt.mirror: %q must start with http:// or https://", mirror)
			}
		}
		for _, suite := range a.Suites {
			if strings.TrimSpace(suite) == "" || strings.ContainsAny(suite, " \t\n\r") {
				return fmt.Sprintf("spec.bootstrap.apt.suites: %q is not a suite name", suite)
			}
		}
		for _, component := range a.Components {
			if strings.TrimSpace(component) == "" || strings.ContainsAny(component, " \t\n\r") {
				return fmt.Sprintf("spec.bootstrap.apt.components: %q is not a component name", component)
			}
		}
	}
	if c := b.Containerd; c != nil {
		for host, endpoints := range c.Mirrors {
			if strings.TrimSpace(host) == "" || strings.ContainsAny(host, " \t\n\r\"") {
				return fmt.Sprintf("spec.bootstrap.containerd.mirrors: %q is not a registry host", host)
			}
			if len(endpoints) == 0 {
				return fmt.Sprintf("spec.bootstrap.containerd.mirrors[%q]: at least one endpoint is required", host)
			}
			for _, endpoint := range endpoints {
				if strings.TrimSpace(endpoint) == "" || strings.ContainsAny(endpoint, " \t\n\r\"") {
					return fmt.Sprintf("spec.bootstrap.containerd.mirrors[%q]: %q is not an endpoint", host, endpoint)
				}
			}
		}
	}
	return ""
}

func validSysctlKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '/':
		default:
			return false
		}
	}
	return true
}
