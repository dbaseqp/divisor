package events

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"

	"divisor/internal/config"
)

const (
	redisEventsChannel = "events"
	redisRoundFinish   = "round_finish"
)

// Redis subscribes to Quotient's events channel and reacts to round_finish.
type Redis struct{}

// NewRedis returns the Redis event source.
func NewRedis() *Redis {
	return &Redis{}
}

var _ Source = (*Redis)(nil)

// Listen connects, then calls onRound for each round_finish payload.
func (r *Redis) Listen(ctx context.Context, onRound func() error) error {
	slog.Debug("Connecting to Redis...")
	redisClient, err := connectToRedis(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}
	slog.Debug("Connected to Redis")

	events := redisClient.Subscribe(ctx, redisEventsChannel)
	defer events.Close()
	eventsChannel := events.Channel()

	for msg := range eventsChannel {
		slog.Info("Received message", "message", msg.String())

		if msg.Payload == redisRoundFinish {
			slog.Info("round_finish event received, reconfiguring interface")
		} else {
			continue
		}

		if err := onRound(); err != nil {
			slog.Error("Failed to reconfigure network", "error", err)
		}
	}

	return nil
}

func connectToRedis(ctx context.Context) (*redis.Client, error) {
	cfg := config.Load()

	redisClient := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
	})

	if err := redisClient.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return redisClient, nil
}
