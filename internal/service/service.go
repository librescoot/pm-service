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
	"time"

	"github.com/librescoot/librefsm"
	"github.com/librescoot/pm-service/internal/config"
	"github.com/librescoot/pm-service/internal/fsm"
	"github.com/librescoot/pm-service/internal/hibernation"
	"github.com/librescoot/pm-service/internal/inhibitor"
	"github.com/librescoot/pm-service/internal/systemd"
	"github.com/redis/go-redis/v9"
	redis_ipc "github.com/librescoot/redis-ipc"
)

type Service struct {
	config           *config.Config
	logger           *log.Logger
	redis            *redis_ipc.Client
	standardRedis    *redis.Client
	inhibitorManager *inhibitor.Manager
	hibernationTimer *hibernation.Timer
	systemdClient    *systemd.Client
	delayInhibitor   *inhibitor.Inhibitor
	powerManagerPub  *redis_ipc.HashPublisher
	systemPub        *redis_ipc.HashPublisher

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

	ctx := context.Background()
	standardRedisClient := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", cfg.RedisHost, cfg.RedisPort),
		DB:   0,
	})

	systemdClient, err := systemd.NewClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create systemd client: %v", err)
	}

	service := &Service{
		config:          cfg,
		logger:          logger,
		redis:           redisClient,
		standardRedis:   standardRedisClient,
		systemdClient:   systemdClient,
		powerManagerPub: redisClient.NewHashPublisher("power-manager"),
		systemPub:       redisClient.NewHashPublisher("system"),
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

	// Create hibernation timer
	hibernationTimer := hibernation.NewTimer(
		ctx,
		service.standardRedis,
		logger,
		cfg.HibernationTimer,
		func() {
			// Send FSM event when timer expires
			if service.machine != nil {
				service.fsmData.TargetPowerState = fsm.TargetHibernateTimer
				service.machine.Send(librefsm.Event{ID: fsm.EvHibernationTimerExpired})
			}
		},
	)
	service.hibernationTimer = hibernationTimer

	return service, nil
}

func (s *Service) Run(ctx context.Context) error {
	// Enable wakeup on configured serial ports
	s.enableWakeupSources()

	// Set up Redis hash watchers with initial sync
	// StartWithSync fetches current state and calls handlers before processing new messages
	if err := s.redis.NewHashWatcher("vehicle").
		OnField("state", s.onVehicleStateChanged).
		StartWithSync(); err != nil {
		return fmt.Errorf("failed to start vehicle state watcher: %v", err)
	}

	if err := s.redis.NewHashWatcher("battery:0").
		OnField("state", s.onBatteryStateChanged).
		StartWithSync(); err != nil {
		return fmt.Errorf("failed to start battery state watcher: %v", err)
	}

	redis_ipc.HandleRequests(s.redis, "scooter:power", s.onPowerCommand)
	redis_ipc.HandleRequests(s.redis, "scooter:governor", s.onGovernorCommand)

	// Start hibernation timer settings listener
	go s.listenForHibernationSettings(ctx, s.standardRedis)

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

	// Initialize hibernation timer if vehicle is not actively being used
	if s.fsmData.VehicleState != "ready-to-drive" {
		s.hibernationTimer.ResetTimer(true)
		s.logger.Printf("Initialized hibernation timer - vehicle in idle state: %s", s.fsmData.VehicleState)
	}

	// Trigger low-power sequence evaluation if default state is not "run"
	// The FSM transition guards will determine if conditions allow
	if s.fsmData.TargetPowerState != fsm.TargetRun {
		s.machine.Send(librefsm.Event{
			ID:      fsm.EvVehicleStateChanged,
			Payload: fsm.VehicleStatePayload{State: s.fsmData.VehicleState},
		})
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

func (s *Service) readInitialStates() error {
	const maxRetries = 10
	const retryDelay = 500 * time.Millisecond

	s.logger.Printf("Reading initial vehicle and battery states from Redis...")

	for i := range maxRetries {
		vehicleState, vehicleErr := s.redis.HGet("vehicle", "state")
		batteryState, batteryErr := s.redis.HGet("battery:0", "state")

		if vehicleErr == nil && batteryErr == nil {
			s.fsmData.VehicleState = vehicleState
			s.fsmData.BatteryState = batteryState
			s.logger.Printf("Successfully read initial states - Vehicle: %s, Battery: %s", vehicleState, batteryState)
			return nil
		}

		if i < maxRetries-1 {
			s.logger.Printf("Failed to read initial states (attempt %d/%d) - Vehicle error: %v, Battery error: %v. Retrying in %v...",
				i+1, maxRetries, vehicleErr, batteryErr, retryDelay)
			time.Sleep(retryDelay)
		}
	}

	s.fsmData.VehicleState = "initializing"
	s.fsmData.BatteryState = "initializing"
	s.logger.Printf("WARNING: Failed to read initial states from Redis after %d attempts. Using safe default state 'initializing'.", maxRetries)

	return nil
}

// Redis handlers - send FSM events

func (s *Service) onVehicleStateChanged(vehicleState string) error {
	oldState := s.fsmData.VehicleState
	s.fsmData.VehicleState = vehicleState

	// Only log when state actually changes
	if oldState != vehicleState {
		s.logger.Printf("Vehicle state: %s -> %s", oldState, vehicleState)
	}

	// Update hibernation timer based on vehicle state
	// Timer runs in all unattended/idle states (everything except ready-to-drive)
	// Timer resets when leaving ready-to-drive (user finished using scooter)
	isActive := vehicleState == "ready-to-drive"
	wasActive := oldState == "ready-to-drive"

	if wasActive && !isActive {
		// Just left ready-to-drive - reset timer for fresh countdown
		s.hibernationTimer.ResetTimer(true)
	} else if isActive && !wasActive {
		// Entering ready-to-drive - stop timer
		s.hibernationTimer.ResetTimer(false)
	} else if !isActive && oldState == "" {
		// Bootstrap: starting in idle state - start timer
		s.hibernationTimer.ResetTimer(true)
	}
	// Else: transitions between idle states (timer continues) or staying in ready-to-drive

	// Send FSM event
	if s.machine != nil {
		s.machine.Send(librefsm.Event{
			ID:      fsm.EvVehicleStateChanged,
			Payload: fsm.VehicleStatePayload{State: vehicleState},
		})
	}

	// If vehicle left standby/parked, cancel any pending low power state
	if (oldState == "stand-by" || oldState == "parked") && vehicleState != "stand-by" && vehicleState != "parked" {
		s.fsmData.TargetPowerState = fsm.TargetRun
		s.fsmData.ModemDisabled = false
	}

	return nil
}

func (s *Service) onBatteryStateChanged(newState string) error {
	oldState := s.fsmData.BatteryState
	s.fsmData.BatteryState = newState

	// Only log when state actually changes
	if oldState != newState {
		s.logger.Printf("Battery state: %s -> %s", oldState, newState)
	}

	if s.machine != nil {
		// Send semantic battery events
		if oldState != "active" && newState == "active" {
			s.machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameActive})
		} else if oldState == "active" && newState != "active" {
			s.machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameInactive})
		}
	}

	return nil
}

func (s *Service) onPowerCommand(command string) error {
	s.logger.Printf("Received power command: %s", command)

	// Check priority and update target state
	if !s.canSetTargetState(command) {
		s.logger.Printf("Power command %s ignored due to priority (current target: %s)", command, s.fsmData.TargetPowerState)
		return nil
	}

	s.fsmData.TargetPowerState = command

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
		s.machine.Send(librefsm.Event{ID: eventID})
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

// Priority-based power state setting
func (s *Service) canSetTargetState(newState string) bool {
	current := s.fsmData.TargetPowerState

	// Run can always be set
	if newState == "run" {
		return true
	}

	// Priority order: run > hibernate-manual > hibernate > hibernate-timer > suspend/reboot
	switch current {
	case "hibernate-manual":
		if newState == "hibernate" || newState == "hibernate-timer" || newState == "suspend" || newState == "reboot" {
			return false
		}
	case "hibernate":
		if newState == "hibernate-timer" || newState == "suspend" || newState == "reboot" {
			return false
		}
	case "hibernate-timer":
		if newState == "suspend" || newState == "reboot" {
			return false
		}
	}

	return true
}

// Actions interface implementation

func (s *Service) EnterRunning(c *librefsm.Context) error {
	s.logger.Printf("Entering running state")
	s.fsmData.LowPowerStateIssued = false
	return nil
}

func (s *Service) EnterPreSuspend(c *librefsm.Context) error {
	s.logger.Printf("Entering pre-suspend state, waiting %v", s.config.PreSuspendDelay)
	return nil
}

func (s *Service) EnterSuspendImminent(c *librefsm.Context) error {
	s.logger.Printf("Entering suspend-imminent state")
	// Publish imminent state
	s.publishState(s.fsmData.TargetPowerState + "-imminent")
	return nil
}

func (s *Service) EnterPreHibernate(c *librefsm.Context) error {
	s.logger.Printf("Entering pre-hibernate state, waiting %v", s.config.PreSuspendDelay)
	return nil
}

func (s *Service) EnterHibernateImminent(c *librefsm.Context) error {
	s.logger.Printf("Entering hibernate-imminent state")
	// Publish imminent state
	s.publishState(s.fsmData.TargetPowerState + "-imminent")
	return nil
}

func (s *Service) EnterWaitingInhibitors(c *librefsm.Context) error {
	s.logger.Printf("Entering waiting-for-inhibitors state")

	// Check if we can proceed immediately
	if !s.inhibitorManager.HasBlockingInhibitors() {
		// No blocking inhibitors, proceed to issuing
		c.Send(librefsm.Event{ID: fsm.EvInhibitorsChanged})
	} else if s.hasOnlyModemBlockingInhibitors() {
		// Only modem inhibitors, try to disable modem
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
	s.publishState(s.fsmData.TargetPowerState)

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
	// Schedule removal of delay inhibitor
	go func() {
		time.Sleep(s.config.InhibitorDuration)
		if s.delayInhibitor != nil {
			s.inhibitorManager.RemoveInhibitor(s.delayInhibitor)
			s.delayInhibitor = nil
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

	// Re-add delay inhibitor
	if s.delayInhibitor == nil {
		s.delayInhibitor = s.inhibitorManager.AddInhibitor(
			"pm-service",
			"default delay",
			"delay",
			inhibitor.TypeDelay,
		)
	}

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
	if s.fsmData.TargetPowerState == fsm.TargetRun {
		return false
	}

	// Special case for reboot - allow in both stand-by and shutting-down states
	if s.fsmData.TargetPowerState == fsm.TargetReboot {
		if s.fsmData.VehicleState != "stand-by" && s.fsmData.VehicleState != "shutting-down" {
			s.logger.Printf("Cannot enter reboot state: vehicle state is %s", s.fsmData.VehicleState)
			return false
		}
		return true
	}

	if s.fsmData.VehicleState != "stand-by" && s.fsmData.VehicleState != "parked" && s.fsmData.VehicleState != "shutting-down" {
		s.logger.Printf("Cannot enter low power state: vehicle state is %s", s.fsmData.VehicleState)
		return false
	}

	if s.fsmData.TargetPowerState == fsm.TargetSuspend && s.fsmData.BatteryState == "active" {
		s.logger.Printf("Cannot enter suspend state: battery is active")
		return false
	}

	return true
}

func (s *Service) HasNoBlockingInhibitors(c *librefsm.Context) bool {
	return !s.inhibitorManager.HasBlockingInhibitors()
}

func (s *Service) HasOnlyModemInhibitors(c *librefsm.Context) bool {
	return s.hasOnlyModemBlockingInhibitors() && !s.fsmData.ModemDisabled
}

func (s *Service) IsVehicleInStandbyOrParked(c *librefsm.Context) bool {
	return s.fsmData.VehicleState == "stand-by" || s.fsmData.VehicleState == "parked"
}

func (s *Service) IsVehicleNotInStandbyOrParked(c *librefsm.Context) bool {
	return s.fsmData.VehicleState != "stand-by" && s.fsmData.VehicleState != "parked"
}

func (s *Service) IsTargetNotRun(c *librefsm.Context) bool {
	return s.fsmData.TargetPowerState != fsm.TargetRun
}

func (s *Service) IsBatteryNotActive(c *librefsm.Context) bool {
	return s.fsmData.BatteryState != "active"
}

func (s *Service) IsTargetSuspend(c *librefsm.Context) bool {
	return s.fsmData.TargetPowerState == fsm.TargetSuspend
}

func (s *Service) IsTargetHibernate(c *librefsm.Context) bool {
	// Hibernate includes all non-suspend low-power states except run
	switch s.fsmData.TargetPowerState {
	case fsm.TargetHibernate, fsm.TargetHibernateManual, fsm.TargetHibernateTimer, fsm.TargetReboot:
		return true
	}
	return false
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

func (s *Service) OnVehicleLeftLowPowerState(c *librefsm.Context) error {
	s.logger.Printf("Vehicle left low power state, aborting")
	s.fsmData.TargetPowerState = fsm.TargetRun
	s.fsmData.ModemDisabled = false
	s.publishState("running")
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
	case fsm.StatePreSuspend, fsm.StatePreHibernate:
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

	if err := s.powerManagerPub.Set("state", redisState); err != nil {
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
	busyServicesPub := s.redis.NewHashPublisher("power-manager:busy-services")
	if err := busyServicesPub.ReplaceAll(fields); err != nil {
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

func (s *Service) hasOnlyModemBlockingInhibitors() bool {
	inhibitors := s.inhibitorManager.GetInhibitors()

	hasModemInhibitor := false
	hasOtherInhibitors := false

	for _, inh := range inhibitors {
		if inh.Type == inhibitor.TypeBlock {
			if inh.Who == "unu-modem" || inh.Who == "modem-service" {
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

func (s *Service) listenForHibernationSettings(ctx context.Context, redis *redis.Client) {
	pubsub := redis.Subscribe(ctx, "settings")
	defer pubsub.Close()

	s.loadHibernationTimerSetting(redis)

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			if msg == nil {
				s.logger.Printf("Redis settings channel closed unexpectedly")
				log.Fatalf("Redis connection lost, exiting to allow systemd restart")
			}
			if msg.Payload == "hibernation-timer" {
				s.loadHibernationTimerSetting(redis)
			}
		}
	}
}

func (s *Service) loadHibernationTimerSetting(redis *redis.Client) {
	result := redis.HGet(context.Background(), "settings", "hibernation-timer")
	if result.Err() == nil {
		if timerValue, err := strconv.ParseInt(result.Val(), 10, 32); err == nil {
			s.hibernationTimer.SetTimerValue(int32(timerValue))
			s.logger.Printf("Updated hibernation timer setting: %d seconds", timerValue)
		} else {
			s.logger.Printf("Failed to parse hibernation timer setting: %v", err)
		}
	}
}
