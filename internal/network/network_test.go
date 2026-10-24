package network

import (
	"strings"
	"testing"

	"divisor/internal/config"
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

func TestReconfigure_RejectsNonNumericNumIPs(t *testing.T) {
	t.Setenv(config.EnvNumIPs, "many")
	t.Setenv(config.EnvDesiredSubnet, "192.0.2.0/24")
	t.Setenv(config.EnvDesiredSubnets, "")
	t.Setenv(config.EnvTargetSubnets, "10.0.0.0/8")
	t.Setenv(config.EnvMode, config.ModeSNAT)
	t.Setenv(config.EnvInterfaceName, "divisor-test-unused")

	err := Reconfigure()
	if err == nil || !strings.Contains(err.Error(), "NUM_IPS") {
		t.Fatalf("got %v", err)
	}
}

func TestReconfigure_ValidatesBeforeNetworkChanges(t *testing.T) {
	base := map[string]string{
		config.EnvNumIPs:              "1",
		config.EnvDesiredSubnet:       "192.0.2.0/24",
		config.EnvDesiredSubnets:      "",
		config.EnvTargetSubnets:       "10.0.0.0/8",
		config.EnvMode:                config.ModeSNAT,
		config.EnvInterfaceName:       "divisor-test-unused",
		config.EnvTargetSubnetGateway: "",
	}
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "zero ips",
			env:  map[string]string{config.EnvNumIPs: "0"},
			want: "positive",
		},
		{
			name: "negative ips",
			env:  map[string]string{config.EnvNumIPs: "-2"},
			want: "positive",
		},
		{
			name: "snat trailing comma still rejects zero ips",
			env: map[string]string{
				config.EnvNumIPs:        "0",
				config.EnvTargetSubnets: "10.0.0.0/8, ",
			},
			want: "positive",
		},
		{
			name: "exceeds capacity",
			env: map[string]string{
				config.EnvNumIPs:        "3",
				config.EnvDesiredSubnet: "192.0.2.0/30",
			},
			want: "exceeds total usable",
		},
		{
			name: "invalid desired subnet",
			env:  map[string]string{config.EnvDesiredSubnet: "not-a-cidr"},
			want: "not-a-cidr",
		},
		{
			name: "invalid snat target",
			env: map[string]string{
				config.EnvMode:          config.ModeSNAT,
				config.EnvTargetSubnets: "nope",
			},
			want: "nope",
		},
		{
			name: "invalid direct target",
			env: map[string]string{
				config.EnvMode:          config.ModeDirect,
				config.EnvTargetSubnets: "nope",
			},
			want: "nope",
		},
		{
			name: "direct blank target",
			env: map[string]string{
				config.EnvMode:          config.ModeDirect,
				config.EnvTargetSubnets: "10.0.0.0/8, ",
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

			err := Reconfigure()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "failed to add link") {
				t.Fatalf("network was changed before validation: %v", err)
			}
		})
	}
}
