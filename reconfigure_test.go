package main

import (
	"strings"
	"testing"
)

func TestNormalizeSNATSubnets(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []string
		wantErr string
	}{
		{
			name: "trims and drops blanks",
			in:   []string{" 10.0.0.0/8 ", "", "  ", "10.1.0.0/16"},
			want: []string{"10.0.0.0/8", "10.1.0.0/16"},
		},
		{
			name: "only blanks",
			in:   []string{"", "   "},
			want: []string{},
		},
		{
			name:    "invalid cidr",
			in:      []string{"10.0.0.0/8", "nope"},
			wantErr: "nope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeSNATSubnets(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeSNATSubnets: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("subnet[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseRouteDestinations(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []string
		wantErr string
	}{
		{
			name: "canonicalizes host bits",
			in:   []string{" 10.1.2.3/16 ", "192.0.2.11/31"},
			want: []string{"10.1.0.0/16", "192.0.2.10/31"},
		},
		{
			name:    "blank entry",
			in:      []string{"10.0.0.0/8", " "},
			wantErr: "failed to parse target subnet",
		},
		{
			name:    "invalid cidr",
			in:      []string{"nope"},
			wantErr: "nope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRouteDestinations(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRouteDestinations: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d destinations, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i].String() != tc.want[i] {
					t.Errorf("destination[%d] = %s, want %s", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestConfigureNATRules_RejectsBadInputBeforeIptables(t *testing.T) {
	t.Run("invalid subnet", func(t *testing.T) {
		err := configureNATRules([]string{"192.0.2.10"}, []string{"172.17.0.2"}, []string{"not-a-cidr"})
		if err == nil || !strings.Contains(err.Error(), "not-a-cidr") {
			t.Fatalf("got %v", err)
		}
		if strings.Contains(err.Error(), "iptables") || strings.Contains(err.Error(), "failed to list rules") {
			t.Fatalf("iptables was invoked: %v", err)
		}
	})

	t.Run("no source addresses", func(t *testing.T) {
		err := configureNATRules(nil, []string{"172.17.0.2"}, []string{"10.0.0.0/8", " "})
		if err == nil || !strings.Contains(err.Error(), "no source addresses") {
			t.Fatalf("got %v", err)
		}
		if strings.Contains(err.Error(), "iptables") || strings.Contains(err.Error(), "failed to list rules") {
			t.Fatalf("iptables was invoked: %v", err)
		}
	})
}

func TestHandleNetworkReconfiguration_ValidatesBeforeNetworkChanges(t *testing.T) {
	base := map[string]string{
		EnvNumIPs:              "1",
		EnvDesiredSubnet:       "192.0.2.0/24",
		EnvDesiredSubnets:      "",
		EnvTargetSubnets:       "10.0.0.0/8",
		EnvMode:                ModeSNAT,
		EnvInterfaceName:       "divisor-test-unused",
		EnvTargetSubnetGateway: "",
	}
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "zero ips",
			env:  map[string]string{EnvNumIPs: "0"},
			want: "positive",
		},
		{
			name: "negative ips",
			env:  map[string]string{EnvNumIPs: "-2"},
			want: "positive",
		},
		{
			name: "snat trailing comma still rejects zero ips",
			env: map[string]string{
				EnvNumIPs:        "0",
				EnvTargetSubnets: "10.0.0.0/8, ",
			},
			want: "positive",
		},
		{
			name: "exceeds capacity",
			env: map[string]string{
				EnvNumIPs:        "3",
				EnvDesiredSubnet: "192.0.2.0/30",
			},
			want: "exceeds total usable",
		},
		{
			name: "invalid desired subnet",
			env:  map[string]string{EnvDesiredSubnet: "not-a-cidr"},
			want: "not-a-cidr",
		},
		{
			name: "invalid snat target",
			env: map[string]string{
				EnvMode:          ModeSNAT,
				EnvTargetSubnets: "nope",
			},
			want: "nope",
		},
		{
			name: "invalid direct target",
			env: map[string]string{
				EnvMode:          ModeDirect,
				EnvTargetSubnets: "nope",
			},
			want: "nope",
		},
		{
			name: "direct blank target",
			env: map[string]string{
				EnvMode:          ModeDirect,
				EnvTargetSubnets: "10.0.0.0/8, ",
			},
			want: "failed to parse target subnet",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range base {
				t.Setenv(key, value)
			}
			for key, value := range tc.env {
				t.Setenv(key, value)
			}

			err := handleNetworkReconfiguration()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "failed to add link") {
				t.Fatalf("network was changed before validation: %v", err)
			}
		})
	}
}
