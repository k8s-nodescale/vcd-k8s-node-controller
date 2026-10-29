package mgmt

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// clusterName is the name kubeadm gave this cluster, shown under the page
// title. kubeadm publishes it in the kubeconfig it writes to
// kube-public/cluster-info; kube-system/kubeadm-config is the fallback. An
// empty answer just hides the line - the page works the same without it.
func (s *Server) clusterName(ctx context.Context) string {
	s.clusterOnce.Do(func() {
		s.cluster = s.readClusterName(ctx)
	})
	return s.cluster
}

func (s *Server) readClusterName(ctx context.Context) string {
	if s.Client == nil {
		return ""
	}

	info := &corev1.ConfigMap{}
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "kube-public", Name: "cluster-info"}, info); err == nil {
		var kubeconfig struct {
			Clusters []struct {
				Name string `json:"name"`
			} `json:"clusters"`
		}
		if err := yaml.Unmarshal([]byte(info.Data["kubeconfig"]), &kubeconfig); err == nil {
			for _, c := range kubeconfig.Clusters {
				if name := strings.TrimSpace(c.Name); name != "" {
					return name
				}
			}
		}
	}

	adm := &corev1.ConfigMap{}
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "kubeadm-config"}, adm); err == nil {
		var cfg struct {
			ClusterName string `json:"clusterName"`
		}
		if err := yaml.Unmarshal([]byte(adm.Data["ClusterConfiguration"]), &cfg); err == nil {
			return strings.TrimSpace(cfg.ClusterName)
		}
	}
	return ""
}
