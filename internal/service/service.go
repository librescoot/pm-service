package service

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/librescoot/pm-service/internal/config"
	"github.com/librescoot/pm-service/internal/hibernation"
	"github.com/librescoot/pm-service/internal/inhibitor"
	"github.com/librescoot/pm-service/internal/power"
	"github.com/redis/go-redis/v9"
	redis_ipc "github.com/rescoot/redis-ipc"
)

type Service struct {
	config              *config.Config
	logger              *log.Logger
	redis               *redis_ipc.Client
	standardRedis       *redis.Client
	powerManager        *power.Manager
	inhibitorManager    *inhibitor.Manager
	hibernationSM       *hibernation.StateMachine
	hibernationTimer    *hibernation.Timer
	hibernationListener *hibernation.RedisListener
	delayInhibitor      *inhibitor.Inhibitor

	events chan Event

	vehicleState string
	batteryState string

	preSuspendTimer      *time.Timer
	suspendImminentTimer *time.Timer

	lpmImminentTimerElapsed bool
	modemDisabled           bool
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

	// Create standard Redis client for hardware manager and hibernation timer
	ctx := context.Background()
	standardRedisClient := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", cfg.RedisHost, cfg.RedisPort),
		DB:   0,
	})

	service := &Service{
		config:                  cfg,
		logger:                  logger,
		redis:                   redisClient,
		standardRedis:           standardRedisClient,
		events:                  make(chan Event, 100),
		vehicleState:            "",
		batteryState:            "",
		lpmImminentTimerElapsed: false,
		modemDisabled:           false,
	}

	powerManager, err := power.NewManager(
		logger,
		cfg.DryRun,
		service.onLowPowerStateEnter,
		service.onLowPowerStateExit,
		service.onWakeup,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create power manager: %v", err)
	}
	service.powerManager = powerManager

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

	// Create hibernation state machine
	hibernationSM := hibernation.NewStateMachine(ctx, service.standardRedis, logger, func() {
		// Callback when hibernation sequence completes
		service.events <- Event{
			Type: EventHibernationComplete,
			Data: nil,
		}
	})
	service.hibernationSM = hibernationSM

	// Create hibernation timer
	hibernationTimer := hibernation.NewTimer(
		ctx,
		service.standardRedis,
		logger,
		cfg.HibernationTimer, // Default hibernation timer duration
		func() {
			// Execute hibernation when timer expires
			service.events <- Event{
				Type: EventHibernationTimerExpired,
				Data: nil,
			}
		},
	)
	service.hibernationTimer = hibernationTimer

	// Create hibernation Redis listener
	hibernationListener := hibernation.NewRedisListener(ctx, service.standardRedis, hibernationSM, logger)
	service.hibernationListener = hibernationListener

	switch cfg.DefaultState {
	case "run":
		service.powerManager.SetTargetState(power.StateRun)
	case "suspend":
		service.powerManager.SetTargetState(power.StateSuspend)
	case "hibernate":
		service.powerManager.SetTargetState(power.StateHibernate)
	case "hibernate-manual":
		service.powerManager.SetTargetState(power.StateHibernateManual)
	case "hibernate-timer":
		service.powerManager.SetTargetState(power.StateHibernateTimer)
	case "reboot":
		service.powerManager.SetTargetState(power.StateReboot)
	default:
		service.powerManager.SetTargetState(power.StateSuspend)
	}

	return service, nil
}

func (s *Service) Run(ctx context.Context) error {
	// Enable wakeup on configured serial ports
	s.enableWakeupSources()

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

	// Start hibernation Redis listener
	if err := s.hibernationListener.Start(); err != nil {
		return fmt.Errorf("failed to start hibernation listener: %v", err)
	}

	// Start hibernation timer settings listener
	go s.listenForHibernationSettings(ctx, s.standardRedis)

	// Read initial states with retries
	if err := s.readInitialStates(); err != nil {
		return fmt.Errorf("failed to read initial states from Redis: %v", err)
	}

	// Set initial power manager state in Redis based on current target state
	currentTargetState := s.powerManager.GetTargetState()
	s.publishState(string(currentTargetState))

	if s.canEnterLowPowerState() {
		s.startPreSuspendTimer()
	}

	// Run event loop
	s.eventLoop(ctx)

	if s.preSuspendTimer != nil {
		s.preSuspendTimer.Stop()
	}
	if s.suspendImminentTimer != nil {
		s.suspendImminentTimer.Stop()
	}

	// Stop listeners
	s.hibernationListener.Stop()

	if err := s.powerManager.Close(); err != nil {
		s.logger.Printf("Failed to close power manager: %v", err)
	}

	if err := s.inhibitorManager.Close(); err != nil {
		s.logger.Printf("Failed to close inhibitor manager: %v", err)
	}

	s.hibernationSM.Close()
	s.hibernationTimer.Close()

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
			s.vehicleState = vehicleState
			s.batteryState = batteryState
			s.logger.Printf("Successfully read initial states - Vehicle: %s, Battery: %s", vehicleState, batteryState)
			return nil
		}

		if i < maxRetries-1 {
			s.logger.Printf("Failed to read initial states (attempt %d/%d) - Vehicle error: %v, Battery error: %v. Retrying in %v...",
				i+1, maxRetries, vehicleErr, batteryErr, retryDelay)
			time.Sleep(retryDelay)
		}
	}

	// After all retries failed, use safe default states that block power actions
	s.vehicleState = "initializing"
	s.batteryState = "initializing"
	s.logger.Printf("WARNING: Failed to read initial states from Redis after %d attempts. Using safe default state 'initializing' which blocks all power actions until real state is received via subscription.", maxRetries)

	return nil
}

func (s *Service) onVehicleState(data []byte) error {
	vehicleState, err := s.redis.HGet("vehicle", "state")
	if err != nil {
		return fmt.Errorf("failed to get vehicle state: %v", err)
	}

	s.events <- Event{
		Type: EventVehicleState,
		Data: VehicleStateData{State: vehicleState},
	}

	return nil
}

func (s *Service) onBatteryState(data []byte) error {
	batteryState, err := s.redis.HGet("battery:0", "state")
	if err != nil {
		return fmt.Errorf("failed to get battery state: %v", err)
	}

	s.events <- Event{
		Type: EventBatteryState,
		Data: BatteryStateData{State: batteryState},
	}

	return nil
}

func (s *Service) onPowerCommand(data []byte) error {
	command := string(data)

	s.events <- Event{
		Type: EventPowerCommand,
		Data: PowerCommandData{Command: command},
	}

	return nil
}

func (s *Service) onInhibitorsChanged() {
	s.events <- Event{
		Type: EventInhibitorsChanged,
		Data: nil,
	}
}

func (s *Service) onLowPowerStateEnter(state power.PowerState) {
	s.events <- Event{
		Type: EventLowPowerStateEnter,
		Data: LowPowerStateEnterData{State: string(state)},
	}
}

func (s *Service) onWakeup(wakeupReason string) {
	s.events <- Event{
		Type: EventWakeup,
		Data: WakeupData{Reason: wakeupReason},
	}
}

func (s *Service) onLowPowerStateExit() {
	s.events <- Event{
		Type: EventLowPowerStateExit,
		Data: nil,
	}
}

// eventLoop processes all events sequentially, owning all Service state
func (s *Service) eventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-s.events:
			s.handleEvent(evt)
		}
	}
}

// handleEvent dispatches events to appropriate handlers
func (s *Service) handleEvent(evt Event) {
	switch evt.Type {
	case EventPowerCommand:
		data := evt.Data.(PowerCommandData)
		s.handlePowerCommand(data.Command)
	case EventGovernorCommand:
		data := evt.Data.(GovernorCommandData)
		s.handleGovernorCommand(data.Governor)
	case EventVehicleState:
		data := evt.Data.(VehicleStateData)
		s.handleVehicleStateChange(data.State)
	case EventBatteryState:
		data := evt.Data.(BatteryStateData)
		s.handleBatteryStateChange(data.State)
	case EventInhibitorsChanged:
		s.handleInhibitorsChanged()
	case EventPreSuspendElapsed:
		s.handlePreSuspendElapsed()
	case EventSuspendImminentElapsed:
		s.handleSuspendImminentElapsed()
	case EventLowPowerStateEnter:
		data := evt.Data.(LowPowerStateEnterData)
		s.handleLowPowerStateEnter(data.State)
	case EventLowPowerStateExit:
		s.handleLowPowerStateExit()
	case EventWakeup:
		data := evt.Data.(WakeupData)
		s.handleWakeup(data.Reason)
	case EventHibernationTimerExpired:
		s.handleHibernationTimerExpired()
	case EventHibernationComplete:
		s.handleHibernationComplete()
	case EventHibernationSettingsChanged:
		data := evt.Data.(HibernationSettingsData)
		s.handleHibernationSettingsChanged(data.TimerValue)
	case EventDelayInhibitorRemove:
		s.handleDelayInhibitorRemove()
	}
}

// handlePowerCommand processes power command events
func (s *Service) handlePowerCommand(command string) {
	s.logger.Printf("Received power command: %s", command)

	s.logger.Printf("Current state - Vehicle: %s, Battery: %s, Target power: %s",
		s.vehicleState, s.batteryState, s.powerManager.GetTargetState())

	switch command {
	case "run":
		s.powerManager.SetTargetState(power.StateRun)
	case "suspend":
		s.powerManager.SetTargetState(power.StateSuspend)
	case "hibernate":
		s.powerManager.SetTargetState(power.StateHibernate)
	case "hibernate-manual":
		s.powerManager.SetTargetState(power.StateHibernateManual)
	case "hibernate-timer":
		s.powerManager.SetTargetState(power.StateHibernateTimer)
	case "reboot":
		s.powerManager.SetTargetState(power.StateReboot)
	default:
		s.logger.Printf("Unknown power command: %s", command)
		return
	}

	canEnter := s.canEnterLowPowerState()
	isIssued := s.powerManager.IsLowPowerStateIssued()
	targetState := s.powerManager.GetTargetState()

	s.logger.Printf("Power state check - Can enter low power: %v, Is issued: %v, Target state: %s",
		canEnter, isIssued, targetState)

	if canEnter && !isIssued && targetState != power.StateRun {
		s.logger.Printf("Starting suspend imminent timer for target state: %s", targetState)
		s.startSuspendImminentTimer()
	} else {
		s.logger.Printf("Not starting suspend imminent timer, conditions not met")
	}
}

// handleGovernorCommand processes CPU governor command events
func (s *Service) handleGovernorCommand(governor string) {
	s.logger.Printf("Received governor command: %s", governor)

	// Validate governor value
	switch governor {
	case "ondemand", "powersave", "performance":
		// Valid governors
	default:
		s.logger.Printf("Invalid governor command: %s", governor)
		return
	}

	s.setGovernor(governor)
}

// handleVehicleStateChange processes vehicle state change events
func (s *Service) handleVehicleStateChange(newState string) {
	oldVehicleState := s.vehicleState
	s.vehicleState = newState

	s.logger.Printf("Vehicle state: %s", newState)

	if (oldVehicleState == "stand-by" || oldVehicleState == "parked") && newState != "stand-by" && newState != "parked" {
		s.logger.Printf("Vehicle state changed from %s to %s, aborting low power mode", oldVehicleState, newState)
		s.powerManager.SetTargetState(power.StateRun)
		s.stopSuspendImminentTimer()
		s.stopPreSuspendTimer()

		s.modemDisabled = false

		s.publishState(string(power.StateRun))
	}

	if s.canEnterLowPowerState() {
		s.startPreSuspendTimer()
	}

	if newState == "stand-by" || newState == "parked" {
		s.hibernationTimer.ResetTimer(true)
	} else {
		s.hibernationTimer.ResetTimer(false)
	}
}

// handleBatteryStateChange processes battery state change events
func (s *Service) handleBatteryStateChange(newState string) {
	s.batteryState = newState

	s.logger.Printf("Battery state: %s", newState)

	if s.canEnterLowPowerState() {
		s.startPreSuspendTimer()
	} else {
		s.stopPreSuspendTimer()
		s.stopSuspendImminentTimer()
	}
}

// handleInhibitorsChanged processes inhibitor change events
func (s *Service) handleInhibitorsChanged() {
	inhibitors := s.inhibitorManager.GetInhibitors()

	tx := s.redis.NewTxGroup("inhibitors")

	tx.Add("DEL", "power-manager:busy-services")

	for _, inhibitor := range inhibitors {
		tx.Add("HSET", "power-manager:busy-services",
			fmt.Sprintf("%s %s %s", inhibitor.Who, inhibitor.Why, inhibitor.What),
			string(inhibitor.Type))
	}

	tx.Add("PUBLISH", "power-manager:busy-services", "updated")

	if _, err := tx.Exec(); err != nil {
		s.logger.Printf("Failed to publish inhibitors: %v", err)
	}

	// Check if there are other blocking inhibitors besides modem
	hasOtherBlockingInhibitors := false
	for _, inh := range inhibitors {
		if inh.Type == inhibitor.TypeBlock && inh.Who != "unu-modem" && inh.Who != "modem-service" {
			hasOtherBlockingInhibitors = true
			break
		}
	}

	if hasOtherBlockingInhibitors {
		s.modemDisabled = false
	} else if s.lpmImminentTimerElapsed && s.hasOnlyModemBlockingInhibitors() && !s.modemDisabled {
		s.disableModem()
	}

	if s.lpmImminentTimerElapsed && !s.inhibitorManager.HasBlockingInhibitors() && !s.powerManager.IsLowPowerStateIssued() {
		s.issueLowPowerState()
	}
}

// handleLowPowerStateEnter processes low power state enter events
func (s *Service) handleLowPowerStateEnter(state string) {
	s.logger.Printf("Entering low power state: %s", state)
	s.publishState(state)

	// Schedule removal of inhibitor
	go func() {
		time.Sleep(s.config.InhibitorDuration)
		s.events <- Event{
			Type: EventDelayInhibitorRemove,
			Data: nil,
		}
	}()
}

// handleLowPowerStateExit processes low power state exit events
func (s *Service) handleLowPowerStateExit() {
	s.logger.Printf("Exiting low power state")
	s.publishState(string(power.StateRun))
}

// handleWakeup processes wakeup events
func (s *Service) handleWakeup(wakeupReason string) {
	s.logger.Printf("Wakeup detected with reason: %s", wakeupReason)
	s.publishWakeupSource(wakeupReason)

	s.modemDisabled = false

	if s.delayInhibitor == nil {
		s.delayInhibitor = s.inhibitorManager.AddInhibitor(
			"pm-service",
			"default delay",
			"delay",
			inhibitor.TypeDelay,
		)
	}

	if s.powerManager.GetTargetState() != power.StateRun && s.canEnterLowPowerState() {
		// Use shorter delay for RTC wakeup (IRQ 45)
		if wakeupReason == "45" {
			s.startSuspendImminentTimer()
		} else {
			s.startPreSuspendTimer()
		}
	}
}

// handleHibernationTimerExpired processes hibernation timer expiration events
func (s *Service) handleHibernationTimerExpired() {
	s.logger.Printf("Hibernation timer expired, executing timer-based hibernation")
	s.powerManager.SetTargetState(power.StateHibernateTimer)
	if s.canEnterLowPowerState() {
		s.startSuspendImminentTimer()
	}
}

// handleHibernationComplete processes hibernation sequence completion events
func (s *Service) handleHibernationComplete() {
	s.powerManager.SetTargetState(power.StateHibernateManual)
	if s.canEnterLowPowerState() {
		s.startSuspendImminentTimer()
	}
}

// handleHibernationSettingsChanged processes hibernation settings change events
func (s *Service) handleHibernationSettingsChanged(timerValue int32) {
	s.hibernationTimer.SetTimerValue(timerValue)
	s.logger.Printf("Updated hibernation timer setting: %d seconds", timerValue)
}

// handleDelayInhibitorRemove processes delay inhibitor removal events
func (s *Service) handleDelayInhibitorRemove() {
	if s.delayInhibitor != nil {
		s.inhibitorManager.RemoveInhibitor(s.delayInhibitor)
		s.delayInhibitor = nil
	}
}

func (s *Service) canEnterLowPowerState() bool {
	targetState := s.powerManager.GetTargetState()

	// Cannot enter low power state if target is to stay running
	if targetState == power.StateRun {
		return false
	}

	// Special case for reboot - allow in both stand-by and shutting-down states
	if targetState == power.StateReboot {
		if s.vehicleState != "stand-by" && s.vehicleState != "shutting-down" {
			s.logger.Printf("Cannot enter reboot state: vehicle state is %s (needs stand-by or shutting-down)",
				s.vehicleState)
			return false
		}
		return true
	}

	// Regular check for other power states
	if s.vehicleState != "stand-by" && s.vehicleState != "parked" {
		s.logger.Printf("Cannot enter low power state: vehicle state is %s (needs stand-by or parked)", s.vehicleState)
		return false
	}

	if targetState == power.StateSuspend && s.batteryState == "active" {
		s.logger.Printf("Cannot enter suspend state: battery state is active")
		return false
	}

	return true
}

func (s *Service) startPreSuspendTimer() {
	if s.preSuspendTimer != nil {
		s.preSuspendTimer.Stop()
	}

	s.preSuspendTimer = time.AfterFunc(s.config.PreSuspendDelay, func() {
		s.events <- Event{
			Type: EventPreSuspendElapsed,
			Data: nil,
		}
	})
}

func (s *Service) stopPreSuspendTimer() {
	if s.preSuspendTimer != nil {
		s.preSuspendTimer.Stop()
		s.preSuspendTimer = nil
	}
}

// handlePreSuspendElapsed processes pre-suspend timer expiration
func (s *Service) handlePreSuspendElapsed() {
	s.logger.Printf("Pre-suspend timer elapsed")

	if s.canEnterLowPowerState() {
		s.startSuspendImminentTimer()
	}
}

func (s *Service) startSuspendImminentTimer() {
	if s.suspendImminentTimer != nil {
		s.suspendImminentTimer.Stop()
	}

	s.publishState(string(s.powerManager.GetTargetState()) + "-imminent")

	s.suspendImminentTimer = time.AfterFunc(s.config.SuspendImminentDelay, func() {
		s.events <- Event{
			Type: EventSuspendImminentElapsed,
			Data: nil,
		}
	})
}

// handleSuspendImminentElapsed processes suspend imminent timer expiration
func (s *Service) handleSuspendImminentElapsed() {
	s.logger.Printf("Suspend imminent timer elapsed")
	s.lpmImminentTimerElapsed = true

	if s.hasOnlyModemBlockingInhibitors() {
		s.disableModem()
	}

	if !s.inhibitorManager.HasBlockingInhibitors() && !s.powerManager.IsLowPowerStateIssued() {
		s.issueLowPowerState()
	}
}

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
	if s.modemDisabled {
		return
	}

	s.logger.Printf("Issuing modem to turn off")
	if _, err := s.redis.LPush("scooter:modem", "disable"); err != nil {
		s.logger.Printf("Failed to disable modem: %v", err)
		return
	}

	s.modemDisabled = true
}

func (s *Service) stopSuspendImminentTimer() {
	if s.suspendImminentTimer != nil {
		s.suspendImminentTimer.Stop()
		s.suspendImminentTimer = nil
		s.lpmImminentTimerElapsed = false
	}
}

// listenForHibernationSettings listens for hibernation timer setting changes
func (s *Service) listenForHibernationSettings(ctx context.Context, redis *redis.Client) {
	// Subscribe to settings changes
	pubsub := redis.Subscribe(ctx, "settings")
	defer pubsub.Close()

	// Load initial hibernation timer setting
	s.loadHibernationTimerSetting(redis)

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			if msg.Payload == "hibernation-timer" {
				s.loadHibernationTimerSetting(redis)
			}
		}
	}
}

// loadHibernationTimerSetting loads the hibernation timer setting from Redis
func (s *Service) loadHibernationTimerSetting(redis *redis.Client) {
	result := redis.HGet(context.Background(), "settings", "hibernation-timer")
	if result.Err() == nil {
		if timerValue, err := strconv.ParseInt(result.Val(), 10, 32); err == nil {
			s.events <- Event{
				Type: EventHibernationSettingsChanged,
				Data: HibernationSettingsData{TimerValue: int32(timerValue)},
			}
		} else {
			s.logger.Printf("Failed to parse hibernation timer setting: %v", err)
		}
	}
}

func (s *Service) issueLowPowerState() {
	if s.powerManager.IsLowPowerStateIssued() {
		s.logger.Printf("Low power state already issued, skipping")
		return
	}

	state := s.powerManager.GetTargetState()
	if state == power.StateRun {
		s.logger.Printf("Target state is run, not issuing low power state")
		return
	}

	s.logger.Printf("Issuing low power state: %s", state)

	if err := s.powerManager.IssueTargetState(state); err != nil {
		s.logger.Printf("Failed to issue low power state: %v", err)
	} else {
		s.logger.Printf("Successfully issued low power state command: %s", state)
	}
}

// mapPowerStateToRedis converts power state to Redis state format
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

func (s *Service) publishState(state string) {
	redisState := s.mapPowerStateToRedis(state)
	s.logger.Printf("Publishing power state: %s (Redis: %s)", state, redisState)

	tx := s.redis.NewTxGroup("power-state")

	tx.Add("HSET", "power-manager", "state", redisState)

	tx.Add("PUBLISH", "power-manager", "state")

	if _, err := tx.Exec(); err != nil {
		s.logger.Printf("Failed to publish power state: %v", err)
	}
}

// onGovernorCommand handles CPU governor change requests
func (s *Service) onGovernorCommand(data []byte) error {
	governor := string(data)

	s.events <- Event{
		Type: EventGovernorCommand,
		Data: GovernorCommandData{Governor: governor},
	}

	return nil
}

// setGovernor changes the CPU governor and publishes the change
func (s *Service) setGovernor(governor string) error {
	s.logger.Printf("Setting CPU governor to: %s", governor)

	// Use the direct sysfs interface to change the CPU governor
	governorPath := "/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor"

	// Execute the change using shell command for reliability
	cmd := exec.Command("sh", "-c", fmt.Sprintf("echo %s > %s", governor, governorPath))
	if err := cmd.Run(); err != nil {
		s.logger.Printf("Failed to set CPU governor to %s: %v", governor, err)
		return fmt.Errorf("failed to set CPU governor to %s: %w", governor, err)
	}

	s.logger.Printf("Successfully set CPU governor to %s", governor)

	// Publish the governor change to Redis
	if err := s.publishGovernorChange(governor); err != nil {
		s.logger.Printf("Warning: Failed to publish governor change to Redis: %v", err)
	}

	return nil
}

// publishGovernorChange publishes governor changes to Redis
func (s *Service) publishGovernorChange(governor string) error {
	tx := s.redis.NewTxGroup("governor")

	tx.Add("HSET", "system", "cpu:governor", governor)
	tx.Add("PUBLISH", "system", "cpu:governor")

	if _, err := tx.Exec(); err != nil {
		return fmt.Errorf("failed to publish governor change: %w", err)
	}

	return nil
}

// enableWakeupSources enables wakeup on configured serial ports
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

// publishWakeupSource publishes the wakeup source to Redis
func (s *Service) publishWakeupSource(wakeupReason string) {
	s.logger.Printf("Publishing wakeup source: %s", wakeupReason)

	tx := s.redis.NewTxGroup("wakeup-source")

	tx.Add("HSET", "power-manager", "wakeup-source", wakeupReason)
	tx.Add("PUBLISH", "power-manager", "wakeup-source")

	if _, err := tx.Exec(); err != nil {
		s.logger.Printf("Failed to publish wakeup source: %v", err)
	}
}
