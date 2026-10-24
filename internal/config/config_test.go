package config

import "testing"

func TestDesiredSubnets(t *testing.T) {
	t.Setenv(EnvDesiredSubnet, "10.0.0.0/24")
	t.Setenv(EnvDesiredSubnets, "")
	if got := Load().DesiredSubnets; got != "10.0.0.0/24" {
		t.Fatalf("got %q, want 10.0.0.0/24", got)
	}

	t.Setenv(EnvDesiredSubnets, "10.1.0.0/24,10.2.0.0/24")
	if got := Load().DesiredSubnets; got != "10.1.0.0/24,10.2.0.0/24" {
		t.Fatalf("got %q, want 10.1.0.0/24,10.2.0.0/24", got)
	}
}

func TestDesiredSubnets_WhitespaceAndFallback(t *testing.T) {
	cases := []struct {
		name     string
		singular string
		plural   string
		want     string
	}{
		{name: "plural whitespace falls back", singular: "10.0.0.0/24", plural: "   ", want: "10.0.0.0/24"},
		{name: "singular is trimmed", singular: "  10.0.0.0/8  ", plural: "", want: "10.0.0.0/8"},
		{name: "plural is trimmed and wins", singular: "10.0.0.0/24", plural: " 10.1.0.0/24,10.2.0.0/24 ", want: "10.1.0.0/24,10.2.0.0/24"},
		{name: "both empty", singular: "", plural: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvDesiredSubnet, tc.singular)
			t.Setenv(EnvDesiredSubnets, tc.plural)
			if got := Load().DesiredSubnets; got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateMode(t *testing.T) {
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{mode: "", wantErr: false},
		{mode: ModeSNAT, wantErr: false},
		{mode: ModeDirect, wantErr: false},
		{mode: "bridge", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			err := (Config{Mode: tc.mode}).ValidateMode()
			if tc.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoadLeavesInterfaceNameUnset(t *testing.T) {
	t.Setenv(EnvInterfaceName, "")
	t.Setenv(EnvContainerPrefix, "")
	t.Setenv(EnvRedisAddr, "")
	cfg := Load()
	if cfg.InterfaceName != "" {
		t.Fatalf("interface name defaulted to %q", cfg.InterfaceName)
	}
	if cfg.ContainerPrefix != "" {
		t.Fatalf("container prefix defaulted to %q", cfg.ContainerPrefix)
	}
	if cfg.RedisAddr != DefaultRedisAddr {
		t.Fatalf("redis addr = %q", cfg.RedisAddr)
	}
	if cfg.InterfaceNameOrDefault() != DefaultInterfaceName {
		t.Fatalf("default interface = %q", cfg.InterfaceNameOrDefault())
	}
	if cfg.ContainerPrefixOrDefault() != DefaultContainerPrefix {
		t.Fatalf("default prefix = %q", cfg.ContainerPrefixOrDefault())
	}
}
