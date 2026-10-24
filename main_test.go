package main

import (
	"math/rand"
	"testing"
)

func TestParseDesiredSubnets_SingleAndMultiple(t *testing.T) {
	subnets, err := parseDesiredSubnets("10.192.0.0/10")
	if err != nil {
		t.Fatalf("unexpected error for single CIDR: %v", err)
	}
	if len(subnets) != 1 || subnets[0].String() != "10.192.0.0/10" {
		t.Fatalf("unexpected parsed subnet: %+v", subnets)
	}

	multi, err := parseDesiredSubnets(" 10.10.0.0/16 , 172.16.50.0/24,192.168.200.5/32 ")
	if err != nil {
		t.Fatalf("unexpected error for multiple CIDRs: %v", err)
	}
	if len(multi) != 3 {
		t.Fatalf("expected 3 subnets, got %d", len(multi))
	}
	want := []string{"10.10.0.0/16", "172.16.50.0/24", "192.168.200.5/32"}
	for i, w := range want {
		if multi[i].String() != w {
			t.Errorf("subnet[%d] = %s, want %s", i, multi[i].String(), w)
		}
	}
}

func TestParseDesiredSubnets_Errors(t *testing.T) {
	cases := []string{
		"",
		"   ,  ",
		"10.10.0.0",
		"10.10.0.0/16,not-a-cidr",
		"2001:db8::/32",
	}
	for _, tc := range cases {
		if _, err := parseDesiredSubnets(tc); err == nil {
			t.Errorf("expected error for %q, got nil", tc)
		}
	}
}

func TestPickRandomAddress_AcrossMultipleSubnets(t *testing.T) {
	subnets, err := parseDesiredSubnets("10.10.0.0/16,172.16.50.0/24,192.168.200.42/32")
	if err != nil {
		t.Fatalf("parseDesiredSubnets failed: %v", err)
	}

	rng := rand.New(rand.NewSource(42))
	seenSubnet := make(map[string]bool)

	for i := 0; i < 200; i++ {
		ipNet, err := pickRandomAddress(subnets, rng)
		if err != nil {
			t.Fatalf("pickRandomAddress failed on iteration %d: %v", i, err)
		}

		matched := false
		for _, sn := range subnets {
			if sn.Contains(ipNet.IP) && sn.Mask.String() == ipNet.Mask.String() {
				matched = true
				seenSubnet[sn.String()] = true
				break
			}
		}
		if !matched {
			t.Fatalf("generated IPNet %s did not match any configured subnet", ipNet.String())
		}
	}

	if len(seenSubnet) != len(subnets) {
		t.Fatalf("expected all %d subnets to be sampled, got %v", len(subnets), seenSubnet)
	}
}

func TestPickRandomAddress_ExcludesNetworkAndBroadcast(t *testing.T) {
	subnets, err := parseDesiredSubnets("192.168.1.0/30")
	if err != nil {
		t.Fatalf("parseDesiredSubnets failed: %v", err)
	}
	if cap := totalSubnetsCapacity(subnets); cap != 2 {
		t.Fatalf("expected capacity 2 for /30, got %d", cap)
	}

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		ipNet, err := pickRandomAddress(subnets, rng)
		if err != nil {
			t.Fatalf("pickRandomAddress failed: %v", err)
		}
		got := ipNet.IP.String()
		if got == "192.168.1.0" || got == "192.168.1.3" {
			t.Fatalf("picked network or broadcast address %s in /30", got)
		}
	}
}

func TestGetDesiredSubnetsEnv(t *testing.T) {
	t.Setenv(EnvDesiredSubnet, "10.0.0.0/24")
	t.Setenv(EnvDesiredSubnets, "")
	if got := getDesiredSubnetsEnv(); got != "10.0.0.0/24" {
		t.Fatalf("got %q, want 10.0.0.0/24", got)
	}

	t.Setenv(EnvDesiredSubnets, "10.1.0.0/24,10.2.0.0/24")
	if got := getDesiredSubnetsEnv(); got != "10.1.0.0/24,10.2.0.0/24" {
		t.Fatalf("got %q, want 10.1.0.0/24,10.2.0.0/24", got)
	}
}
