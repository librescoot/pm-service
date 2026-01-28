package inhibitor

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

const (
	// Redis keys for power inhibits
	InhibitHashKey = "power:inhibits"
	InhibitChannel = "power:inhibits"
)

// InhibitData represents the data stored in Redis for an inhibit
type InhibitData struct {
	ID       string `json:"id"`
	Who      string `json:"who"`
	What     string `json:"what"`
	Why      string `json:"why"`
	Type     string `json:"type"`
	Duration int64  `json:"duration"`
	Created  int64  `json:"created"`
}

// RedisListener listens for power inhibit requests from Redis
type RedisListener struct {
	client     *redis.Client
	manager    *Manager
	logger     *log.Logger
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	inhibitors map[string]*Inhibitor
	mutex      sync.RWMutex
}

// NewRedisListener creates a new Redis listener for power inhibits
func NewRedisListener(redisAddr string, manager *Manager, logger *log.Logger) (*RedisListener, error) {
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
		client:     client,
		manager:    manager,
		logger:     logger,
		ctx:        ctx,
		cancel:     cancel,
		inhibitors: make(map[string]*Inhibitor),
		mutex:      sync.RWMutex{},
	}, nil
}

// Start starts the Redis listener
func (r *RedisListener) Start() error {
	r.logger.Printf("Starting Redis listener for power inhibits")

	// Subscribe to power inhibit channel
	pubsub := r.client.Subscribe(r.ctx, InhibitChannel)
	r.logger.Printf("Subscribed to Redis channel: %s", InhibitChannel)

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
			if msg.Channel == InhibitChannel {
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
			inhibits, err := r.client.HGetAll(r.ctx, InhibitHashKey).Result()
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
	inhibitData, err := r.client.HGet(r.ctx, InhibitHashKey, id).Result()
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

	// Check if we already have this inhibitor
	r.mutex.RLock()
	_, exists := r.inhibitors[id]
	r.mutex.RUnlock()

	if exists {
		r.logger.Printf("Inhibitor %s already exists, ignoring", id)
		return
	}

	// Create inhibitor with the appropriate type
	var inhibitType InhibitorType
	if data.Type == "block" {
		inhibitType = TypeBlock
	} else {
		inhibitType = TypeDelay
	}

	// Create inhibitor
	inhibitor := r.manager.AddInhibitor(
		data.Who,
		data.What,
		data.Why,
		inhibitType,
	)

	// Store the inhibitor
	r.mutex.Lock()
	r.inhibitors[id] = inhibitor
	r.mutex.Unlock()

	// If duration is specified, create a timer to remove the inhibit
	if data.Duration > 0 {
		duration := time.Duration(data.Duration) * time.Second
		time.AfterFunc(duration, func() {
			r.handleRemoveInhibit(id)
		})
	}
}

// handleRemoveInhibit handles removing a power inhibit
func (r *RedisListener) handleRemoveInhibit(id string) {
	// Don't process removals after context cancellation (e.g. from stale AfterFunc timers)
	if r.ctx.Err() != nil {
		return
	}

	// Check if we have this inhibitor
	r.mutex.RLock()
	inhibitor, exists := r.inhibitors[id]
	r.mutex.RUnlock()

	if !exists {
		r.logger.Printf("Inhibitor %s does not exist, ignoring", id)
		return
	}

	// Remove the inhibitor
	r.manager.RemoveInhibitor(inhibitor)

	// Remove from our map
	r.mutex.Lock()
	delete(r.inhibitors, id)
	r.mutex.Unlock()
}

// Stop stops the Redis listener
func (r *RedisListener) Stop() {
	r.cancel()
	r.wg.Wait()

	// Remove all inhibitors
	r.mutex.Lock()
	for id, inhibitor := range r.inhibitors {
		r.manager.RemoveInhibitor(inhibitor)
		delete(r.inhibitors, id)
	}
	r.mutex.Unlock()

	r.client.Close()
}
