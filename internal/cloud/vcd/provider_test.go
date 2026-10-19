package vcd

import "testing"

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
