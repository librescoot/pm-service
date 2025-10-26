package power

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// InhibitData represents the data stored in Redis for an inhibit
type InhibitData struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Duration int64  `json:"duration"`
	Created  int64  `json:"created"`
}

// RedisListener listens for power inhibit requests from Redis
type RedisListener struct {
	client    *redis.Client
	inhibitor *Inhibitor
	logger    *log.Logger
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewRedisListener creates a new Redis listener for power inhibits
func NewRedisListener(redisAddr string, inhibitor *Inhibitor, logger *log.Logger) (*RedisListener, error) {
	client := redis.NewClient(&redis.Options{
		Addr: redisAddr,
		DB:   0,
	})

	ctx, cancel := context.WithCancel(context.Background())

	// Test connection
	if err := client.Ping(ctx).Err(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &RedisListener{
		client:    client,
		inhibitor: inhibitor,
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
	}, nil
}

// Start starts the Redis listener
func (r *RedisListener) Start() error {
	r.logger.Printf("Starting Redis listener for power inhibits")

	// Subscribe to power inhibit channel
	pubsub := r.client.Subscribe(r.ctx, "power:inhibits")
	r.logger.Printf("Subscribed to Redis channel: power:inhibits")

	// Start pub/sub listener
	r.wg.Add(1)
	go r.channelListener(pubsub)

	// Start hash field monitor
	r.wg.Add(1)
	go r.hashFieldMonitor()

	return nil
}

// channelListener listens for messages on the power inhibits channel
func (r *RedisListener) channelListener(pubsub *redis.PubSub) {
	defer r.wg.Done()
	defer pubsub.Close()

	r.logger.Printf("Starting Redis channel listener")
	channel := pubsub.Channel()

	for {
		select {
		case msg, ok := <-channel:
			if !ok {
				r.logger.Printf("Redis channel closed unexpectedly")
				log.Fatalf("Redis connection lost, exiting to allow systemd restart")
			}
			if msg == nil {
				r.logger.Printf("Received nil Redis message")
				log.Fatalf("Redis connection lost, exiting to allow systemd restart")
			}

			r.logger.Printf("Received Redis message: channel=%s payload=%s", msg.Channel, msg.Payload)

			// Handle power inhibit messages
			if msg.Channel == "power:inhibits" {
				parts := strings.SplitN(msg.Payload, ":", 2)
				if len(parts) != 2 {
					r.logger.Printf("Invalid power inhibit message format: %s", msg.Payload)
					continue
				}

				action, id := parts[0], parts[1]
				switch action {
				case "add":
					r.handleAddInhibit(id)
				case "remove":
					r.handleRemoveInhibit(id)
				default:
					r.logger.Printf("Unknown power inhibit action: %s", action)
				}
			}

		case <-r.ctx.Done():
			r.logger.Printf("Context cancelled, exiting listener")
			return
		}
	}
}

// hashFieldMonitor monitors the power inhibits hash for changes
func (r *RedisListener) hashFieldMonitor() {
	defer r.wg.Done()

	r.logger.Printf("Starting hash field monitor for power inhibits")

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Keep track of known inhibits
	knownInhibits := make(map[string]bool)

	for {
		select {
		case <-r.ctx.Done():
			r.logger.Printf("Context cancelled, exiting hash field monitor")
			return
		case <-ticker.C:
			// Get all inhibits from Redis
			inhibits, err := r.client.HGetAll(r.ctx, "power:inhibits").Result()
			if err != nil {
				r.logger.Printf("Error getting power inhibits from Redis: %v", err)
				continue
			}

			// Check for new inhibits
			for id := range inhibits {
				if !knownInhibits[id] {
					r.logger.Printf("Found new inhibit in hash: %s", id)
					r.handleAddInhibit(id)
					knownInhibits[id] = true
				}
			}

			// Check for removed inhibits
			for id := range knownInhibits {
				if _, exists := inhibits[id]; !exists {
					r.logger.Printf("Inhibit removed from hash: %s", id)
					r.handleRemoveInhibit(id)
					delete(knownInhibits, id)
				}
			}
		}
	}
}

// handleAddInhibit handles adding a power inhibit
func (r *RedisListener) handleAddInhibit(id string) {
	// Get inhibit data from Redis
	inhibitData, err := r.client.HGet(r.ctx, "power:inhibits", id).Result()
	if err != nil {
		r.logger.Printf("Error getting inhibit data for %s: %v", id, err)
		return
	}

	// Parse inhibit data
	var data InhibitData
	if err := json.Unmarshal([]byte(inhibitData), &data); err != nil {
		r.logger.Printf("Error parsing inhibit data for %s: %v", id, err)
		return
	}

	// Create inhibit request
	var req InhibitRequest
	req.ID = id

	switch data.Type {
	case "downloading":
		req.Reason = InhibitReasonDownloading
		req.Defer = false

		// Use the duration from the data, or default to 5 minutes
		if data.Duration > 0 {
			req.Duration = time.Duration(data.Duration) * time.Second
		} else {
			req.Duration = 5 * time.Minute
		}

	case "installing":
		req.Reason = InhibitReasonInstalling
		req.Defer = true
		req.Duration = 0 // Indefinite

	default:
		r.logger.Printf("Unknown inhibit type: %s", data.Type)
		return
	}

	// Add inhibit
	if err := r.inhibitor.AddInhibit(req); err != nil {
		r.logger.Printf("Error adding inhibit %s: %v", id, err)
	}
}

// handleRemoveInhibit handles removing a power inhibit
func (r *RedisListener) handleRemoveInhibit(id string) {
	if err := r.inhibitor.RemoveInhibit(id); err != nil {
		r.logger.Printf("Error removing inhibit %s: %v", id, err)
	}
}

// Stop stops the Redis listener
func (r *RedisListener) Stop() {
	r.cancel()
	r.wg.Wait()
	r.client.Close()
}
