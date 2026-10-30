package mgmt

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultExporterLabels is the label the prometheus-community node-exporter
// chart puts on its DaemonSet, which is what the installations deploy.
const DefaultExporterLabels = "app.kubernetes.io/name=prometheus-node-exporter"

// DefaultExporterPort is node-exporter's own port.
const DefaultExporterPort = 9100

// exporterSample is one scrape of node-exporter, reduced to what the table
// needs. CPU is a counter, so the share comes from the delta between two
// samples; memory is a gauge and answers on the first one.
type exporterSample struct {
	at      time.Time
	idle    float64
	cpus    int
	avail   float64
	total   float64
	haveMem bool
	// fs is the filesystems worth a column: the OS root and the two data
	// mounts, by mountpoint.
	fs map[string]fsSample
}

// diskMounts are the columns the page shows. The order is the column order.
var diskMounts = []string{"/", "/var", "/var/lib"}

// fsSample is one filesystem: total and available bytes.
type fsSample struct {
	size  float64
	avail float64
}

// realUsage is absolute usage as metrics.k8s.io reports it.
type realUsage struct {
	millicores int64
	memoryMB   int64
}

// exporterEndpoints maps a Kubernetes node name to its node-exporter URL. The
// chart runs the exporter with hostNetwork, so the pod IP is the node IP and
// the address is reachable without a Service per node.
func (c *Collector) exporterEndpoints(ctx context.Context) map[string]string {
	selector := c.ExporterLabels
	if selector == "" {
		selector = DefaultExporterLabels
	}
	sel, err := labels.Parse(selector)
	if err != nil {
		logf("node-exporter selector %q: %v", selector, err)
		return nil
	}
	pods := &corev1.PodList{}
	if err := c.Client.List(ctx, pods, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		logf("list node-exporter pods: %v", err)
		return nil
	}
	port := c.ExporterPort
	if port == 0 {
		port = DefaultExporterPort
	}
	out := map[string]string{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" || p.Status.PodIP == "" || p.Status.Phase != corev1.PodRunning {
			continue
		}
		if _, seen := out[p.Spec.NodeName]; seen {
			continue
		}
		out[p.Spec.NodeName] = fmt.Sprintf("http://%s:%d/metrics", p.Status.PodIP, port)
	}
	return out
}

// scrape reads one exporter. The timeout is short on purpose: a node that does
// not answer costs a dash in the table, not a slow page.
func (c *Collector) scrape(ctx context.Context, url string) (exporterSample, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return exporterSample{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return exporterSample{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return exporterSample{}, fmt.Errorf("%s: %s", url, resp.Status)
	}
	s := exporterSample{at: c.now(), fs: map[string]fsSample{}}
	if err := parseExporter(resp.Body, &s); err != nil {
		return s, err
	}
	return s, nil
}

// parseExporter keeps three series and ignores the rest of the page.
func parseExporter(r io.Reader, s *exporterSample) error {
	if s.fs == nil {
		s.fs = map[string]fsSample{}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		name := line[:sp]
		val, err := strconv.ParseFloat(strings.TrimSpace(line[sp+1:]), 64)
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(name, "node_cpu_seconds_total"):
			if !strings.Contains(name, "mode=\"idle\"") {
				continue
			}
			s.idle += val
			s.cpus++
		case strings.HasPrefix(name, "node_memory_MemAvailable_bytes"):
			s.avail = val
			s.haveMem = true
		case strings.HasPrefix(name, "node_memory_MemTotal_bytes"):
			s.total = val
		case strings.HasPrefix(name, "node_filesystem_size_bytes"):
			if m := wantedMount(name); m != "" {
				fs := s.fs[m]
				fs.size = val
				s.fs[m] = fs
			}
		case strings.HasPrefix(name, "node_filesystem_avail_bytes"):
			if m := wantedMount(name); m != "" {
				fs := s.fs[m]
				fs.avail = val
				s.fs[m] = fs
			}
		}
	}
	return sc.Err()
}

// wantedMount returns the mountpoint of a filesystem series when that
// mountpoint is one of the columns, and "" otherwise. Parsing the label by
// hand keeps the page free of a Prometheus client.
func wantedMount(line string) string {
	const key = "mountpoint=\""
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	m := rest[:j]
	for _, want := range diskMounts {
		if m == want {
			return m
		}
	}
	return ""
}

// diskShares turns the scrape into the three disk cells. A mount the exporter
// does not report keeps its column and shows a dash, so the table does not
// shift shape between nodes.
func diskShares(s exporterSample) []Disk {
	out := make([]Disk, 0, len(diskMounts))
	for _, m := range diskMounts {
		d := Disk{Mount: m}
		if fs, ok := s.fs[m]; ok && fs.size > 0 {
			used := fs.size - fs.avail
			if used < 0 {
				used = 0
			}
			pct := clampPercent(used / fs.size * 100)
			d.Percent = pct
			d.Level = Level(pct)
			d.SizeGB = math.Round(fs.size/(1<<30)*10) / 10
			d.UsedGB = math.Round(used/(1<<30)*10) / 10
		}
		out = append(out, d)
	}
	return out
}

// shares turns a scrape into percentages. CPU needs the previous sample of the
// same node, so the first pass after a restart reports memory only.
func (c *Collector) shares(node string, s exporterSample) (cpu, mem *float64) {
	c.mu.Lock()
	prev, had := c.last[node]
	if c.last == nil {
		c.last = map[string]exporterSample{}
	}
	c.last[node] = s
	c.mu.Unlock()

	if had && s.cpus > 0 && s.idle >= prev.idle {
		if dt := s.at.Sub(prev.at).Seconds(); dt > 0 {
			busy := 1 - (s.idle-prev.idle)/(dt*float64(s.cpus))
			cpu = clampPercent(busy * 100)
		}
	}
	if s.haveMem && s.total > 0 {
		mem = clampPercent((1 - s.avail/s.total) * 100)
	}
	return cpu, mem
}

func clampPercent(p float64) *float64 {
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return &p
}

// realUsage is the third column group: the exporters first, metrics.k8s.io
// second, a dash when neither answers.
func (c *Collector) realUsage(ctx context.Context, kn *corev1.Node, alloc Allocatable,
	exporters map[string]string, metrics map[string]realUsage) (Usage, string, []Disk) {

	if kn == nil {
		return Usage{}, "", diskShares(exporterSample{})
	}
	u := Usage{}
	source := ""
	if url, ok := exporters[kn.Name]; ok {
		s, err := c.scrape(ctx, url)
		if err == nil {
			cpu, mem := c.shares(kn.Name, s)
			if cpu != nil {
				u.CPU = Share{Percent: cpu, Level: Level(cpu)}
				u.Millicores = int64(*cpu / 100 * float64(alloc.Millicores))
			}
			if mem != nil {
				u.Memory = Share{Percent: mem, Level: Level(mem)}
				u.MemoryMB = int64(*mem / 100 * float64(alloc.MemoryMB))
			}
			if cpu != nil || mem != nil {
				source = "node-exporter"
			}
			disks := diskShares(s)
			return u, source, disks
		}
		logf("scrape %s: %v", url, err)
	}
	if source == "" {
		if m, ok := metrics[kn.Name]; ok {
			u.Millicores = m.millicores
			u.MemoryMB = m.memoryMB
			u.CPU = NewShare(m.millicores, alloc.Millicores)
			u.Memory = NewShare(m.memoryMB, alloc.MemoryMB)
			source = "metrics-server"
		}
	}
	return u, source, diskShares(exporterSample{})
}
