package ippool

import (
	"math/rand"
	"strings"
	"testing"
)

func TestParseDesiredSubnets_SingleAndMultiple(t *testing.T) {
	subnets, err := ParseDesiredSubnets("10.192.0.0/10")
	if err != nil {
		t.Fatalf("unexpected error for single CIDR: %v", err)
	}
	if len(subnets) != 1 || subnets[0].String() != "10.192.0.0/10" {
		t.Fatalf("unexpected parsed subnet: %+v", subnets)
	}

	multi, err := ParseDesiredSubnets(" 10.10.0.0/16 , 172.16.50.0/24,192.168.200.5/32 ")
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
		if _, err := ParseDesiredSubnets(tc); err == nil {
			t.Errorf("expected error for %q, got nil", tc)
		}
	}
}

func TestPickRandomAddress_AcrossMultipleSubnets(t *testing.T) {
	subnets, err := ParseDesiredSubnets("10.10.0.0/16,172.16.50.0/24,192.168.200.42/32")
	if err != nil {
		t.Fatalf("ParseDesiredSubnets failed: %v", err)
	}

	rng := rand.New(rand.NewSource(42))
	seenSubnet := make(map[string]bool)

	for i := 0; i < 200; i++ {
		ipNet, err := PickRandomAddress(subnets, rng)
		if err != nil {
			t.Fatalf("PickRandomAddress failed on iteration %d: %v", i, err)
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
	subnets, err := ParseDesiredSubnets("192.168.1.0/30")
	if err != nil {
		t.Fatalf("ParseDesiredSubnets failed: %v", err)
	}
	if cap := TotalSubnetsCapacity(subnets); cap != 2 {
		t.Fatalf("expected capacity 2 for /30, got %d", cap)
	}

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		ipNet, err := PickRandomAddress(subnets, rng)
		if err != nil {
			t.Fatalf("PickRandomAddress failed: %v", err)
		}
		got := ipNet.IP.String()
		if got == "192.168.1.0" || got == "192.168.1.3" {
			t.Fatalf("picked network or broadcast address %s in /30", got)
		}
	}
}

func TestParseDesiredSubnets_CanonicalizesHostAddresses(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{in: "10.1.2.3/16", want: []string{"10.1.0.0/16"}},
		{in: "10.1.2.200/25", want: []string{"10.1.2.128/25"}},
		{in: "192.0.2.11/31", want: []string{"192.0.2.10/31"}},
		{in: "10.0.0.0/24,", want: []string{"10.0.0.0/24"}},
		{in: "10.0.0.0/24,, 10.1.2.3/25,", want: []string{"10.0.0.0/24", "10.1.2.0/25"}},
		{in: "  192.0.2.1/32  ", want: []string{"192.0.2.1/32"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseDesiredSubnets(tc.in)
			if err != nil {
				t.Fatalf("ParseDesiredSubnets: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d subnets (%v), want %d", len(got), got, len(tc.want))
			}
			for i, want := range tc.want {
				if got[i].String() != want {
					t.Errorf("subnet[%d] = %s, want %s", i, got[i], want)
				}
			}
		})
	}
}

func TestTotalSubnetsCapacity(t *testing.T) {
	// Wider than /2 is rejected so a /0 cannot be counted as 2^32 addresses.
	cases := []struct {
		cidrs string
		want  int
	}{
		{cidrs: "192.0.2.5/32", want: 1},
		{cidrs: "192.0.2.0/31", want: 2},
		{cidrs: "192.0.2.0/30", want: 2},
		{cidrs: "192.0.2.0/29", want: 6},
		{cidrs: "192.0.2.0/24", want: 254},
		{cidrs: "10.0.0.0/16", want: 65534},
		{cidrs: "10.0.0.0/8", want: 16777214},
		{cidrs: "0.0.0.0/0", want: 0},
		{cidrs: "0.0.0.0/1", want: 0},
		{cidrs: "192.0.2.9/32,192.0.2.10/31,192.0.2.0/30", want: 5},
		{cidrs: "0.0.0.0/0,192.0.2.9/32", want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.cidrs, func(t *testing.T) {
			subnets, err := ParseDesiredSubnets(tc.cidrs)
			if err != nil {
				t.Fatalf("ParseDesiredSubnets: %v", err)
			}
			if got := TotalSubnetsCapacity(subnets); got != tc.want {
				t.Fatalf("capacity = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPickRandomAddress_StaysInUsableSet(t *testing.T) {
	cases := []struct {
		cidr   string
		usable []string
	}{
		{cidr: "192.0.2.9/32", usable: []string{"192.0.2.9"}},
		// /31 keeps both endpoints (RFC 3021), including when the input host bit is set.
		{cidr: "192.0.2.11/31", usable: []string{"192.0.2.10", "192.0.2.11"}},
		{cidr: "192.0.2.5/30", usable: []string{"192.0.2.5", "192.0.2.6"}},
		{cidr: "10.1.2.9/29", usable: []string{"10.1.2.9", "10.1.2.10", "10.1.2.11", "10.1.2.12", "10.1.2.13", "10.1.2.14"}},
	}
	for _, tc := range cases {
		t.Run(tc.cidr, func(t *testing.T) {
			subnets, err := ParseDesiredSubnets(tc.cidr)
			if err != nil {
				t.Fatalf("ParseDesiredSubnets: %v", err)
			}
			allowed := make(map[string]bool, len(tc.usable))
			for _, ip := range tc.usable {
				allowed[ip] = true
			}
			seen := make(map[string]bool, len(tc.usable))
			rng := rand.New(rand.NewSource(1))
			for i := 0; i < 200; i++ {
				gotNet, err := PickRandomAddress(subnets, rng)
				if err != nil {
					t.Fatalf("pick %d: %v", i, err)
				}
				if !subnets[0].Contains(gotNet.IP) {
					t.Fatalf("picked %s outside %s", gotNet.IP, subnets[0])
				}
				if gotNet.Mask.String() != subnets[0].Mask.String() {
					t.Fatalf("mask = %s, want %s", gotNet.Mask, subnets[0].Mask)
				}
				got := gotNet.IP.String()
				if !allowed[got] {
					t.Fatalf("picked %s, usable set is %v", got, tc.usable)
				}
				seen[got] = true
			}
			if len(seen) != len(allowed) {
				t.Fatalf("saw %v, want every address in %v", seen, tc.usable)
			}
		})
	}
}

func TestPickRandomAddress_DeterministicForSeed(t *testing.T) {
	subnets, err := ParseDesiredSubnets("10.0.0.0/24,192.0.2.0/30")
	if err != nil {
		t.Fatalf("ParseDesiredSubnets: %v", err)
	}
	collect := func(seed int64) []string {
		rng := rand.New(rand.NewSource(seed))
		got := make([]string, 0, 16)
		for i := 0; i < 16; i++ {
			ipNet, err := PickRandomAddress(subnets, rng)
			if err != nil {
				t.Fatalf("pick %d: %v", i, err)
			}
			got = append(got, ipNet.String())
		}
		return got
	}
	first := collect(42)
	second := collect(42)
	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Fatalf("same seed produced %v and %v", first, second)
	}
	other := collect(7)
	if strings.Join(first, ",") == strings.Join(other, ",") {
		t.Fatalf("different seeds both produced %v", first)
	}
}

func TestPickRandomAddress_RejectsUnusablePools(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	if _, err := PickRandomAddress(nil, rng); err == nil {
		t.Fatal("expected error for no subnets")
	}

	for _, cidr := range []string{"0.0.0.0/0", "0.0.0.0/1", "10.0.0.0/0"} {
		t.Run(cidr, func(t *testing.T) {
			subnets, err := ParseDesiredSubnets(cidr)
			if err != nil {
				t.Fatalf("ParseDesiredSubnets: %v", err)
			}
			if _, err := PickRandomAddress(subnets, rng); err == nil {
				t.Fatal("expected unsupported prefix error")
			}
		})
	}
}

func TestGetUnusedAddress_FailsWhenPoolIsExhausted(t *testing.T) {
	cases := []struct {
		cidr     string
		assigned []string
	}{
		{cidr: "192.0.2.10/32", assigned: []string{"192.0.2.10"}},
		{cidr: "192.0.2.10/31", assigned: []string{"192.0.2.10", "192.0.2.11"}},
		{cidr: "192.0.2.0/30", assigned: []string{"192.0.2.1", "192.0.2.2"}},
	}
	for _, tc := range cases {
		t.Run(tc.cidr, func(t *testing.T) {
			subnets, err := ParseDesiredSubnets(tc.cidr)
			if err != nil {
				t.Fatal(err)
			}
			assigned := make(map[string]bool, len(tc.assigned))
			for _, ip := range tc.assigned {
				assigned[ip] = true
			}
			_, err = GetUnusedAddress(subnets, assigned)
			if err == nil || !strings.Contains(err.Error(), "unable to find an unused IP address") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestGetUnusedAddress_RejectsEmptyOrUnsupportedPool(t *testing.T) {
	if _, err := GetUnusedAddress(nil, map[string]bool{}); err == nil || !strings.Contains(err.Error(), "no desired subnets") {
		t.Fatalf("empty pool: got %v", err)
	}

	subnets, err := ParseDesiredSubnets("0.0.0.0/0")
	if err != nil {
		t.Fatal(err)
	}
	_, err = GetUnusedAddress(subnets, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "unsupported prefix") {
		t.Fatalf("got %v", err)
	}
}
