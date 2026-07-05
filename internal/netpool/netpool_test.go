package netpool

import (
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

func cluster(cidr string, deleted bool) *domain.Cluster {
	c := &domain.Cluster{NetworkCIDR: cidr}
	if deleted {
		t := c.CreatedAt // zero time is fine; just needs to be non-nil
		c.DeletedAt = &t
	}
	return c
}

func TestAllocateAutoPicksFirstFreeBlock(t *testing.T) {
	got, err := Allocate(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.200.0.0/24" {
		t.Fatalf("first auto CIDR = %q, want 10.200.0.0/24", got)
	}
}

func TestAllocateAutoSkipsUsed(t *testing.T) {
	existing := []*domain.Cluster{cluster("10.200.0.0/24", false), cluster("10.200.1.0/24", false)}
	got, err := Allocate(existing, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.200.2.0/24" {
		t.Fatalf("auto CIDR = %q, want 10.200.2.0/24 (first two taken)", got)
	}
}

func TestAllocateAutoReusesDeletedClusterBlock(t *testing.T) {
	existing := []*domain.Cluster{cluster("10.200.0.0/24", true)} // deleted → not in use
	got, err := Allocate(existing, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.200.0.0/24" {
		t.Fatalf("auto CIDR = %q, want the deleted cluster's freed block", got)
	}
}

func TestAllocateManualCanonicalises(t *testing.T) {
	got, err := Allocate(nil, "10.50.7.42/24")
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.50.7.0/24" {
		t.Fatalf("manual CIDR = %q, want canonical network 10.50.7.0/24", got)
	}
}

func TestAllocateManualRejectsReservedOverlap(t *testing.T) {
	// 10.96.0.0/12 is the service CIDR - reserved.
	if _, err := Allocate(nil, "10.96.5.0/24"); err == nil {
		t.Fatal("expected overlap-with-reserved rejection, got nil")
	}
}

func TestAllocateManualRejectsClusterOverlap(t *testing.T) {
	existing := []*domain.Cluster{cluster("10.30.0.0/16", false)}
	if _, err := Allocate(existing, "10.30.4.0/24"); err == nil {
		t.Fatal("expected overlap-with-cluster rejection, got nil")
	}
}

func TestAllocateManualRejectsBadPrefix(t *testing.T) {
	if _, err := Allocate(nil, "10.40.0.0/30"); err == nil { // /30 is smaller than maxPrefix
		t.Fatal("expected out-of-range prefix rejection, got nil")
	}
	if _, err := Allocate(nil, "10.40.0.0/8"); err == nil { // /8 is bigger than minPrefix
		t.Fatal("expected out-of-range prefix rejection, got nil")
	}
}

func TestAllocateManualRejectsGarbage(t *testing.T) {
	if _, err := Allocate(nil, "not-a-cidr"); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestVIPIsHighHost(t *testing.T) {
	got, err := VIP("10.200.3.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.200.3.250" { // 256 - 6
		t.Fatalf("VIP = %q, want 10.200.3.250", got)
	}
}

func TestVIPWithinTinySubnet(t *testing.T) {
	got, err := VIP("10.200.3.0/28") // 16 addresses; last usable is .14
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.200.3.10" { // 16 - 6
		t.Fatalf("VIP = %q, want 10.200.3.10", got)
	}
}

func TestLoadBalancerIPIsHighHostBelowVIP(t *testing.T) {
	lb, err := LoadBalancerIP("10.200.3.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if lb != "10.200.3.249" { // 256 - 7, one below the VIP (.250)
		t.Fatalf("LoadBalancerIP = %q, want 10.200.3.249", lb)
	}
}

// The reserved LB address must never collide with the HA VIP, in any subnet size (including the
// tiny-subnet fallback branch), since a cluster reserves both.
func TestLoadBalancerIPNeverEqualsVIP(t *testing.T) {
	for _, cidr := range []string{"10.200.3.0/24", "10.200.3.0/26", "10.200.3.0/27", "10.200.3.0/28"} {
		vip, err := VIP(cidr)
		if err != nil {
			t.Fatalf("VIP(%s): %v", cidr, err)
		}
		lb, err := LoadBalancerIP(cidr)
		if err != nil {
			t.Fatalf("LoadBalancerIP(%s): %v", cidr, err)
		}
		if lb == vip {
			t.Fatalf("%s: LoadBalancerIP %q collides with VIP", cidr, lb)
		}
	}
}

func TestGatewayIsFirstHost(t *testing.T) {
	got, err := Gateway("10.200.3.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.200.3.1" {
		t.Fatalf("gateway = %q, want 10.200.3.1", got)
	}
}
