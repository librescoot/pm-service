package hibernation

import (
	"context"
	"log"
	"strings"

	"github.com/redis/go-redis/v9"
)

// RedisListener handles Redis input events for hibernation
type RedisListener struct {
	redis        *redis.Client
	stateMachine *StateMachine
	logger       *log.Logger
	ctx          context.Context
	cancel       context.CancelFunc
}

// NewRedisListener creates a new Redis listener for hibernation inputs
func NewRedisListener(ctx context.Context, redisClient *redis.Client, stateMachine *StateMachine, logger *log.Logger) *RedisListener {
	listenerCtx, cancel := context.WithCancel(ctx)
	return &RedisListener{
		redis:        redisClient,
		stateMachine: stateMachine,
		logger:       logger,
		ctx:          listenerCtx,
		cancel:       cancel,
	}
}

// Start begins listening for hibernation-related input events
func (rl *RedisListener) Start() error {
	go rl.listenForInputEvents()
	return nil
}

// Stop stops the Redis listener
func (rl *RedisListener) Stop() {
	rl.cancel()
}

// listenForInputEvents monitors vehicle input events for hibernation triggers
func (rl *RedisListener) listenForInputEvents() {
	// Subscribe to vehicle inputs that could trigger hibernation
	pubsub := rl.redis.Subscribe(rl.ctx, "buttons", "seatbox", "vehicle")
	defer pubsub.Close()

	ch := pubsub.Channel()
	for {
		select {
		case <-rl.ctx.Done():
			return
		case msg := <-ch:
			rl.handleInputEvent(msg.Channel, msg.Payload)
		}
	}
}

// handleInputEvent processes input events for hibernation sequence
func (rl *RedisListener) handleInputEvent(channel, payload string) {
	switch channel {
	case "buttons":
		// Parse button events for hibernation trigger
		if strings.Contains(payload, "hibernation") {
			if strings.Contains(payload, "pressed") {
				rl.stateMachine.ProcessInput("hibernation_input", "pressed")
			} else if strings.Contains(payload, "released") {
				rl.stateMachine.ProcessInput("hibernation_input", "released")
			}
		}

	case "seatbox":
		// Monitor seatbox state for hibernation sequence
		if strings.Contains(payload, "closed") {
			rl.stateMachine.ProcessInput("seatbox", "closed")
		} else if strings.Contains(payload, "opened") {
			rl.stateMachine.ProcessInput("seatbox", "opened")
		}

	case "vehicle":
		// Monitor vehicle state changes that might cancel hibernation
		if strings.Contains(payload, "state") {
			// Get current vehicle state
			vehicleState, err := rl.redis.HGet(rl.ctx, "vehicle", "state").Result()
			if err == nil && vehicleState != "stand-by" {
				// Vehicle left standby state, cancel hibernation sequence
				if rl.stateMachine.GetState() != HibernationStateIdle {
					rl.logger.Printf("Vehicle left standby state (%s), canceling hibernation sequence", vehicleState)
					rl.stateMachine.CancelSequence()
				}
			}
		}
	}
}
