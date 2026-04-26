package service

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/librescoot/librefsm"
	"github.com/librescoot/pm-service/internal/config"
	"github.com/librescoot/pm-service/internal/fsm"
	"github.com/librescoot/pm-service/internal/hibernation"
	"github.com/librescoot/pm-service/internal/inhibitor"
	"github.com/librescoot/pm-service/internal/systemd"
	redis_ipc "github.com/librescoot/redis-ipc"
)

type Service struct {
	config             *config.Config
	logger             *log.Logger
	redis              *redis_ipc.Client
	inhibitorManager   *inhibitor.Manager
	hibernationTimer   *hibernation.Timer
	systemdClient      *systemd.Client
	delayInhibitor     *inhibitor.Inhibitor
	delayInhibitorMu   sync.Mutex
	cancelDelayRemoval context.CancelFunc
	powerManagerPub    *redis_ipc.HashPublisher
	systemPub          *redis_ipc.HashPublisher
	busyServicesPub    *redis_ipc.HashPublisher
	ctx                context.Context
	ctxCancel          context.CancelFunc

	// librefsm
	machine *librefsm.Machine
	fsmData *fsm.FSMData
}

func New(cfg *config.Config, logger *log.Logger) (*Service, error) {
	redisClient, err := redis_ipc.New(
		redis_ipc.WithAddress(cfg.RedisHost),
		redis_ipc.WithPort(cfg.RedisPort),
		redis_ipc.WithRetryInterval(5*time.Second),
		redis_ipc.WithMaxRetries(3),
		redis_ipc.WithCodec(redis_ipc.StringCodec{}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Redis client: %v", err)
	}

	systemdClient, err := systemd.NewClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create systemd client: %v", err)
	}

	service := &Service{
		config:          cfg,
		logger:          logger,
		redis:           redisClient,
		systemdClient:   systemdClient,
		powerManagerPub: redisClient.NewHashPublisher("power-manager"),
		systemPub:       redisClient.NewHashPublisher("system"),
		busyServicesPub: redisClient.NewHashPublisher("power-manager:busy-services"),
		fsmData: &fsm.FSMData{
			TargetPowerState: cfg.DefaultState,
			VehicleState:     "",
			BatteryState:     "",
		},
	}

	inhibitorManager, err := inhibitor.NewManager(
		logger,
		cfg.SocketPath,
		service.onInhibitorsChanged,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create inhibitor manager: %v", err)
	}
	service.inhibitorManager = inhibitorManager

	service.delayInhibitor = inhibitorManager.AddInhibitor(
		"pm-service",
		"default delay",
		"delay",
		inhibitor.TypeDelay,
	)

	return service, nil
}

func (s *Service) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.ctx = ctx
	s.ctxCancel = cancel

	// Read initial states directly before starting any goroutines.
	// This populates fsmData for startup decisions (hibernation timer, initial trigger)
	// without racing against FSM action writers.
	if vehicleState, err := s.redis.HGet("vehicle", "state"); err == nil && vehicleState != "" {
		s.fsmData.VehicleState = vehicleState
		s.logger.Printf("Initial vehicle state: %s", vehicleState)
	}
	if batteryState, err := s.redis.HGet("battery:0", "state"); err == nil && batteryState != "" {
		s.fsmData.Battery0State = batteryState
		s.logger.Printf("Initial battery:0 state: %s", batteryState)
	}
	if batteryState, err := s.redis.HGet("battery:1", "state"); err == nil && batteryState != "" {
		s.fsmData.Battery1State = batteryState
		s.logger.Printf("Initial battery:1 state: %s", batteryState)
	}
	if s.fsmData.Battery0State == "active" || s.fsmData.Battery1State == "active" {
		s.fsmData.BatteryState = "active"
	}

	// Read default power state from settings; CLI arg is the fallback
	if defaultState, err := s.redis.HGet("settings", "pm.default-state"); err == nil && defaultState != "" {
		if isValidDefaultState(defaultState) {
			s.logger.Printf("Default power state from settings: %s (CLI fallback: %s)", defaultState, s.config.DefaultState)
			s.config.DefaultState = defaultState
			s.fsmData.TargetPowerState = defaultState
		} else {
			s.logger.Printf("Ignoring invalid pm.default-state setting %q, using CLI default: %s", defaultState, s.config.DefaultState)
		}
	}

	// Create hibernation timer with the run context
	s.hibernationTimer = hibernation.NewTimer(
		ctx,
		s.logger,
		s.config.HibernationTimer,
		func() {
			if s.machine != nil {
				s.machine.Send(librefsm.Event{
					ID:      fsm.EvHibernationTimerExpired,
					Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetHibernateTimer},
				})
			}
		},
	)

	// Enable wakeup on configured serial ports
	s.enableWakeupSources()

	// Build FSM
	def := fsm.NewDefinition(s, s.config.PreSuspendDelay, s.config.SuspendImminentDelay)
	machine, err := def.Build(
		librefsm.WithData(s.fsmData),
		librefsm.WithLogger(slog.Default()),
		librefsm.WithEventQueueSize(100),
	)
	if err != nil {
		return fmt.Errorf("failed to build FSM: %v", err)
	}
	s.machine = machine

	// Set up state change callback for Redis publishing
	s.machine.OnStateChange(func(from, to librefsm.StateID) {
		s.logger.Printf("FSM state transition: %s -> %s", from, to)
		s.publishFSMState(to)
	})

	// Start FSM
	if err := s.machine.Start(ctx); err != nil {
		return fmt.Errorf("failed to start FSM: %v", err)
	}

	// Publish initial power state
	s.publishFSMState(s.machine.CurrentState())

	// Initialize hibernation timer based on initial vehicle state (read above, before any races)
	if s.fsmData.VehicleState != "ready-to-drive" {
		s.hibernationTimer.ResetTimer(true)
		s.logger.Printf("Initialized hibernation timer - vehicle in idle state: %s", s.fsmData.VehicleState)
	}

	// Trigger low-power sequence evaluation if default state is not "run".
	// Uses cfg.DefaultState to avoid reading fsmData from a potentially racing goroutine.
	if s.config.DefaultState != fsm.TargetRun {
		s.machine.Send(librefsm.Event{
			ID:      fsm.EvVehicleStateChanged,
			Payload: fsm.VehicleStatePayload{State: s.fsmData.VehicleState},
		})
	}

	// Set up Redis hash watchers with initial sync.
	// FSM is running; callbacks send events which are processed by FSM actions.
	if err := s.redis.NewHashWatcher("vehicle").
		OnField("state", s.onVehicleStateChanged).
		StartWithSync(); err != nil {
		return fmt.Errorf("failed to start vehicle state watcher: %v", err)
	}

	if err := s.redis.NewHashWatcher("battery:0").
		OnField("state", func(state string) error { return s.onBatteryStateChanged("0", state) }).
		StartWithSync(); err != nil {
		return fmt.Errorf("failed to start battery:0 state watcher: %v", err)
	}

	if err := s.redis.NewHashWatcher("battery:1").
		OnField("state", func(state string) error { return s.onBatteryStateChanged("1", state) }).
		StartWithSync(); err != nil {
		return fmt.Errorf("failed to start battery:1 state watcher: %v", err)
	}

	redis_ipc.HandleRequests(s.redis, "scooter:power", s.onPowerCommand)
	redis_ipc.HandleRequests(s.redis, "scooter:governor", s.onGovernorCommand)

	// Start Redis inhibitor listener (syncs power:inhibits hash into inhibitor manager)
	go s.inhibitorManager.StartRedisListener(ctx, s.redis, s.logger)

	// Watch settings in Redis
	if err := s.redis.NewHashWatcher("settings").
		OnField("pm.hibernation-timer", s.onHibernationTimerSetting).
		OnField("pm.default-state", s.onDefaultStateSetting).
		StartWithSync(); err != nil {
		return fmt.Errorf("failed to start settings watcher: %v", err)
	}

	// Wait for context cancellation
	<-ctx.Done()

	// Cleanup
	s.machine.Stop()
	s.hibernationTimer.Close()

	if err := s.inhibitorManager.Close(); err != nil {
		s.logger.Printf("Failed to close inhibitor manager: %v", err)
	}

	if err := s.systemdClient.Close(); err != nil {
		s.logger.Printf("Failed to close systemd client: %v", err)
	}

	if err := s.redis.Close(); err != nil {
		s.logger.Printf("Failed to close Redis client: %v", err)
	}

	return nil
}

// Redis handlers - send FSM events only, no fsmData writes

func (s *Service) onVehicleStateChanged(vehicleState string) error {
	if s.machine != nil {
		s.machine.Send(librefsm.Event{
			ID:      fsm.EvVehicleStateChanged,
			Payload: fsm.VehicleStatePayload{State: vehicleState},
		})
	}
	return nil
}

func (s *Service) onBatteryStateChanged(slot, newState string) error {
	if s.machine != nil {
		payload := fsm.BatteryStatePayload{Slot: slot, State: newState}
		if newState == "active" {
			s.machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameActive, Payload: payload})
		} else {
			s.machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameInactive, Payload: payload})
		}
	}
	return nil
}

func (s *Service) onPowerCommand(command string) error {
	s.logger.Printf("Received power command: %s", command)

	var eventID librefsm.EventID
	switch command {
	case "run":
		eventID = fsm.EvPowerRun
	case "suspend":
		eventID = fsm.EvPowerSuspend
	case "hibernate":
		eventID = fsm.EvPowerHibernate
	case "hibernate-manual":
		eventID = fsm.EvPowerHibernateManual
	case "hibernate-timer":
		eventID = fsm.EvPowerHibernateTimer
	case "reboot":
		eventID = fsm.EvPowerReboot
	default:
		s.logger.Printf("Unknown power command: %s", command)
		return nil
	}

	if s.machine != nil {
		s.machine.Send(librefsm.Event{
			ID:      eventID,
			Payload: fsm.PowerCommandPayload{TargetState: command},
		})
	}

	return nil
}

func (s *Service) onInhibitorsChanged() {
	// Publish inhibitors to Redis
	s.publishInhibitors()

	// Send FSM event
	if s.machine != nil {
		s.machine.Send(librefsm.Event{ID: fsm.EvInhibitorsChanged})
	}
}

func (s *Service) onGovernorCommand(governor string) error {
	s.logger.Printf("Received governor command: %s", governor)

	switch governor {
	case "ondemand", "powersave", "performance":
		s.setGovernor(governor)
	default:
		s.logger.Printf("Invalid governor command: %s", governor)
	}

	return nil
}

// vehicleStateFromContext returns the vehicle state from the event payload if available,
// otherwise falls back to fsmData. This ensures guards see the correct state during
// EvVehicleStateChanged transitions before the action has updated fsmData.
func (s *Service) vehicleStateFromContext(c *librefsm.Context) string {
	if c.Event != nil {
		if p, ok := c.Event.Payload.(fsm.VehicleStatePayload); ok {
			return p.State
		}
	}
	return s.fsmData.VehicleState
}

// targetPowerStateFromContext returns the target power state from the event payload if available,
// otherwise falls back to fsmData. This ensures guards see the correct state during
// power command transitions before the action has updated fsmData.
func (s *Service) targetPowerStateFromContext(c *librefsm.Context) string {
	if c.Event != nil {
		if p, ok := c.Event.Payload.(fsm.PowerCommandPayload); ok {
			return p.TargetState
		}
	}
	return s.fsmData.TargetPowerState
}

// batteryStateFromContext returns the battery state from the event payload if available,
// otherwise falls back to fsmData. This ensures guards see the correct state during
// EvBatteryBecame* transitions before the action has updated fsmData.
func (s *Service) batteryStateFromContext(c *librefsm.Context) string {
	if c.Event != nil {
		if p, ok := c.Event.Payload.(fsm.BatteryStatePayload); ok {
			return p.State
		}
	}
	return s.fsmData.BatteryState
}

// Actions interface implementation

func (s *Service) EnterPreSuspend(c *librefsm.Context) error {
	s.logger.Printf("Entering pre-suspend state, waiting %v", s.config.PreSuspendDelay)
	return nil
}

func (s *Service) EnterSuspendImminent(c *librefsm.Context) error {
	s.logger.Printf("Entering suspend-imminent state")
	return nil
}

func (s *Service) EnterHibernateImminent(c *librefsm.Context) error {
	s.logger.Printf("Entering hibernate-imminent state")
	return nil
}

func (s *Service) EnterWaitingInhibitors(c *librefsm.Context) error {
	s.logger.Printf("Entering waiting-for-inhibitors state")

	target := s.fsmData.TargetPowerState

	// Check if we can proceed immediately
	if !s.inhibitorManager.HasBlockingInhibitors(target) {
		c.Send(librefsm.Event{ID: fsm.EvInhibitorsChanged})
	} else if s.hasOnlyModemBlockingInhibitors(target) {
		s.disableModem()
	}

	return nil
}

func (s *Service) EnterIssuingLowPower(c *librefsm.Context) error {
	s.logger.Printf("Entering issuing-low-power state")

	if s.config.DryRun {
		s.logger.Printf("[DRY RUN] Would issue power state %s", s.fsmData.TargetPowerState)
		// In dry-run mode, simulate immediate wakeup for suspend
		if s.fsmData.TargetPowerState == "suspend" {
			c.Send(librefsm.Event{
				ID:      fsm.EvWakeup,
				Payload: fsm.WakeupPayload{Reason: "dry-run"},
			})
		}
		return nil
	}

	var target string
	switch s.fsmData.TargetPowerState {
	case "suspend":
		target = "suspend"
	case "hibernate", "hibernate-manual", "hibernate-timer":
		target = "poweroff"
	case "reboot":
		target = "reboot"
	default:
		return fmt.Errorf("unsupported power state: %s", s.fsmData.TargetPowerState)
	}

	s.fsmData.LowPowerStateIssued = true

	s.logger.Printf("Issuing %s command", target)
	// Note: For suspend, this call blocks until the system wakes up
	if err := s.systemdClient.IssueCommand(target); err != nil {
		s.fsmData.LowPowerStateIssued = false
		return fmt.Errorf("failed to issue %s command: %v", target, err)
	}

	// For suspend, we just woke up - handle wakeup
	if target == "suspend" {
		s.handleWakeupAfterSuspend(c)
	}
	// For poweroff/reboot, the system will have stopped - we won't reach here

	return nil
}

func (s *Service) ExitIssuingLowPower(c *librefsm.Context) error {
	s.delayInhibitorMu.Lock()
	// Cancel any previous pending removal
	if s.cancelDelayRemoval != nil {
		s.cancelDelayRemoval()
	}
	removalCtx, cancel := context.WithCancel(s.ctx)
	s.cancelDelayRemoval = cancel
	s.delayInhibitorMu.Unlock()

	go func() {
		select {
		case <-time.After(s.config.InhibitorDuration):
			s.delayInhibitorMu.Lock()
			defer s.delayInhibitorMu.Unlock()
			if s.delayInhibitor != nil {
				s.inhibitorManager.RemoveInhibitor(s.delayInhibitor)
				s.delayInhibitor = nil
			}
			s.cancelDelayRemoval = nil
		case <-removalCtx.Done():
			return
		}
	}()
	return nil
}

func (s *Service) handleWakeupAfterSuspend(c *librefsm.Context) {
	// Read wakeup reason
	wakeupReason := "unknown"
	if data, err := os.ReadFile("/sys/power/pm_wakeup_irq"); err == nil {
		wakeupReason = strings.TrimSpace(string(data))
	}

	s.fsmData.WakeupReason = wakeupReason
	s.fsmData.LowPowerStateIssued = false
	s.fsmData.ModemDisabled = false

	s.logger.Printf("Wakeup detected with reason: %s", wakeupReason)

	// Cancel any pending delay inhibitor removal and re-add if needed
	s.delayInhibitorMu.Lock()
	if s.cancelDelayRemoval != nil {
		s.cancelDelayRemoval()
		s.cancelDelayRemoval = nil
	}
	if s.delayInhibitor == nil {
		s.delayInhibitor = s.inhibitorManager.AddInhibitor(
			"pm-service",
			"default delay",
			"delay",
			inhibitor.TypeDelay,
		)
	}
	s.delayInhibitorMu.Unlock()

	// Use EvWakeupRTC for RTC wakeup (IRQ 45) to skip pre-suspend delay
	eventID := fsm.EvWakeup
	if wakeupReason == "45" {
		eventID = fsm.EvWakeupRTC
		s.logger.Printf("RTC wakeup detected, using fast path")
	}

	c.Send(librefsm.Event{
		ID:      eventID,
		Payload: fsm.WakeupPayload{Reason: wakeupReason},
	})
}

// Guards

func (s *Service) CanEnterLowPowerState(c *librefsm.Context) bool {
	targetState := s.targetPowerStateFromContext(c)
	vehicleState := s.vehicleStateFromContext(c)

	if targetState == fsm.TargetRun {
		return false
	}

	// Reboot is also allowed during an in-progress shutdown so a reboot command
	// can supersede an ongoing poweroff sequence (librescoot-specific).
	if targetState == fsm.TargetReboot {
		if vehicleState != "stand-by" && vehicleState != "shutting-down" {
			s.logger.Printf("Cannot enter reboot state: vehicle state is %s", vehicleState)
			return false
		}
		return true
	}

	// Only stand-by is a valid LPM entry state.
	if vehicleState != "stand-by" {
		s.logger.Printf("Cannot enter low power state: vehicle state is %s", vehicleState)
		return false
	}

	if targetState == fsm.TargetSuspend && s.batteryStateFromContext(c) == "active" {
		s.logger.Printf("Cannot enter suspend state: battery is active")
		return false
	}

	return true
}

func (s *Service) HasNoBlockingInhibitors(c *librefsm.Context) bool {
	target := s.fsmData.TargetPowerState
	return !s.inhibitorManager.HasBlockingInhibitors(target)
}

func (s *Service) HasOnlyModemInhibitors(c *librefsm.Context) bool {
	target := s.fsmData.TargetPowerState
	return s.hasOnlyModemBlockingInhibitors(target) && !s.fsmData.ModemDisabled
}

func (s *Service) IsVehicleNotInStandby(c *librefsm.Context) bool {
	return s.vehicleStateFromContext(c) != "stand-by"
}

func (s *Service) IsTargetNotRun(c *librefsm.Context) bool {
	return s.targetPowerStateFromContext(c) != fsm.TargetRun
}

func (s *Service) IsBatteryNotActive(c *librefsm.Context) bool {
	return s.fsmData.BatteryState != "active"
}

func (s *Service) IsTargetSuspend(c *librefsm.Context) bool {
	return s.targetPowerStateFromContext(c) == fsm.TargetSuspend
}

func (s *Service) IsTargetHibernate(c *librefsm.Context) bool {
	switch s.targetPowerStateFromContext(c) {
	case fsm.TargetHibernate, fsm.TargetHibernateManual, fsm.TargetHibernateTimer, fsm.TargetReboot:
		return true
	}
	return false
}

// IsPowerCommandHigherPriority checks if the command in the event payload can override
// the current target power state. Guards against priority downgrades.
// Priority order: run > hibernate-manual > hibernate > hibernate-timer > suspend/reboot
func (s *Service) IsPowerCommandHigherPriority(c *librefsm.Context) bool {
	newState := s.targetPowerStateFromContext(c)
	current := s.fsmData.TargetPowerState

	if newState == "run" {
		return true
	}

	switch current {
	case "hibernate-manual":
		if newState == "hibernate" || newState == "hibernate-timer" || newState == "suspend" || newState == "reboot" {
			s.logger.Printf("Power command %s ignored due to priority (current target: %s)", newState, current)
			return false
		}
	case "hibernate":
		if newState == "hibernate-timer" || newState == "suspend" || newState == "reboot" {
			s.logger.Printf("Power command %s ignored due to priority (current target: %s)", newState, current)
			return false
		}
	case "hibernate-timer":
		if newState == "suspend" || newState == "reboot" {
			s.logger.Printf("Power command %s ignored due to priority (current target: %s)", newState, current)
			return false
		}
	}

	return true
}

// Transition actions

func (s *Service) OnPreSuspendTimeout(c *librefsm.Context) error {
	s.logger.Printf("Pre-suspend timeout elapsed")
	return nil
}

func (s *Service) OnSuspendImminentTimeout(c *librefsm.Context) error {
	s.logger.Printf("Suspend imminent timeout elapsed")
	return nil
}

func (s *Service) OnInhibitorsChanged(c *librefsm.Context) error {
	return nil
}

func (s *Service) OnWakeup(c *librefsm.Context) error {
	s.fsmData.LowPowerStateIssued = false
	if payload, ok := c.Event.Payload.(fsm.WakeupPayload); ok {
		s.publishWakeupSource(payload.Reason)
	}
	s.publishState("running")
	return nil
}

func (s *Service) OnDisableModem(c *librefsm.Context) error {
	s.disableModem()
	return nil
}

// OnVehicleStateChanged updates fsmData from the event payload, manages the hibernation
// timer, and resets target power state if the vehicle left standby/parked.
func (s *Service) OnVehicleStateChanged(c *librefsm.Context) error {
	p, ok := c.Event.Payload.(fsm.VehicleStatePayload)
	if !ok {
		return nil
	}

	oldState := s.fsmData.VehicleState
	newState := p.State

	if oldState != newState {
		s.logger.Printf("Vehicle state: %s -> %s", oldState, newState)
	}

	s.fsmData.VehicleState = newState

	// Manage hibernation timer: runs in all idle states (everything except ready-to-drive)
	isActive := newState == "ready-to-drive"
	wasActive := oldState == "ready-to-drive"
	if wasActive && !isActive {
		s.hibernationTimer.ResetTimer(true)
	} else if isActive && !wasActive {
		s.hibernationTimer.ResetTimer(false)
	}

	// Reset target to configured default on either signal:
	//   - leaving stand-by: cancels pending low-power intent but keeps the
	//     target seeded at default so re-entering stand-by re-triggers the
	//     natural low-power path.
	//   - entering ready-to-drive: explicit "user is using the scooter"
	//     signal that cancels any stored intent from an aborted lock-
	//     hibernate (e.g., user tapped lock-hibernate, then unlocked
	//     during the 5s shutting-down window, then drove off).
	if (oldState == "stand-by" && newState != "stand-by") || newState == "ready-to-drive" {
		s.fsmData.TargetPowerState = s.config.DefaultState
		s.fsmData.ModemDisabled = false
	}

	return nil
}

func (s *Service) OnVehicleLeftLowPowerState(c *librefsm.Context) error {
	s.logger.Printf("Vehicle left low power state, aborting")

	// Update vehicle state and manage timer (same as OnVehicleStateChanged)
	if p, ok := c.Event.Payload.(fsm.VehicleStatePayload); ok {
		oldState := s.fsmData.VehicleState
		s.fsmData.VehicleState = p.State

		isActive := p.State == "ready-to-drive"
		wasActive := oldState == "ready-to-drive"
		if wasActive && !isActive {
			s.hibernationTimer.ResetTimer(true)
		} else if isActive && !wasActive {
			s.hibernationTimer.ResetTimer(false)
		}
	}

	s.fsmData.TargetPowerState = s.config.DefaultState
	s.fsmData.ModemDisabled = false
	return nil
}

// OnBatteryStateChanged updates per-slot battery state and derives the aggregate.
func (s *Service) OnBatteryStateChanged(c *librefsm.Context) error {
	if p, ok := c.Event.Payload.(fsm.BatteryStatePayload); ok {
		switch p.Slot {
		case "0":
			s.fsmData.Battery0State = p.State
		case "1":
			s.fsmData.Battery1State = p.State
		}
		if s.fsmData.Battery0State == "active" || s.fsmData.Battery1State == "active" {
			s.fsmData.BatteryState = "active"
		} else {
			s.fsmData.BatteryState = p.State
		}
		s.logger.Printf("Battery %s state -> %s (any active: %v)", p.Slot, p.State, s.fsmData.BatteryState == "active")
	}
	return nil
}

// OnPowerCommand updates fsmData.TargetPowerState from the event payload.
func (s *Service) OnPowerCommand(c *librefsm.Context) error {
	if p, ok := c.Event.Payload.(fsm.PowerCommandPayload); ok {
		if s.fsmData.TargetPowerState != p.TargetState {
			s.logger.Printf("Target power state: %s -> %s", s.fsmData.TargetPowerState, p.TargetState)
		}
		s.fsmData.TargetPowerState = p.TargetState
	}
	return nil
}

// Publishing methods

func (s *Service) PublishState(state string) error {
	return s.publishState(state)
}

func (s *Service) PublishWakeupSource(reason string) error {
	s.publishWakeupSource(reason)
	return nil
}

func (s *Service) publishState(state string) error {
	redisState := s.mapPowerStateToRedis(state)
	s.logger.Printf("Publishing power state: %s (Redis: %s)", state, redisState)

	if err := s.powerManagerPub.Set("state", redisState); err != nil {
		s.logger.Printf("Failed to publish power state: %v", err)
		return err
	}
	return nil
}

func (s *Service) publishFSMState(state librefsm.StateID) {
	var redisState string

	switch state {
	case fsm.StateRunning:
		redisState = "running"
	case fsm.StatePreSuspend:
		redisState = s.mapPowerStateToRedis(s.fsmData.TargetPowerState) + "-pending"
	case fsm.StateSuspendImminent, fsm.StateHibernateImminent:
		redisState = s.mapPowerStateToRedis(s.fsmData.TargetPowerState + "-imminent")
	case fsm.StateWaitingInhibitors:
		redisState = s.mapPowerStateToRedis(s.fsmData.TargetPowerState + "-imminent")
	case fsm.StateIssuingLowPower:
		redisState = s.mapPowerStateToRedis(s.fsmData.TargetPowerState)
	case fsm.StateSuspended:
		redisState = s.mapPowerStateToRedis(s.fsmData.TargetPowerState)
	default:
		// For hibernation states, don't change power-manager state
		return
	}

	if _, err := s.powerManagerPub.SetIfChanged("state", redisState); err != nil {
		s.logger.Printf("Failed to publish FSM state: %v", err)
	}
}

func (s *Service) publishWakeupSource(reason string) {
	s.logger.Printf("Publishing wakeup source: %s", reason)

	if err := s.powerManagerPub.Set("wakeup-source", reason); err != nil {
		s.logger.Printf("Failed to publish wakeup source: %v", err)
	}
}

func (s *Service) publishInhibitors() {
	inhibitors := s.inhibitorManager.GetInhibitors()

	// Build fields map for HashPublisher.ReplaceAll
	fields := make(map[string]any)
	for _, inh := range inhibitors {
		key := fmt.Sprintf("%s %s %s", inh.Who, inh.Why, inh.What)
		fields[key] = string(inh.Type)
	}

	// ReplaceAll does: DEL + HMSET + PUBLISH atomically
	if err := s.busyServicesPub.ReplaceAll(fields); err != nil {
		s.logger.Printf("Failed to publish inhibitors: %v", err)
	}
}

func (s *Service) mapPowerStateToRedis(state string) string {
	switch state {
	case "run":
		return "running"
	case "suspend":
		return "suspending"
	case "hibernate":
		return "hibernating"
	case "hibernate-manual":
		return "hibernating-manual"
	case "hibernate-timer":
		return "hibernating-timer"
	case "reboot":
		return "reboot"
	case "suspend-imminent":
		return "suspending-imminent"
	case "hibernate-imminent":
		return "hibernating-imminent"
	case "hibernate-manual-imminent":
		return "hibernating-manual-imminent"
	case "hibernate-timer-imminent":
		return "hibernating-timer-imminent"
	case "reboot-imminent":
		return "reboot-imminent"
	default:
		return state
	}
}

// Helper methods

func (s *Service) hasOnlyModemBlockingInhibitors(targetPowerState string) bool {
	inhibitors := s.inhibitorManager.GetInhibitors()

	isHibernatePath := targetPowerState == "hibernate" ||
		targetPowerState == "hibernate-manual" ||
		targetPowerState == "hibernate-timer" ||
		targetPowerState == "reboot"

	hasModemInhibitor := false
	hasOtherInhibitors := false

	for _, inh := range inhibitors {
		if inh.Type == inhibitor.TypeSuspendOnly && isHibernatePath {
			continue
		}
		if inh.Type == inhibitor.TypeBlock || inh.Type == inhibitor.TypeSuspendOnly {
			if inh.Who == "modem-service" {
				hasModemInhibitor = true
			} else {
				hasOtherInhibitors = true
			}
		}
	}

	return hasModemInhibitor && !hasOtherInhibitors
}

func (s *Service) disableModem() {
	if s.fsmData.ModemDisabled {
		return
	}

	s.logger.Printf("Issuing modem to turn off")
	if _, err := s.redis.LPush("scooter:modem", "disable"); err != nil {
		s.logger.Printf("Failed to disable modem: %v", err)
		return
	}

	s.fsmData.ModemDisabled = true
}

func (s *Service) setGovernor(governor string) error {
	s.logger.Printf("Setting CPU governor to: %s", governor)

	governorPath := "/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor"
	cmd := exec.Command("sh", "-c", fmt.Sprintf("echo %s > %s", governor, governorPath))
	if err := cmd.Run(); err != nil {
		s.logger.Printf("Failed to set CPU governor to %s: %v", governor, err)
		return fmt.Errorf("failed to set CPU governor to %s: %w", governor, err)
	}

	s.logger.Printf("Successfully set CPU governor to %s", governor)

	if err := s.systemPub.Set("cpu:governor", governor); err != nil {
		s.logger.Printf("Warning: Failed to publish governor change: %v", err)
	}

	return nil
}

func (s *Service) enableWakeupSources() {
	for _, port := range s.config.WakeupSources {
		wakeupPath := fmt.Sprintf("/sys/class/tty/%s/power/wakeup", port)
		if err := os.WriteFile(wakeupPath, []byte("enabled"), 0644); err != nil {
			s.logger.Printf("Warning: cannot enable wakeup for %s: %v", port, err)
		} else {
			s.logger.Printf("Enabled wakeup on %s", port)
		}
	}
}

func (s *Service) onHibernationTimerSetting(value string) error {
	timerValue, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		s.logger.Printf("Failed to parse hibernation timer setting: %v", err)
		return nil
	}
	s.hibernationTimer.SetTimerValue(int32(timerValue))
	s.logger.Printf("Updated hibernation timer setting: %d seconds", timerValue)
	return nil
}

func (s *Service) onDefaultStateSetting(value string) error {
	if !isValidDefaultState(value) {
		s.logger.Printf("Ignoring invalid pm.default-state setting: %q", value)
		return nil
	}
	if s.config.DefaultState == value {
		return nil
	}
	s.logger.Printf("Default power state changed: %s -> %s", s.config.DefaultState, value)
	s.config.DefaultState = value
	return nil
}

func isValidDefaultState(state string) bool {
	switch state {
	case fsm.TargetRun, fsm.TargetSuspend, fsm.TargetHibernate,
		fsm.TargetHibernateManual, fsm.TargetHibernateTimer, fsm.TargetReboot:
		return true
	}
	return false
}
