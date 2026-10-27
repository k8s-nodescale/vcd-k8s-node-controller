package cloud

import (
	"strings"
	"testing"
)

type nopProvider struct{ Provider }

type nopFactory struct{}

func (nopFactory) For(Placement) (Provider, error) { return nopProvider{}, nil }

// A pool picks its platform by name; an unnamed provider is vCD, and an unknown
// name is an error that lists what is registered.
func TestRegistryDispatchesOnThePlacement(t *testing.T) {
	reg := Registry{ProviderVCD: nopFactory{}, ProviderVSphere: nopFactory{}}
	for _, provider := range []string{"", ProviderVCD, ProviderVSphere} {
		got, err := reg.For(Placement{Provider: provider})
		if err != nil {
			t.Fatalf("For(%q): %v", provider, err)
		}
		if got == nil {
			t.Fatalf("For(%q) returned no provider", provider)
		}
	}

	_, err := reg.For(Placement{Provider: "openstack"})
	if err == nil {
		t.Fatal("For accepted an unknown provider")
	}
	if !strings.Contains(err.Error(), "vcd, vsphere") {
		t.Fatalf("error does not list the registered providers: %v", err)
	}
}
