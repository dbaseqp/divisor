package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromoteSecondariesEnabled(t *testing.T) {
	root := t.TempDir()
	conf := filepath.Join(root, "conf")
	if err := os.Mkdir(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	// A sibling of conf. Joining ".." onto conf would read this file.
	if err := os.WriteFile(filepath.Join(root, "promote_secondaries"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(iface, content string) {
		t.Helper()
		dir := filepath.Join(conf, iface)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "promote_secondaries"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("eth0", "1\n")
	write("lo", "0\n")
	write("padded", " 1 \n")
	write("all", "1\n")
	write(filepath.Join("foo", "bar"), "1\n")

	cases := []struct {
		name    string
		iface   string
		want    bool
		wantErr bool
	}{
		{name: "enabled", iface: "eth0", want: true},
		{name: "disabled", iface: "lo", want: false},
		{name: "surrounding whitespace", iface: "padded", want: true},
		{name: "all", iface: "all", want: true},
		{name: "missing interface", iface: "no-such", want: false},
		{name: "dotdot", iface: "..", wantErr: true},
		{name: "dotdot component", iface: "../promote_secondaries", wantErr: true},
		{name: "slash stays inside", iface: "foo/bar", wantErr: true},
		{name: "slash and dotdot", iface: "foo/../eth0", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := promoteSecondariesEnabled(conf, tc.iface)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("enabled = %v, want an error for %q", got, tc.iface)
				}
				if !strings.Contains(err.Error(), tc.iface) {
					t.Fatalf("error %q does not mention %q", err, tc.iface)
				}
				return
			}
			if err != nil {
				t.Fatalf("promoteSecondariesEnabled: %v", err)
			}
			if got != tc.want {
				t.Fatalf("enabled = %v, want %v", got, tc.want)
			}
		})
	}
}
