package netpool

import (
	"slices"
	"testing"
)

func TestParseRange(t *testing.T) {
	from, to, err := ParseRange(" 172.23.252.50 - 172.23.252.99 ")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if from.String() != "172.23.252.50" || to.String() != "172.23.252.99" {
		t.Fatalf("ParseRange = %s-%s, want 172.23.252.50-172.23.252.99", from, to)
	}
	for _, bad := range []string{"", "172.23.252.50", "172.23.252.99-172.23.252.50", "nope-nope"} {
		if _, _, err := ParseRange(bad); err == nil {
			t.Errorf("ParseRange(%q) = nil error, want a rejection", bad)
		}
	}
}

func TestContainsIP(t *testing.T) {
	cidr := "172.23.252.0/24"
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"172.23.252.50", true},
		{"172.23.252.1", true},    // the gateway is a usable host; the CALLER excludes it
		{"172.23.252.0", false},   // network
		{"172.23.252.255", false}, // broadcast
		{"172.23.253.1", false},   // different subnet
	} {
		got, err := ContainsIP(cidr, tc.ip)
		if err != nil {
			t.Fatalf("ContainsIP(%s): %v", tc.ip, err)
		}
		if got != tc.want {
			t.Errorf("ContainsIP(%s, %s) = %v, want %v", cidr, tc.ip, got, tc.want)
		}
	}
	if _, err := ContainsIP("172.23.252.0/24", "not-an-ip"); err == nil {
		t.Error("ContainsIP with a junk address = nil error, want a rejection")
	}
}

// Allocation must be deterministic (lowest free first) so a retried admission converges on the
// same addresses, and must skip everything already spoken for.
func TestAllocateStaticSkipsUsedAndGateway(t *testing.T) {
	used := map[string]bool{"172.23.252.51": true, "172.23.252.52": true}
	got, err := AllocateStatic("172.23.252.50-172.23.252.60", "172.23.252.0/24", "172.23.252.50", used, 3)
	if err != nil {
		t.Fatalf("AllocateStatic: %v", err)
	}
	want := []string{"172.23.252.53", "172.23.252.54", "172.23.252.55"}
	if !slices.Equal(got, want) {
		t.Fatalf("AllocateStatic = %v, want %v (lowest free first, skipping the gateway and used addresses)", got, want)
	}
}

func TestAllocateStaticExhaustion(t *testing.T) {
	_, err := AllocateStatic("172.23.252.50-172.23.252.52", "172.23.252.0/24", "", nil, 4)
	if err == nil {
		t.Fatal("AllocateStatic over-subscribing a 3-address range = nil error, want an exhaustion error")
	}
}

// A range that falls outside the cluster subnet is a misconfiguration, not an allocation.
func TestAllocateStaticRejectsRangeOutsideSubnet(t *testing.T) {
	if _, err := AllocateStatic("10.0.0.10-10.0.0.20", "172.23.252.0/24", "", nil, 1); err == nil {
		t.Fatal("AllocateStatic with a range outside the subnet = nil error, want a rejection")
	}
}
