package hardware

import (
	"fmt"
	"log"

	"github.com/warthog618/go-gpiocdev"
)

// GPIOManager handles GPIO operations for power hardware control
type GPIOManager struct {
	chip   *gpiocdev.Chip
	lines  map[string]*gpiocdev.Line
	logger *log.Logger
	dryRun bool
}

// NewGPIOManager creates a new GPIO manager
func NewGPIOManager(logger *log.Logger, dryRun bool) (*GPIOManager, error) {
	gm := &GPIOManager{
		lines:  make(map[string]*gpiocdev.Line),
		logger: logger,
		dryRun: dryRun,
	}

	if !dryRun {
		// Open GPIO chip
		chip, err := gpiocdev.NewChip("gpiochip0")
		if err != nil {
			return nil, fmt.Errorf("failed to open GPIO chip: %w", err)
		}
		gm.chip = chip

		// Initialize power GPIO lines
		if err := gm.initializePowerLines(); err != nil {
			chip.Close()
			return nil, fmt.Errorf("failed to initialize power GPIO lines: %w", err)
		}
	}

	return gm, nil
}

// initializePowerLines sets up the GPIO lines for power control
func (gm *GPIOManager) initializePowerLines() error {
	// Dashboard power - GPIO 1:18 (chip offset + pin)
	dashboardLine, err := gm.chip.RequestLine(50, gpiocdev.AsOutput(0)) // GPIO 1:18 = 32*1 + 18 = 50
	if err != nil {
		return fmt.Errorf("failed to request dashboard power GPIO: %w", err)
	}
	gm.lines["dashboard_power"] = dashboardLine

	// Engine power - GPIO 2:17 (chip offset + pin)
	engineLine, err := gm.chip.RequestLine(81, gpiocdev.AsOutput(0)) // GPIO 2:17 = 32*2 + 17 = 81
	if err != nil {
		return fmt.Errorf("failed to request engine power GPIO: %w", err)
	}
	gm.lines["engine_power"] = engineLine

	gm.logger.Printf("Initialized power GPIO lines: dashboard_power (GPIO 1:18), engine_power (GPIO 2:17)")
	return nil
}

// SetDashboardPower controls the dashboard/DBC power
func (gm *GPIOManager) SetDashboardPower(enabled bool) error {
	if gm.dryRun {
		gm.logger.Printf("DRY RUN: Would set dashboard power to %v", enabled)
		return nil
	}

	line, exists := gm.lines["dashboard_power"]
	if !exists {
		return fmt.Errorf("dashboard power GPIO line not initialized")
	}

	value := 0
	if enabled {
		value = 1
	}

	err := line.SetValue(value)
	if err != nil {
		return fmt.Errorf("failed to set dashboard power GPIO: %w", err)
	}

	gm.logger.Printf("Set dashboard power to %v (GPIO value: %d)", enabled, value)
	return nil
}

// SetEnginePower controls the motor controller power
func (gm *GPIOManager) SetEnginePower(enabled bool) error {
	if gm.dryRun {
		gm.logger.Printf("DRY RUN: Would set engine power to %v", enabled)
		return nil
	}

	line, exists := gm.lines["engine_power"]
	if !exists {
		return fmt.Errorf("engine power GPIO line not initialized")
	}

	value := 0
	if enabled {
		value = 1
	}

	err := line.SetValue(value)
	if err != nil {
		return fmt.Errorf("failed to set engine power GPIO: %w", err)
	}

	gm.logger.Printf("Set engine power to %v (GPIO value: %d)", enabled, value)
	return nil
}

// Close releases all GPIO resources
func (gm *GPIOManager) Close() error {
	if gm.dryRun {
		return nil
	}

	var lastErr error

	// Close all lines
	for name, line := range gm.lines {
		if err := line.Close(); err != nil {
			gm.logger.Printf("Failed to close GPIO line %s: %v", name, err)
			lastErr = err
		}
	}

	// Close chip
	if gm.chip != nil {
		if err := gm.chip.Close(); err != nil {
			gm.logger.Printf("Failed to close GPIO chip: %v", err)
			lastErr = err
		}
	}

	gm.logger.Printf("Closed GPIO manager")
	return lastErr
}
