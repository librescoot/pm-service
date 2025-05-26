package hardware

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// GovernorManager handles CPU governor management
type GovernorManager struct {
	logger  *log.Logger
	dryRun  bool
	cpuPath string
	current string
}

// NewGovernorManager creates a new CPU governor manager
func NewGovernorManager(logger *log.Logger, dryRun bool) *GovernorManager {
	return &GovernorManager{
		logger:  logger,
		dryRun:  dryRun,
		cpuPath: "/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor",
	}
}

// SetGovernor sets the CPU governor
func (gm *GovernorManager) SetGovernor(governor string) error {
	// Validate governor
	validGovernors := map[string]bool{
		"ondemand":     true,
		"powersave":    true,
		"performance":  true,
		"conservative": true,
		"userspace":    true,
	}

	if !validGovernors[governor] {
		return fmt.Errorf("invalid governor: %s", governor)
	}

	if gm.dryRun {
		gm.logger.Printf("DRY RUN: Would set CPU governor to %s", governor)
		gm.current = governor
		return nil
	}

	// Write to sysfs
	err := os.WriteFile(gm.cpuPath, []byte(governor), 0644)
	if err != nil {
		return fmt.Errorf("failed to set CPU governor to %s: %w", governor, err)
	}

	gm.current = governor
	gm.logger.Printf("Set CPU governor to %s", governor)
	return nil
}

// GetGovernor returns the current CPU governor
func (gm *GovernorManager) GetGovernor() (string, error) {
	if gm.dryRun {
		return gm.current, nil
	}

	data, err := os.ReadFile(gm.cpuPath)
	if err != nil {
		return "", fmt.Errorf("failed to read CPU governor: %w", err)
	}

	governor := strings.TrimSpace(string(data))
	gm.current = governor
	return governor, nil
}

// GetAvailableGovernors returns the list of available governors
func (gm *GovernorManager) GetAvailableGovernors() ([]string, error) {
	if gm.dryRun {
		return []string{"ondemand", "powersave", "performance"}, nil
	}

	availablePath := "/sys/devices/system/cpu/cpu0/cpufreq/scaling_available_governors"
	data, err := os.ReadFile(availablePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read available governors: %w", err)
	}

	governors := strings.Fields(string(data))
	return governors, nil
}
