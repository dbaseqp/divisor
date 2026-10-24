package events

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"divisor/internal/config"
)

const sampleRoundID = "6f1c0c3e-9b1a-4e0a-9c2a-1b7e5d4a8c11"

func TestOpenSelectsBackend(t *testing.T) {
	redisSource, err := Open(config.Config{Backend: config.BackendRedis})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := redisSource.(*Redis); !ok {
		t.Fatalf("got %T", redisSource)
	}

	natsSource, err := Open(config.Config{Backend: config.BackendNATS})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := natsSource.(*NATS); !ok {
		t.Fatalf("got %T", natsSource)
	}

	if _, err := Open(config.Config{Backend: "kafka"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestDecodeRoundNotice(t *testing.T) {
	cases := []struct {
		name         string
		payload      string
		wantID       string
		wantClosedAt string
		wantErr      string
	}{
		{
			name:         "uuid and rfc3339",
			payload:      `{"round_id":"` + sampleRoundID + `","closed_at":"2026-10-24T22:00:00Z"}`,
			wantID:       sampleRoundID,
			wantClosedAt: "2026-10-24T22:00:00Z",
		},
		{
			name:         "extra field",
			payload:      `{"round_id":"` + sampleRoundID + `","closed_at":"2026-10-24T22:00:00+00:00","note":"extra"}`,
			wantID:       sampleRoundID,
			wantClosedAt: "2026-10-24T22:00:00+00:00",
		},
		{
			name:    "id only",
			payload: `{"round_id":"` + sampleRoundID + `"}`,
			wantID:  sampleRoundID,
		},
		{
			name:         "missing id",
			payload:      `{"closed_at":"2026-10-24T22:00:00Z"}`,
			wantClosedAt: "2026-10-24T22:00:00Z",
		},
		{
			name:         "null id",
			payload:      `{"round_id":null,"closed_at":"2026-10-24T22:00:00Z"}`,
			wantClosedAt: "2026-10-24T22:00:00Z",
		},
		{
			name:         "empty id",
			payload:      `{"round_id":"","closed_at":"2026-10-24T22:00:00Z"}`,
			wantClosedAt: "2026-10-24T22:00:00Z",
		},
		{
			name:         "numeric id keeps closed_at",
			payload:      `{"round_id":12,"closed_at":"2026-10-24T22:00:00Z"}`,
			wantClosedAt: "2026-10-24T22:00:00Z",
			wantErr:      "round_id",
		},
		{
			name:         "non rfc3339 time is kept",
			payload:      `{"round_id":"` + sampleRoundID + `","closed_at":"yesterday"}`,
			wantID:       sampleRoundID,
			wantClosedAt: "yesterday",
		},
		{
			name:    "id kept when time is not a string",
			payload: `{"round_id":"` + sampleRoundID + `","closed_at":12}`,
			wantID:  sampleRoundID,
			wantErr: "closed_at",
		},
		{
			name:    "not json",
			payload: `round_finish`,
			wantErr: "malformed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeRoundNotice([]byte(tc.payload))
			if tc.wantErr != "" {
				if got.DecodeErr == nil || !strings.Contains(got.DecodeErr.Error(), tc.wantErr) {
					t.Fatalf("got %v, want error containing %q", got.DecodeErr, tc.wantErr)
				}
			} else if got.DecodeErr != nil {
				t.Fatal(got.DecodeErr)
			}
			if got.RoundID != tc.wantID {
				t.Fatalf("round_id = %q, want %q", got.RoundID, tc.wantID)
			}
			if got.ClosedAt != tc.wantClosedAt {
				t.Fatalf("closed_at = %q, want %q", got.ClosedAt, tc.wantClosedAt)
			}
		})
	}
}

func TestNATSMessageTriggersReconfigure(t *testing.T) {
	ns := startNATS(t)
	subject := "scoring.rounds.closed"
	t.Setenv(config.EnvNATSURL, ns.ClientURL())
	t.Setenv(config.EnvNATSRoundClosedSubject, subject)

	baseSubs := ns.NumSubscriptions()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var calls int
	var roundErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := NewNATS().Listen(ctx, func() error {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				return errReconfigure
			}
			return nil
		})
		if err != nil && ctx.Err() == nil {
			mu.Lock()
			roundErr = err
			mu.Unlock()
		}
	}()

	waitForMoreSubscriptions(t, ns, baseSubs)

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	publish := func(body string) {
		t.Helper()
		if err := nc.Publish(subject, []byte(body)); err != nil {
			t.Fatal(err)
		}
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}
	}

	publish(`{"round_id":"` + sampleRoundID + `","closed_at":"2026-10-24T22:00:00Z"}`)
	waitCalls(t, &mu, &calls, 1)

	// The message arriving is the signal. A body that does not match the
	// contract is logged and still reconfigures.
	publish(`not-json`)
	waitCalls(t, &mu, &calls, 2)
	publish(`{"closed_at":"2026-10-24T22:00:00Z"}`)
	waitCalls(t, &mu, &calls, 3)
	publish(`{"round_id":12,"closed_at":"2026-10-24T22:00:00Z"}`)
	waitCalls(t, &mu, &calls, 4)

	publish(`{"round_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","closed_at":"2026-10-24T22:01:00Z","note":"extra"}`)
	waitCalls(t, &mu, &calls, 5)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return after cancel")
	}
	mu.Lock()
	err = roundErr
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

var errReconfigure = reconfigureError{}

type reconfigureError struct{}

func (reconfigureError) Error() string { return "reconfigure failed" }

func startNATS(t *testing.T) *server.Server {
	t.Helper()
	opts := &server.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	return ns
}

func waitForMoreSubscriptions(t *testing.T, ns *server.Server, base uint32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ns.NumSubscriptions() > base {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscriptions = %d, want more than %d", ns.NumSubscriptions(), base)
}

func waitCalls(t *testing.T, mu *sync.Mutex, calls *int, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := *calls
		mu.Unlock()
		if got >= want {
			if got != want {
				t.Fatalf("calls = %d, want %d", got, want)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := *calls
	mu.Unlock()
	t.Fatalf("calls = %d, want %d", got, want)
}
