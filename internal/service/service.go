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
	redis_ipc "github.com/rescoot/redis-ipc"
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

	// librefsm
	machine *librefsm.Machine
	fsmData *fsm.FSMData
}

func New(cfg *config.Config, logger *log.Logger) (*Service, error) {
	redisConfig := redis_ipc.Config{
		Address:       cfg.RedisHost,
		Port:          cfg.RedisPort,
		RetryInterval: 5 * time.Second,
		MaxRetries:    3,
	}

	redisClient, err := redis_ipc.New(redisConfig)
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
		config:        cfg,
		logger:        logger,
		redis:         redisClient,
		standardRedis: standardRedisClient,
		systemdClient: systemdClient,
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

	// Set up Redis subscriptions
	vehicleSubscriber := s.redis.Subscribe("vehicle")
	if err := vehicleSubscriber.Handle("state", s.onVehicleState); err != nil {
		return fmt.Errorf("failed to subscribe to vehicle state: %v", err)
	}

	batterySubscriber := s.redis.Subscribe("battery:0")
	if err := batterySubscriber.Handle("state", s.onBatteryState); err != nil {
		return fmt.Errorf("failed to subscribe to battery state: %v", err)
	}

	s.redis.HandleRequests("scooter:power", s.onPowerCommand)
	s.redis.HandleRequests("scooter:governor", s.onGovernorCommand)

	// Subscribe to hibernation inputs (for manual hibernation sequence)
	buttonsSubscriber := s.redis.Subscribe("buttons")
	if err := buttonsSubscriber.Handle("hibernation_input", s.onHibernationInput); err != nil {
		s.logger.Printf("Warning: failed to subscribe to hibernation input: %v", err)
	}

	seatboxSubscriber := s.redis.Subscribe("seatbox")
	if err := seatboxSubscriber.Handle("state", s.onSeatboxState); err != nil {
		s.logger.Printf("Warning: failed to subscribe to seatbox state: %v", err)
	}

	// Start hibernation timer settings listener
	go s.listenForHibernationSettings(ctx, s.standardRedis)

	// Read initial states with retries
	if err := s.readInitialStates(); err != nil {
		return fmt.Errorf("failed to read initial states from Redis: %v", err)
	}

	// Publish initial power state
	s.publishFSMState(s.machine.CurrentState())

	// Initialize hibernation timer based on initial vehicle state
	if s.fsmData.VehicleState == "stand-by" || s.fsmData.VehicleState == "parked" {
		s.hibernationTimer.ResetTimer(true)
		s.logger.Printf("Initialized hibernation timer based on initial vehicle state: %s", s.fsmData.VehicleState)
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

func (s *Service) onVehicleState(data []byte) error {
	vehicleState, err := s.redis.HGet("vehicle", "state")
	if err != nil {
		return fmt.Errorf("failed to get vehicle state: %v", err)
	}

	oldState := s.fsmData.VehicleState
	s.fsmData.VehicleState = vehicleState
	s.logger.Printf("Vehicle state: %s", vehicleState)

	// Update hibernation timer based on vehicle state
	if vehicleState == "stand-by" || vehicleState == "parked" {
		s.hibernationTimer.ResetTimer(true)
	} else {
		s.hibernationTimer.ResetTimer(false)
	}

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

func (s *Service) onBatteryState(data []byte) error {
	batteryState, err := s.redis.HGet("battery:0", "state")
	if err != nil {
		return fmt.Errorf("failed to get battery state: %v", err)
	}

	s.fsmData.BatteryState = batteryState
	s.logger.Printf("Battery state: %s", batteryState)

	if s.machine != nil {
		s.machine.Send(librefsm.Event{
			ID:      fsm.EvBatteryStateChanged,
			Payload: fsm.BatteryStatePayload{State: batteryState},
		})
	}

	return nil
}

func (s *Service) onPowerCommand(data []byte) error {
	command := string(data)
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

func (s *Service) onHibernationInput(data []byte) error {
	value := string(data)

	if s.machine != nil {
		switch value {
		case "pressed":
			// Check if we should start hibernation sequence
			if s.machine.CurrentState() == fsm.StateRunning {
				s.machine.Send(librefsm.Event{ID: fsm.EvHibernationStart})
			} else if s.machine.IsInState(fsm.StateHibernationConfirm) {
				s.machine.Send(librefsm.Event{ID: fsm.EvHibernationInputPressed})
			}
		case "released":
			s.machine.Send(librefsm.Event{ID: fsm.EvHibernationInputReleased})
		}
	}

	return nil
}

func (s *Service) onSeatboxState(data []byte) error {
	state, err := s.redis.HGet("seatbox", "state")
	if err != nil {
		return err
	}

	if state == "closed" && s.machine != nil {
		s.machine.Send(librefsm.Event{ID: fsm.EvSeatboxClosed})
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

func (s *Service) onGovernorCommand(data []byte) error {
	governor := string(data)
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

func (s *Service) EnterHibernation(c *librefsm.Context) error {
	s.logger.Printf("Entering hibernation sequence")
	return nil
}

func (s *Service) ExitHibernation(c *librefsm.Context) error {
	s.logger.Printf("Exiting hibernation sequence")
	s.publishHibernationState("idle")
	return nil
}

func (s *Service) EnterHibernationWaiting(c *librefsm.Context) error {
	s.logger.Printf("Hibernation: waiting for initial hold (15s)")
	s.publishHibernationState("waiting-hibernation")
	return nil
}

func (s *Service) EnterHibernationAdvanced(c *librefsm.Context) error {
	s.logger.Printf("Hibernation: advanced state (10s)")
	s.publishHibernationState("waiting-hibernation-advanced")
	return nil
}

func (s *Service) EnterHibernationSeatbox(c *librefsm.Context) error {
	s.logger.Printf("Hibernation: waiting for seatbox closure (60s)")
	s.publishHibernationState("waiting-hibernation-seatbox")
	return nil
}

func (s *Service) EnterHibernationConfirm(c *librefsm.Context) error {
	s.logger.Printf("Hibernation: waiting for confirmation (3s)")
	s.publishHibernationState("waiting-hibernation-confirm")
	return nil
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

	if s.fsmData.VehicleState != "stand-by" && s.fsmData.VehicleState != "parked" {
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

func (s *Service) IsBatteryBlockingSuspend(c *librefsm.Context) bool {
	return s.fsmData.TargetPowerState == fsm.TargetSuspend && s.fsmData.BatteryState == "active"
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

func (s *Service) OnHibernationComplete(c *librefsm.Context) error {
	s.logger.Printf("Hibernation sequence complete, setting target to hibernate-manual")
	s.fsmData.TargetPowerState = fsm.TargetHibernateManual
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

	tx := s.redis.NewTxGroup("power-state")
	tx.Add("HSET", "power-manager", "state", redisState)
	tx.Add("PUBLISH", "power-manager", "state")

	if _, err := tx.Exec(); err != nil {
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
	case fsm.StateSuspendImminent:
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

	tx := s.redis.NewTxGroup("power-state")
	tx.Add("HSET", "power-manager", "state", redisState)
	tx.Add("PUBLISH", "power-manager", "state")

	if _, err := tx.Exec(); err != nil {
		s.logger.Printf("Failed to publish FSM state: %v", err)
	}
}

func (s *Service) publishHibernationState(state string) {
	pipe := s.standardRedis.Pipeline()
	pipe.HSet(context.Background(), "hibernation", "state", state)
	pipe.Publish(context.Background(), "hibernation", "state")
	if _, err := pipe.Exec(context.Background()); err != nil {
		s.logger.Printf("Warning: Failed to publish hibernation state: %v", err)
	}
}

func (s *Service) publishWakeupSource(reason string) {
	s.logger.Printf("Publishing wakeup source: %s", reason)

	tx := s.redis.NewTxGroup("wakeup-source")
	tx.Add("HSET", "power-manager", "wakeup-source", reason)
	tx.Add("PUBLISH", "power-manager", "wakeup-source")

	if _, err := tx.Exec(); err != nil {
		s.logger.Printf("Failed to publish wakeup source: %v", err)
	}
}

func (s *Service) publishInhibitors() {
	inhibitors := s.inhibitorManager.GetInhibitors()

	tx := s.redis.NewTxGroup("inhibitors")
	tx.Add("DEL", "power-manager:busy-services")

	for _, inh := range inhibitors {
		tx.Add("HSET", "power-manager:busy-services",
			fmt.Sprintf("%s %s %s", inh.Who, inh.Why, inh.What),
			string(inh.Type))
	}

	tx.Add("PUBLISH", "power-manager:busy-services", "updated")

	if _, err := tx.Exec(); err != nil {
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

	tx := s.redis.NewTxGroup("governor")
	tx.Add("HSET", "system", "cpu:governor", governor)
	tx.Add("PUBLISH", "system", "cpu:governor")

	if _, err := tx.Exec(); err != nil {
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
