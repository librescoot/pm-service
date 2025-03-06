package power

import (
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/librescoot/pm-service/internal/systemd"
)

type PowerState string

const (
	StateRun             PowerState = "run"
	StateSuspend         PowerState = "suspend"
	StateHibernate       PowerState = "hibernate"
	StateHibernateManual PowerState = "hibernate-manual"
	StateHibernateTimer  PowerState = "hibernate-timer"
	StateReboot          PowerState = "reboot"
)

type Manager struct {
	logger               *log.Logger
	systemd              *systemd.Client
	mutex                sync.RWMutex
	currentState         PowerState
	targetState          PowerState
	dryRunMode           bool
	onLowPowerStateEnter func(PowerState)
	onLowPowerStateExit  func()
	lowPowerModeIssued   bool
	wakeupSourcePath     string
}

func NewManager(logger *log.Logger, dryRunMode bool, onLowPowerStateEnter func(PowerState), onLowPowerStateExit func()) (*Manager, error) {
	systemdClient, err := systemd.NewClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create systemd client: %v", err)
	}

	return &Manager{
		logger:               logger,
		systemd:              systemdClient,
		currentState:         StateRun,
		targetState:          StateRun,
		dryRunMode:           dryRunMode,
		onLowPowerStateEnter: onLowPowerStateEnter,
		onLowPowerStateExit:  onLowPowerStateExit,
		lowPowerModeIssued:   false,
		wakeupSourcePath:     "/sys/power/pm_wakeup_irq",
	}, nil
}

func (m *Manager) GetCurrentState() PowerState {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.currentState
}

func (m *Manager) GetTargetState() PowerState {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.targetState
}

func (m *Manager) SetTargetState(state PowerState) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.targetState == state {
		m.logger.Printf("Target power state is already set to %s, ignoring request", state)
		return
	}

	// Priority rules:
	// 1. run
	// 2. hibernate-manual
	// 3. hibernate
	// 4. hibernate-timer
	// 5. suspend/reboot

	// Don't allow hibernation or hibernation timer request when targetPowerState is hibernate-manual
	if m.targetState == StateHibernateManual && (state == StateHibernate || state == StateHibernateTimer) {
		m.logger.Printf("Current target power state is %s; ignoring requested state %s",
			m.targetState, state)
		return
	}

	// Don't allow hibernation timer request when targetPowerState is hibernate-manual or hibernate
	if (m.targetState == StateHibernateManual || m.targetState == StateHibernate) && state == StateHibernateTimer {
		m.logger.Printf("Current target power state is %s; ignoring requested state %s",
			m.targetState, state)
		return
	}

	// Don't allow suspend or reboot requests when targetPowerState is hibernate, hibernate-manual, or hiberate-timer
	if (m.targetState == StateHibernate || m.targetState == StateHibernateManual || m.targetState == StateHibernateTimer) &&
		(state == StateSuspend || state == StateReboot) {
		m.logger.Printf("Current target power state is %s; ignoring requested state %s",
			m.targetState, state)
		return
	}

	m.logger.Printf("Setting target power state to %s", state)
	m.targetState = state
}

func (m *Manager) IssueTargetState(state PowerState) error {
	if state == StateRun {
		m.logger.Printf("Not issuing power state for run state")
		return nil
	}

	if m.dryRunMode {
		m.logger.Printf("[DRY RUN] Would issue power state %s", state)
		return nil
	}

	var target string
	switch state {
	case StateSuspend:
		target = "suspend"
	case StateHibernate, StateHibernateManual, StateHibernateTimer:
		target = "poweroff"
	case StateReboot:
		target = "reboot"
	default:
		return fmt.Errorf("unsupported power state: %s", state)
	}

	m.mutex.Lock()
	m.lowPowerModeIssued = true
	m.mutex.Unlock()

	if m.onLowPowerStateEnter != nil {
		m.onLowPowerStateEnter(state)
	}

	m.logger.Printf("Issuing %s command", target)

	if err := m.systemd.IssueCommand(target); err != nil {
		m.mutex.Lock()
		m.lowPowerModeIssued = false
		m.mutex.Unlock()
		return fmt.Errorf("failed to issue %s command: %v", target, err)
	}

	return nil
}

func (m *Manager) OnWakeup() {
	wakeupReason := "unknown"
	if data, err := os.ReadFile(m.wakeupSourcePath); err == nil {
		wakeupReason = string(data)
	}

	m.mutex.Lock()
	m.lowPowerModeIssued = false
	m.currentState = StateRun
	m.mutex.Unlock()

	m.logger.Printf("IRQ wakeup reason: %s", wakeupReason)

	if m.onLowPowerStateExit != nil {
		m.onLowPowerStateExit()
	}
}

func (m *Manager) IsLowPowerStateIssued() bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.lowPowerModeIssued
}

func (m *Manager) Close() error {
	return m.systemd.Close()
}
