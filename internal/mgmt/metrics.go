package mgmt

import (
	"context"
	"encoding/json"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// metricsGroupVersion is the addon's API. It is a plain REST read instead of a
// typed client so the sidecar does not depend on k8s.io/metrics: one endpoint,
// one shape.
var metricsGroupVersion = schema.GroupVersion{Group: "metrics.k8s.io", Version: "v1beta1"}

// MetricsAPI is metrics.k8s.io, reduced to the one question the page asks. The
// sidecar does not require it: without the addon the exporters answer alone.
type MetricsAPI interface {
	NodeUsage(ctx context.Context) (map[string]realUsage, error)
}

// NewMetricsClient builds the client. It never fails on a missing addon - the
// first call does, and the collector then leaves the column empty.
func NewMetricsClient(cfg *rest.Config) MetricsAPI {
	c := rest.CopyConfig(cfg)
	c.APIPath = "/apis"
	c.GroupVersion = &metricsGroupVersion
	c.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	rc, err := rest.RESTClientFor(c)
	if err != nil {
		logf("metrics.k8s.io client: %v", err)
		return nil
	}
	return &metricsREST{rc: rc}
}

// metricsUsage asks the addon once per refresh.
func (c *Collector) metricsUsage(ctx context.Context) map[string]realUsage {
	if c.Metrics == nil {
		return nil
	}
	out, err := c.Metrics.NodeUsage(ctx)
	if err != nil {
		logf("metrics.k8s.io: %v", err)
		return nil
	}
	return out
}

type metricsREST struct{ rc rest.Interface }

// NodeUsage reads /apis/metrics.k8s.io/v1beta1/nodes.
func (m *metricsREST) NodeUsage(ctx context.Context) (map[string]realUsage, error) {
	raw, err := m.rc.Get().AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").Do(ctx).Raw()
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Usage struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make(map[string]realUsage, len(list.Items))
	for _, it := range list.Items {
		cpu, err := resource.ParseQuantity(it.Usage.CPU)
		if err != nil {
			continue
		}
		mem, err := resource.ParseQuantity(it.Usage.Memory)
		if err != nil {
			continue
		}
		out[it.Metadata.Name] = realUsage{
			millicores: cpu.MilliValue(),
			memoryMB:   mem.Value() / (1 << 20),
		}
	}
	return out, nil
}
