package service

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/librescoot/pm-service/internal/config"
	"github.com/librescoot/pm-service/internal/inhibitor"
	"github.com/librescoot/pm-service/internal/power"
	redis_ipc "github.com/rescoot/redis-ipc"
)

type Service struct {
	config           *config.Config
	logger           *log.Logger
	redis            *redis_ipc.Client
	powerManager     *power.Manager
	inhibitorManager *inhibitor.Manager
	delayInhibitor   *inhibitor.Inhibitor

	mutex        sync.RWMutex
	vehicleState string
	batteryState string

	preSuspendTimer      *time.Timer
	suspendImminentTimer *time.Timer
	hibernationTimer     *time.Timer

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

	service := &Service{
		config:                  cfg,
		logger:                  logger,
		redis:                   redisClient,
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
	vehicleSubscriber := s.redis.Subscribe("vehicle")
	if err := vehicleSubscriber.Handle("state", s.onVehicleState); err != nil {
		return fmt.Errorf("failed to subscribe to vehicle state: %v", err)
	}

	batterySubscriber := s.redis.Subscribe("battery:0")
	if err := batterySubscriber.Handle("state", s.onBatteryState); err != nil {
		return fmt.Errorf("failed to subscribe to battery state: %v", err)
	}

	_ = s.redis.HandleRequests("scooter:power", s.onPowerCommand)

	vehicleState, err := s.redis.HGet("vehicle", "state")
	if err == nil {
		s.vehicleState = vehicleState
	}

	batteryState, err := s.redis.HGet("battery:0", "state")
	if err == nil {
		s.batteryState = batteryState
	}

	if s.canEnterLowPowerState() {
		s.startPreSuspendTimer()
	}

	<-ctx.Done()

	if s.preSuspendTimer != nil {
		s.preSuspendTimer.Stop()
	}
	if s.suspendImminentTimer != nil {
		s.suspendImminentTimer.Stop()
	}
	if s.hibernationTimer != nil {
		s.hibernationTimer.Stop()
	}

	if err := s.powerManager.Close(); err != nil {
		s.logger.Printf("Failed to close power manager: %v", err)
	}

	if err := s.inhibitorManager.Close(); err != nil {
		s.logger.Printf("Failed to close inhibitor manager: %v", err)
	}

	if err := s.redis.Close(); err != nil {
		s.logger.Printf("Failed to close Redis client: %v", err)
	}

	return nil
}

func (s *Service) onVehicleState(data []byte) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	vehicleState, err := s.redis.HGet("vehicle", "state")
	if err != nil {
		return fmt.Errorf("failed to get vehicle state: %v", err)
	}

	if s.vehicleState == "stand-by" && vehicleState != "stand-by" {
		s.logger.Printf("Vehicle state changed from stand-by to %s, aborting low power mode", vehicleState)
		s.powerManager.SetTargetState(power.StateRun)
		s.stopSuspendImminentTimer()
		s.stopPreSuspendTimer()
		s.modemDisabled = false
		s.publishState(string(power.StateRun))
	}

	s.vehicleState = vehicleState
	s.logger.Printf("Vehicle state: %s", vehicleState)

	if s.canEnterLowPowerState() {
		s.startPreSuspendTimer()
	}

	if vehicleState == "stand-by" {
		s.startHibernationTimer()
	} else {
		s.stopHibernationTimer()
	}

	return nil
}

func (s *Service) onBatteryState(data []byte) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	batteryState, err := s.redis.HGet("battery:0", "state")
	if err != nil {
		return fmt.Errorf("failed to get battery state: %v", err)
	}

	s.batteryState = batteryState
	s.logger.Printf("Battery state: %s", batteryState)

	if s.canEnterLowPowerState() {
		s.startPreSuspendTimer()
	} else {
		s.stopPreSuspendTimer()
		s.stopSuspendImminentTimer()
	}

	return nil
}

func (s *Service) onPowerCommand(data []byte) error {
	command := string(data)
	s.logger.Printf("Received power command: %s", command)

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
		return fmt.Errorf("unknown power command: %s", command)
	}

	if s.canEnterLowPowerState() && !s.powerManager.IsLowPowerStateIssued() && s.powerManager.GetTargetState() != power.StateRun {
		s.startSuspendImminentTimer()
	}

	return nil
}

func (s *Service) onInhibitorsChanged() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

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

func (s *Service) onLowPowerStateEnter(state power.PowerState) {
	s.logger.Printf("Entering low power state: %s", state)
	s.publishState(string(state))

	// Schedule removal of inhibitor without taking a lock in the callback
	go func() {
		time.Sleep(s.config.InhibitorDuration)
		s.removeDelayInhibitor()
	}()
}

func (s *Service) removeDelayInhibitor() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.delayInhibitor != nil {
		s.inhibitorManager.RemoveInhibitor(s.delayInhibitor)
		s.delayInhibitor = nil
	}
}

func (s *Service) onLowPowerStateExit() {
	s.logger.Printf("Exiting low power state")
	s.publishState(string(power.StateRun))

	// Handle state changes in a separate method that takes the lock
	go s.handleWakeup()
}

func (s *Service) handleWakeup() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

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
		s.startPreSuspendTimer()
	}
}

func (s *Service) canEnterLowPowerState() bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if s.vehicleState != "stand-by" {
		return false
	}

	if s.powerManager.GetTargetState() == power.StateSuspend && s.batteryState == "active" {
		return false
	}

	return true
}

func (s *Service) startPreSuspendTimer() {
	if s.preSuspendTimer != nil {
		s.preSuspendTimer.Stop()
	}

	s.preSuspendTimer = time.AfterFunc(s.config.PreSuspendDelay, func() {
		s.mutex.Lock()
		defer s.mutex.Unlock()

		s.logger.Printf("Pre-suspend timer elapsed")

		if s.canEnterLowPowerState() {
			s.startSuspendImminentTimer()
		}
	})
}

func (s *Service) stopPreSuspendTimer() {
	if s.preSuspendTimer != nil {
		s.preSuspendTimer.Stop()
		s.preSuspendTimer = nil
	}
}

func (s *Service) startSuspendImminentTimer() {
	if s.suspendImminentTimer != nil {
		s.suspendImminentTimer.Stop()
	}

	s.publishState(string(s.powerManager.GetTargetState()) + "-imminent")

	s.suspendImminentTimer = time.AfterFunc(s.config.SuspendImminentDelay, func() {
		s.mutex.Lock()
		defer s.mutex.Unlock()

		s.logger.Printf("Suspend imminent timer elapsed")
		s.lpmImminentTimerElapsed = true

		if s.hasOnlyModemBlockingInhibitors() {
			s.disableModem()
		}

		if !s.inhibitorManager.HasBlockingInhibitors() && !s.powerManager.IsLowPowerStateIssued() {
			s.issueLowPowerState()
		}
	})
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

func (s *Service) startHibernationTimer() {
	if s.hibernationTimer != nil {
		s.hibernationTimer.Stop()
	}

	s.hibernationTimer = time.AfterFunc(s.config.HibernationTimer, func() {
		s.mutex.Lock()
		defer s.mutex.Unlock()

		s.logger.Printf("Hibernation timer elapsed")
		s.powerManager.SetTargetState(power.StateHibernateTimer)

		if s.canEnterLowPowerState() {
			s.startSuspendImminentTimer()
		}
	})
}

func (s *Service) stopHibernationTimer() {
	if s.hibernationTimer != nil {
		s.hibernationTimer.Stop()
		s.hibernationTimer = nil
	}
}

func (s *Service) issueLowPowerState() {
	if s.powerManager.IsLowPowerStateIssued() {
		return
	}

	state := s.powerManager.GetTargetState()
	if state == power.StateRun {
		return
	}

	s.logger.Printf("Issuing low power state: %s", state)

	if err := s.powerManager.IssueTargetState(state); err != nil {
		s.logger.Printf("Failed to issue low power state: %v", err)
	}
}

func (s *Service) publishState(state string) {
	s.logger.Printf("Publishing power state: %s", state)

	tx := s.redis.NewTxGroup("power-state")

	tx.Add("HSET", "power-manager", "state", state)

	tx.Add("PUBLISH", "power-manager", "state")

	if _, err := tx.Exec(); err != nil {
		s.logger.Printf("Failed to publish power state: %v", err)
	}
}
