package hardware

import (
	"context"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"
)

// Manager coordinates power hardware control and Redis state management
type Manager struct {
	gpio     *GPIOManager
	governor *GovernorManager
	redis    *redis.Client
	logger   *log.Logger
	ctx      context.Context
}

// NewManager creates a new hardware manager
func NewManager(ctx context.Context, redisClient *redis.Client, logger *log.Logger, dryRun bool) (*Manager, error) {
	gpio, err := NewGPIOManager(logger, dryRun)
	if err != nil {
		return nil, fmt.Errorf("failed to create GPIO manager: %w", err)
	}

	governor := NewGovernorManager(logger, dryRun)

	return &Manager{
		gpio:     gpio,
		governor: governor,
		redis:    redisClient,
		logger:   logger,
		ctx:      ctx,
	}, nil
}

// SetDashboardPower controls dashboard power and updates Redis state
func (m *Manager) SetDashboardPower(enabled bool) error {
	if err := m.gpio.SetDashboardPower(enabled); err != nil {
		return err
	}

	// Update Redis state
	value := "off"
	if enabled {
		value = "on"
	}

	pipe := m.redis.Pipeline()
	pipe.HSet(m.ctx, "dashboard", "power", value)
	pipe.Publish(m.ctx, "dashboard", "power")
	_, err := pipe.Exec(m.ctx)
	if err != nil {
		m.logger.Printf("Warning: Failed to update dashboard power state in Redis: %v", err)
	}

	return nil
}

// SetEnginePower controls engine power and updates Redis state
func (m *Manager) SetEnginePower(enabled bool) error {
	if err := m.gpio.SetEnginePower(enabled); err != nil {
		return err
	}

	// Update Redis state
	value := "off"
	if enabled {
		value = "on"
	}

	pipe := m.redis.Pipeline()
	pipe.HSet(m.ctx, "engine-ecu", "power", value)
	pipe.Publish(m.ctx, "engine-ecu", "power")
	_, err := pipe.Exec(m.ctx)
	if err != nil {
		m.logger.Printf("Warning: Failed to update engine power state in Redis: %v", err)
	}

	return nil
}

// SetCPUGovernor sets the CPU governor and updates Redis state
func (m *Manager) SetCPUGovernor(governor string) error {
	if err := m.governor.SetGovernor(governor); err != nil {
		return err
	}

	// Update Redis state
	pipe := m.redis.Pipeline()
	pipe.HSet(m.ctx, "system", "cpu_governor", governor)
	pipe.Publish(m.ctx, "system", "cpu_governor")
	_, err := pipe.Exec(m.ctx)
	if err != nil {
		m.logger.Printf("Warning: Failed to update CPU governor state in Redis: %v", err)
	}

	return nil
}

// GetCPUGovernor returns the current CPU governor
func (m *Manager) GetCPUGovernor() (string, error) {
	return m.governor.GetGovernor()
}

// InitializeRedisState sets initial Redis state from current hardware state
func (m *Manager) InitializeRedisState() error {
	// Get current CPU governor
	governor, err := m.governor.GetGovernor()
	if err != nil {
		m.logger.Printf("Warning: Could not read current CPU governor: %v", err)
		governor = "unknown"
	}

	// Initialize Redis state
	pipe := m.redis.Pipeline()

	// Set initial power states (assume off at startup)
	pipe.HSet(m.ctx, "dashboard", "power", "off")
	pipe.HSet(m.ctx, "engine-ecu", "power", "off")
	pipe.HSet(m.ctx, "system", "cpu_governor", governor)

	// Publish initial states
	pipe.Publish(m.ctx, "dashboard", "power")
	pipe.Publish(m.ctx, "engine-ecu", "power")
	pipe.Publish(m.ctx, "system", "cpu_governor")

	_, err = pipe.Exec(m.ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize Redis power hardware state: %w", err)
	}

	m.logger.Printf("Initialized Redis power hardware state - CPU governor: %s", governor)
	return nil
}

// Close releases all hardware resources
func (m *Manager) Close() error {
	return m.gpio.Close()
}
