package vcd

import (
	"context"
	"errors"
	"testing"

	"github.com/werton13/vcd-k8s-node-controller/internal/cloud"
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
			in:   "https://iaas.cloud.mts.ru/api",
			want: "https://iaas.cloud.mts.ru/api",
		},
		{
			name: "trailing slash",
			in:   "https://iaas.cloud.mts.ru/api/",
			want: "https://iaas.cloud.mts.ru/api",
		},
		{
			name: "no scheme",
			in:   "iaas.cloud.mts.ru",
			want: "https://iaas.cloud.mts.ru/api",
		},
		{
			name: "host only with scheme",
			in:   "https://iaas.cloud.mts.ru",
			want: "https://iaas.cloud.mts.ru/api",
		},
		{
			name: "http preserved",
			in:   "http://iaas.cloud.mts.ru/api",
			want: "http://iaas.cloud.mts.ru/api",
		},
		{
			name: "double scheme and api",
			in:   "https://https://iaas.cloud.mts.ru/api/api",
			want: "https://iaas.cloud.mts.ru/api",
		},
		{
			name: "triple scheme",
			in:   "https://https://https://iaas.cloud.mts.ru/api/api",
			want: "https://iaas.cloud.mts.ru/api",
		},
		{
			name: "repeated api",
			in:   "https://iaas.cloud.mts.ru/api/api/",
			want: "https://iaas.cloud.mts.ru/api",
		},
		{
			name:    "empty",
			in:      "",
			wantErr: true,
		},
		{
			name:    "unsupported scheme",
			in:      "ftp://iaas.cloud.mts.ru/api",
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
