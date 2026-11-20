package network

import (
	"errors"
	"strings"
	"testing"

	"github.com/moby/moby/libnetwork/iptables"
)

// fakeTable records iptables calls in order.
type fakeTable struct {
	listing  string
	ops      []string
	failNext bool
}

func (f *fakeTable) Raw(args ...string) ([]byte, error) {
	cmd := strings.Join(args, " ")
	if cmd == "-t nat -S POSTROUTING" {
		return []byte(f.listing), nil
	}
	f.ops = append(f.ops, "raw "+cmd)
	return nil, nil
}

func (f *fakeTable) ProgramRule(table iptables.Table, chain string, action iptables.Action, args []string) error {
	if f.failNext {
		return errors.New("iptables failed")
	}
	f.ops = append(f.ops, string(action)+" "+string(table)+" "+chain+" "+strings.Join(args, " "))
	return nil
}

const postrouting = `-P POSTROUTING ACCEPT
-A POSTROUTING -s 172.18.0.5/32 -d 10.100.0.0/16 -j SNAT --to-source 10.192.1.1 --random-fully
-A POSTROUTING -s 172.18.0.6/32 -d 10.100.0.0/16 -j SNAT --to-source 10.192.1.1 --random-fully
-A POSTROUTING -s 172.18.0.0/16 ! -o br-x -j MASQUERADE
`

func withTable(t *testing.T, f *fakeTable) {
	t.Helper()
	orig := natTable
	natTable = func() ruleTable { return f }
	t.Cleanup(func() { natTable = orig })
}

// A rotation must never leave a moment with no SNAT rule for a runner: the
// new rules go in before the old ones come out.
func TestConfigureNATRulesInsertsBeforeDeleting(t *testing.T) {
	f := &fakeTable{listing: postrouting}
	withTable(t, f)

	if err := configureNATRules([]string{"10.192.9.9"}, []string{"172.18.0.5", "172.18.0.6"}, []string{"10.100.0.0/16"}); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"-I nat POSTROUTING -s 172.18.0.5 -d 10.100.0.0/16 -j SNAT --to-source 10.192.9.9 --random-fully",
		"-I nat POSTROUTING -s 172.18.0.6 -d 10.100.0.0/16 -j SNAT --to-source 10.192.9.9 --random-fully",
		"raw -t nat -D POSTROUTING -s 172.18.0.5/32 -d 10.100.0.0/16 -j SNAT --to-source 10.192.1.1 --random-fully",
		"raw -t nat -D POSTROUTING -s 172.18.0.6/32 -d 10.100.0.0/16 -j SNAT --to-source 10.192.1.1 --random-fully",
	}
	if strings.Join(f.ops, "\n") != strings.Join(want, "\n") {
		t.Fatalf("iptables calls:\n%s\nwant:\n%s", strings.Join(f.ops, "\n"), strings.Join(want, "\n"))
	}
}

// If the new rules can't be installed, the old ones stay: the runners keep
// the addresses they had instead of losing SNAT altogether.
func TestConfigureNATRulesKeepsOldRulesWhenInsertFails(t *testing.T) {
	f := &fakeTable{listing: postrouting, failNext: true}
	withTable(t, f)

	if err := configureNATRules([]string{"10.192.9.9"}, []string{"172.18.0.5"}, []string{"10.100.0.0/16"}); err == nil {
		t.Fatal("expected an error")
	}
	for _, op := range f.ops {
		if strings.Contains(op, " -D ") {
			t.Fatalf("old rule deleted after a failed insert: %s", op)
		}
	}
}
