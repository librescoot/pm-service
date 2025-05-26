package hardware

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisListener handles Redis commands for hardware control
type RedisListener struct {
	redis   *redis.Client
	manager *Manager
	logger  *log.Logger
	ctx     context.Context
	cancel  context.CancelFunc
}

// NewRedisListener creates a new Redis listener for hardware commands
func NewRedisListener(ctx context.Context, redisClient *redis.Client, manager *Manager, logger *log.Logger) *RedisListener {
	listenerCtx, cancel := context.WithCancel(ctx)
	return &RedisListener{
		redis:   redisClient,
		manager: manager,
		logger:  logger,
		ctx:     listenerCtx,
		cancel:  cancel,
	}
}

// Start begins listening for Redis commands
func (rl *RedisListener) Start() error {
	go rl.listenForHardwareCommands()
	go rl.listenForGovernorCommands()
	return nil
}

// Stop stops the Redis listener
func (rl *RedisListener) Stop() {
	rl.cancel()
}

// listenForHardwareCommands handles scooter:hardware commands
func (rl *RedisListener) listenForHardwareCommands() {
	for {
		select {
		case <-rl.ctx.Done():
			return
		default:
			// Block for up to 1 second waiting for commands
			result, err := rl.redis.BRPop(rl.ctx, time.Second, "scooter:hardware").Result()
			if err != nil {
				if err == redis.Nil {
					continue // Timeout, continue listening
				}
				if strings.Contains(err.Error(), "context canceled") {
					return
				}
				rl.logger.Printf("Error reading from scooter:hardware: %v", err)
				time.Sleep(time.Second)
				continue
			}

			if len(result) != 2 {
				continue
			}

			command := result[1]
			rl.handleHardwareCommand(command)
		}
	}
}

// listenForGovernorCommands handles scooter:governor commands
func (rl *RedisListener) listenForGovernorCommands() {
	for {
		select {
		case <-rl.ctx.Done():
			return
		default:
			// Block for up to 1 second waiting for commands
			result, err := rl.redis.BRPop(rl.ctx, time.Second, "scooter:governor").Result()
			if err != nil {
				if err == redis.Nil {
					continue // Timeout, continue listening
				}
				if strings.Contains(err.Error(), "context canceled") {
					return
				}
				rl.logger.Printf("Error reading from scooter:governor: %v", err)
				time.Sleep(time.Second)
				continue
			}

			if len(result) != 2 {
				continue
			}

			governor := result[1]
			rl.handleGovernorCommand(governor)
		}
	}
}

// handleHardwareCommand processes hardware control commands
func (rl *RedisListener) handleHardwareCommand(command string) {
	rl.logger.Printf("Received hardware command: %s", command)

	parts := strings.Split(command, ":")
	if len(parts) != 2 {
		rl.logger.Printf("Invalid hardware command format: %s", command)
		return
	}

	component := parts[0]
	action := parts[1]

	var err error
	switch component {
	case "dashboard":
		switch action {
		case "on":
			err = rl.manager.SetDashboardPower(true)
		case "off":
			err = rl.manager.SetDashboardPower(false)
		default:
			rl.logger.Printf("Unknown dashboard action: %s", action)
			return
		}
	case "engine":
		switch action {
		case "on":
			err = rl.manager.SetEnginePower(true)
		case "off":
			err = rl.manager.SetEnginePower(false)
		default:
			rl.logger.Printf("Unknown engine action: %s", action)
			return
		}
	default:
		rl.logger.Printf("Unknown hardware component: %s", component)
		return
	}

	if err != nil {
		rl.logger.Printf("Failed to execute hardware command %s: %v", command, err)
	}
}

// handleGovernorCommand processes CPU governor commands
func (rl *RedisListener) handleGovernorCommand(governor string) {
	rl.logger.Printf("Received governor command: %s", governor)

	err := rl.manager.SetCPUGovernor(governor)
	if err != nil {
		rl.logger.Printf("Failed to set CPU governor to %s: %v", governor, err)
	}
}
