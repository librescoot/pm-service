package hibernation

import (
	"context"
	"log"
	"strings"
	"time"

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
	go rl.listenForHibernationCommands()
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

// listenForHibernationCommands handles direct hibernation commands
func (rl *RedisListener) listenForHibernationCommands() {
	for {
		select {
		case <-rl.ctx.Done():
			return
		default:
			// Block for up to 1 second waiting for commands
			result, err := rl.redis.BRPop(rl.ctx, time.Second, "scooter:hibernation").Result()
			if err != nil {
				if err == redis.Nil {
					continue // Timeout, continue listening
				}
				if strings.Contains(err.Error(), "context canceled") {
					return
				}
				rl.logger.Printf("Error reading from scooter:hibernation: %v", err)
				time.Sleep(time.Second)
				continue
			}

			if len(result) != 2 {
				continue
			}

			command := result[1]
			rl.handleHibernationCommand(command)
		}
	}
}

// handleInputEvent processes input events for hibernation sequence
func (rl *RedisListener) handleInputEvent(channel, payload string) {
	switch channel {
	case "buttons":
		// Parse button events for hibernation trigger
		// This would need to be customized based on the actual button mapping
		// For example, holding both brake buttons + seatbox button for hibernation
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

// handleHibernationCommand processes direct hibernation commands
func (rl *RedisListener) handleHibernationCommand(command string) {
	rl.logger.Printf("Received hibernation command: %s", command)

	switch command {
	case "start":
		rl.stateMachine.StartHibernationSequence()
	case "cancel":
		rl.stateMachine.CancelSequence()
	case "input-pressed":
		rl.stateMachine.ProcessInput("hibernation_input", "pressed")
	case "input-released":
		rl.stateMachine.ProcessInput("hibernation_input", "released")
	case "seatbox-closed":
		rl.stateMachine.ProcessInput("seatbox", "closed")
	case "seatbox-opened":
		rl.stateMachine.ProcessInput("seatbox", "opened")
	default:
		rl.logger.Printf("Unknown hibernation command: %s", command)
	}
}
