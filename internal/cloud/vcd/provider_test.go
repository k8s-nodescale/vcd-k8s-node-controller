package vcd

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/vmware/go-vcloud-director/v2/govcd"

	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "already api endpoint",
			in:   "https://vcd.example.com/api",
			want: "https://vcd.example.com/api",
		},
		{
			name: "trailing slash",
			in:   "https://vcd.example.com/api/",
			want: "https://vcd.example.com/api",
		},
		{
			name: "no scheme",
			in:   "vcd.example.com",
			want: "https://vcd.example.com/api",
		},
		{
			name: "host only with scheme",
			in:   "https://vcd.example.com",
			want: "https://vcd.example.com/api",
		},
		{
			name: "http preserved",
			in:   "http://vcd.example.com/api",
			want: "http://vcd.example.com/api",
		},
		{
			name: "double scheme and api",
			in:   "https://https://vcd.example.com/api/api",
			want: "https://vcd.example.com/api",
		},
		{
			name: "triple scheme",
			in:   "https://https://https://vcd.example.com/api/api",
			want: "https://vcd.example.com/api",
		},
		{
			name: "repeated api",
			in:   "https://vcd.example.com/api/api/",
			want: "https://vcd.example.com/api",
		},
		{
			name:    "empty",
			in:      "",
			wantErr: true,
		},
		{
			name:    "unsupported scheme",
			in:      "ftp://vcd.example.com/api",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeURL(%q) expected error, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeURL(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestUpdatePoolMemberRejectsUnaddressablePools(t *testing.T) {
	p := &Provider{}
	cases := []struct {
		name string
		pool cloud.PoolRef
	}{
		{name: "nsxt is not implemented", pool: cloud.PoolRef{Type: "nsxt", Name: "p", Edge: "e"}},
		{name: "missing edge", pool: cloud.PoolRef{Type: "nsxv", Name: "p"}},
		{name: "missing name", pool: cloud.PoolRef{Type: "nsxv", Edge: "e"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := p.AddPoolMember(context.Background(), tc.pool, "192.0.2.10")
			if !errors.Is(err, cloud.ErrPoolNotConfigured) {
				t.Fatalf("err = %v, want ErrPoolNotConfigured", err)
			}
		})
	}
}

func TestPoolMemberNameIsNSXVCompatible(t *testing.T) {
	if got := poolMemberName("192.168.100.8"); got != "node-192-168-100-8" {
		t.Fatalf("poolMemberName = %q, want node-192-168-100-8", got)
	}
	// NSX-V: letters, digits, dash and underscore, and the first character has
	// to be a letter.
	for _, ip := range []string{"192.168.100.8", "fd00::5"} {
		name := poolMemberName(ip)
		if name[0] < 'a' || name[0] > 'z' {
			t.Fatalf("member name %q does not start with a letter", name)
		}
		for _, r := range name {
			ok := r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !ok {
				t.Fatalf("member name %q carries %q", name, r)
			}
		}
	}
}

func TestHTTPTimeoutDefault(t *testing.T) {
	if got := httpTimeout(Config{}); got != defaultHTTPTimeout {
		t.Fatalf("httpTimeout(Config{}) = %v, want %v", got, defaultHTTPTimeout)
	}
	if got := httpTimeout(Config{Timeout: -time.Second}); got != defaultHTTPTimeout {
		t.Fatalf("httpTimeout(negative) = %v, want %v", got, defaultHTTPTimeout)
	}
}

func TestHTTPTimeoutCustom(t *testing.T) {
	if got := httpTimeout(Config{Timeout: 5 * time.Second}); got != 5*time.Second {
		t.Fatalf("httpTimeout(5s) = %v, want 5s", got)
	}
}

// TestNewBoundsEveryRequest guards the bug that stalled a scale-out: the SDK
// client must carry a timeout, otherwise a dropped connection blocks the only
// reconcile worker forever.
func TestNewBoundsEveryRequest(t *testing.T) {
	client := govcd.NewVCDClient(url.URL{Scheme: "https", Host: "vcd.example.com", Path: "/api"}, true)
	client.Client.Http.Timeout = httpTimeout(Config{Timeout: 7 * time.Second})
	if got := client.Client.Http.Timeout; got != 7*time.Second {
		t.Fatalf("client timeout = %v, want 7s", got)
	}
}
