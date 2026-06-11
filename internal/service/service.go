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

// Last-ditch hibernate thresholds. The trigger fires when both main battery
// slots are unusable (present=false, or present but charge==0) AND the system
// has lost enough reserve to risk a brown-out — either the CBB is below
// threshold or the aux 12V rail (which feeds the iMX6) has dipped. Aux uses a Schmitt-trigger
// recovery margin to avoid event thrash near the threshold, mirroring
// battery-service's aux-low keep-active logic.
const (
	lastDitchHibernateCBBThreshold = 50
	lastDitchHibernateAuxEnterMv   = 11500
	lastDitchHibernateAuxExitMv    = 11700

	// lastDitchHibernateBootGrace suppresses the last-ditch trigger for the
	// first minutes after pm-service starts. After waking from a last-ditch
	// hibernation, Redis still holds the pre-hibernate battery state (slots
	// absent, CBB low) until battery-service re-detects the pack, so firing
	// on the seeded values would power the scooter straight back off before
	// a freshly inserted battery is recognized. A timer re-evaluates the
	// condition once the grace expires, so a genuinely battery-less scooter
	// still hibernates — just not within the first 5 minutes of boot.
	lastDitchHibernateBootGrace = 5 * time.Minute

	// suspendQuiesceTimeout bounds the wait for bluetooth-service's confirm
	// that the nRF52 was told we are suspending; on expiry the suspend is
	// aborted (staying awake beats the suspend/wake loop).
	suspendQuiesceTimeout = 3 * time.Second
	// suspendQuiesceMargin lets the nRF's reply to the suspending frame drain
	// onto ttymxc1 while we are still awake to absorb it.
	suspendQuiesceMargin = 200 * time.Millisecond
	// suspendEnterTimeout bounds the wait for logind's PrepareForSleep(true)
	// after requesting a suspend. logind emits it before honoring delay
	// inhibitors, so this only expires if the suspend job failed outright.
	suspendEnterTimeout = 15 * time.Second
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

	// Last-ditch hibernate inputs. Updated from Redis hash watchers under
	// lastDitchMu; read by lastDitchTriggeredLocked (callers hold the mutex).
	// The trigger itself is level-triggered and lives in the FSM: watchers
	// send EvLastDitchCheck and the IsLastDitchTriggered guard decides at
	// transition time, so no edge latch is needed.
	// cbBatteryCharge==-1 / auxVoltageMv==-1 mean "unknown" (no reading yet)
	// and suppress their respective sub-conditions.
	lastDitchMu     sync.Mutex
	cbBatteryCharge int  // 0..100, -1 if unknown
	battery0Present bool // defaults to true so we don't trigger before first sync
	battery1Present bool
	battery0Charge  int // 0..100, -1 if unknown
	battery1Charge  int
	auxVoltageMv    int  // millivolts, -1 if unknown
	auxLowLatched   bool // Schmitt latch: true once below enter, until above exit
	// lastDitchVehicleStandby mirrors vehicle.state == "stand-by" for the
	// sendLastDitchCheck pre-filter only; CanEnterLowPowerState remains the
	// authoritative vehicle-state guard inside the FSM.
	lastDitchVehicleStandby bool

	// lastDitchGraceUntil is set once in Run() before any watcher starts;
	// the trigger condition reports false before this instant.
	lastDitchGraceUntil time.Time
	lastDitchGraceLog   bool // one-shot: suppression has been logged this boot

	// Settings overrides for the wake timer; protected by settingsMu.
	settingsMu              sync.Mutex
	wakeTimerMaxSecondsOver uint32
	wakeTimerAckTimeoutOver time.Duration

	// Setting A ("keep online scooters reachable"): suspendWhenOnline
	// (pm.suspend-when-online) gates whether a locked scooter with a main
	// battery inserted may suspend while online; online mirrors
	// internet.status == "connected". Both guarded by settingsMu.
	suspendWhenOnline bool
	online            bool

	// wakeTimerAcks receives the nRF52's wake-timer-set acknowledgement: true
	// when the timer is armed, false when it was disarmed. Buffered with size 1
	// so the watcher goroutine never blocks. Consumers drain stale values
	// before each wait.
	wakeTimerAcks chan bool

	// suspendQuiesceAcks receives bluetooth-service's confirmation (the
	// power-state-sent field) that it forwarded "suspending" to the nRF52.
	// Same buffered-size-1 discipline as wakeTimerAcks.
	suspendQuiesceAcks chan struct{}

	// scheduler drives cron-based "hibernate every N at HH:MM for D" plans.
	scheduler *hibernation.Scheduler

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
		config:             cfg,
		logger:             logger,
		redis:              redisClient,
		systemdClient:      systemdClient,
		powerManagerPub:    redisClient.NewHashPublisher("power-manager"),
		systemPub:          redisClient.NewHashPublisher("system"),
		busyServicesPub:    redisClient.NewHashPublisher("power-manager:busy-services"),
		wakeTimerAcks:      make(chan bool, 1),
		suspendQuiesceAcks: make(chan struct{}, 1),
		cbBatteryCharge:    -1,
		battery0Present:    true,
		battery1Present:    true,
		battery0Charge:     -1,
		battery1Charge:     -1,
		auxVoltageMv:       -1,
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
		s.lastDitchVehicleStandby = (vehicleState == "stand-by")
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

	// Seed last-ditch hibernate inputs from current Redis state. The boot
	// grace deadline must be set before any watcher can call the evaluator.
	s.lastDitchGraceUntil = time.Now().Add(lastDitchHibernateBootGrace)
	if v, err := s.redis.HGet("battery:0", "present"); err == nil && v != "" {
		s.battery0Present = parseBool(v)
	}
	if v, err := s.redis.HGet("battery:1", "present"); err == nil && v != "" {
		s.battery1Present = parseBool(v)
	}
	// Seed online state before the first low-power evaluation so the
	// suspend-when-online guard takes effect immediately (the internet watcher
	// only syncs later).
	if v, err := s.redis.HGet("internet", "status"); err == nil && v != "" {
		s.online = (v == "connected")
	}
	if v, err := s.redis.HGet("battery:0", "charge"); err == nil && v != "" {
		s.battery0Charge = parseChargeOrUnknown(v)
	}
	if v, err := s.redis.HGet("battery:1", "charge"); err == nil && v != "" {
		s.battery1Charge = parseChargeOrUnknown(v)
	}
	if v, err := s.redis.HGet("cb-battery", "charge"); err == nil && v != "" {
		s.cbBatteryCharge = parseChargeOrUnknown(v)
	}
	if v, err := s.redis.HGet("aux-battery", "voltage"); err == nil && v != "" {
		s.auxVoltageMv = parseNonNegativeInt(v)
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

	// Cron-based scheduled hibernation (e.g. "every evening at 22:00 for 8h").
	// Fires by sending EvPowerHibernateFor; behaviour matches the ad-hoc
	// hibernate-for command from there onwards.
	s.scheduler = hibernation.NewScheduler(
		s.logger,
		s.wakeTimerMaxSeconds,
		func(wakeSeconds uint32) {
			if s.machine == nil || wakeSeconds == 0 {
				return
			}
			s.machine.Send(librefsm.Event{
				ID: fsm.EvPowerHibernateFor,
				Payload: fsm.PowerCommandPayload{
					TargetState: fsm.TargetHibernateFor,
					WakeSeconds: wakeSeconds,
				},
			})
		},
	)
	s.scheduler.Start(ctx)

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
		OnField("present", func(v string) error { return s.onBatteryPresentChanged("0", v) }).
		OnField("charge", func(v string) error { return s.onBatteryChargeChanged("0", v) }).
		StartWithSync(); err != nil {
		return fmt.Errorf("failed to start battery:0 state watcher: %v", err)
	}

	if err := s.redis.NewHashWatcher("battery:1").
		OnField("state", func(state string) error { return s.onBatteryStateChanged("1", state) }).
		OnField("present", func(v string) error { return s.onBatteryPresentChanged("1", v) }).
		OnField("charge", func(v string) error { return s.onBatteryChargeChanged("1", v) }).
		StartWithSync(); err != nil {
		return fmt.Errorf("failed to start battery:1 state watcher: %v", err)
	}

	if err := s.redis.NewHashWatcher("cb-battery").
		OnField("charge", s.onCBBatteryChargeChanged).
		StartWithSync(); err != nil {
		return fmt.Errorf("failed to start cb-battery watcher: %v", err)
	}

	if err := s.redis.NewHashWatcher("aux-battery").
		OnField("voltage", s.onAuxVoltageChanged).
		StartWithSync(); err != nil {
		return fmt.Errorf("failed to start aux-battery watcher: %v", err)
	}

	if err := s.redis.NewHashWatcher("internet").
		OnField("status", s.onInternetStatusChanged).
		StartWithSync(); err != nil {
		return fmt.Errorf("failed to start internet watcher: %v", err)
	}

	redis_ipc.HandleRequests(s.redis, "scooter:power", s.onPowerCommand)
	redis_ipc.HandleRequests(s.redis, "scooter:governor", s.onGovernorCommand)

	// Start Redis inhibitor listener (syncs power:inhibits hash into inhibitor manager)
	go s.inhibitorManager.StartRedisListener(ctx, s.redis, s.logger)

	// Watch settings in Redis
	if err := s.redis.NewHashWatcher("settings").
		OnField("pm.hibernation-timer", s.onHibernationTimerSetting).
		OnField("pm.default-state", s.onDefaultStateSetting).
		OnField("pm.suspend-when-online", s.onSuspendWhenOnlineSetting).
		OnField("pm.wake-timer-max-seconds", s.onWakeTimerMaxSecondsSetting).
		OnField("pm.wake-timer-ack-timeout", s.onWakeTimerAckTimeoutSetting).
		OnField("pm.scheduled-hibernate-enabled", s.onScheduledHibernateEnabledSetting).
		OnField("pm.scheduled-hibernate-cron", s.onScheduledHibernateCronSetting).
		OnField("pm.scheduled-hibernate-duration", s.onScheduledHibernateDurationSetting).
		StartWithSync(); err != nil {
		return fmt.Errorf("failed to start settings watcher: %v", err)
	}

	// Poll gps.active as the proxy for "the wall clock has been bootstrapped
	// from GPS". modem-service calls chronyc settime in the same loop
	// iteration that flips gps.active to true on a valid fix, so once it's
	// true, chrony has the time.
	//
	// We poll instead of pub/sub-watching: modem-service writes the gps hash
	// either via SetMany(NoPublish) (offline scooters, always) or
	// SetManyPublishOne(data, "timestamp") (online recovery only) — neither
	// path fires an OnField("active") subscription. The poller does an
	// immediate first check, then every 30 s; once it sees "true" the
	// scheduler is latched and the poller exits. NTP-only scooters without a
	// GPS receiver would never flip this; documented v1 limitation.
	go s.pollGPSActiveForTimeSync(ctx)

	// Re-check the last-ditch condition once the boot grace expires, so a
	// trigger suppressed during the grace still fires even if no further
	// battery/CBB/aux updates arrive.
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(time.Until(s.lastDitchGraceUntil) + time.Second):
			s.sendLastDitchCheck()
		}
	}()

	// Watch power-manager hash for the nRF52 wake-timer ACK so EnterIssuingLowPower
	// can confirm the wake source is armed before issuing systemctl poweroff.
	if err := s.redis.NewHashWatcher("power-manager").
		OnField("wake-timer-armed", s.onWakeTimerArmed).
		OnField("power-state-sent", s.onPowerStateSent).
		Start(); err != nil {
		return fmt.Errorf("failed to start power-manager watcher: %v", err)
	}

	// Wait for context cancellation
	<-ctx.Done()

	// Cleanup
	s.machine.Stop()
	s.hibernationTimer.Close()
	if s.scheduler != nil {
		s.scheduler.Close()
	}

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
	s.lastDitchMu.Lock()
	s.lastDitchVehicleStandby = (vehicleState == "stand-by")
	s.lastDitchMu.Unlock()
	if s.machine != nil {
		s.machine.Send(librefsm.Event{
			ID:      fsm.EvVehicleStateChanged,
			Payload: fsm.VehicleStatePayload{State: vehicleState},
		})
	}
	if s.scheduler != nil {
		s.scheduler.OnVehicleStateChanged(vehicleState)
	}
	// Vehicle state is a guard input for last-ditch hibernate
	// (CanEnterLowPowerState requires stand-by); re-check after the vehicle
	// event so the trigger fires when the vehicle settles into stand-by
	// while CBB is already low. The check event queues behind the vehicle
	// event, so the guard sees the updated fsmData.VehicleState.
	s.sendLastDitchCheck()
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

func (s *Service) onBatteryPresentChanged(slot, v string) error {
	present := parseBool(v)
	s.lastDitchMu.Lock()
	switch slot {
	case "0":
		s.battery0Present = present
	case "1":
		s.battery1Present = present
	}
	s.lastDitchMu.Unlock()
	s.sendLastDitchCheck()
	return nil
}

func (s *Service) onBatteryChargeChanged(slot, v string) error {
	charge := parseChargeOrUnknown(v)
	s.lastDitchMu.Lock()
	switch slot {
	case "0":
		s.battery0Charge = charge
	case "1":
		s.battery1Charge = charge
	}
	s.lastDitchMu.Unlock()
	s.sendLastDitchCheck()
	return nil
}

func (s *Service) onCBBatteryChargeChanged(v string) error {
	charge := parseChargeOrUnknown(v)
	s.lastDitchMu.Lock()
	s.cbBatteryCharge = charge
	s.lastDitchMu.Unlock()
	s.sendLastDitchCheck()
	return nil
}

func (s *Service) onAuxVoltageChanged(v string) error {
	mv := parseNonNegativeInt(v)
	s.lastDitchMu.Lock()
	s.auxVoltageMv = mv
	// Aux Schmitt trigger: latch below the enter threshold, release above
	// the exit threshold. Updated here (not in the guard) so the guard
	// stays read-only no matter how often the FSM evaluates it.
	if mv >= 0 {
		if s.auxLowLatched {
			if mv >= lastDitchHibernateAuxExitMv {
				s.auxLowLatched = false
			}
		} else if mv < lastDitchHibernateAuxEnterMv {
			s.auxLowLatched = true
		}
	}
	s.lastDitchMu.Unlock()
	s.sendLastDitchCheck()
	return nil
}

// lastDitchTriggeredLocked reports whether the last-ditch hibernate condition
// currently holds:
//
//	(both main slots missing — present=false OR charge==0)
//	AND
//	(CBB charge < lastDitchHibernateCBBThreshold OR aux below the Schmitt
//	 threshold).
//
// Within lastDitchHibernateBootGrace of startup it reports false (see the
// constant's comment for the wake-race rationale); the grace suppression is
// logged once per boot. Callers must hold lastDitchMu.
func (s *Service) lastDitchTriggeredLocked() bool {
	cbb := s.cbBatteryCharge
	slot0Missing := !s.battery0Present || s.battery0Charge == 0
	slot1Missing := !s.battery1Present || s.battery1Charge == 0
	bothMissing := slot0Missing && slot1Missing
	cbbLow := cbb >= 0 && cbb < lastDitchHibernateCBBThreshold

	if !(bothMissing && (cbbLow || s.auxLowLatched)) {
		return false
	}
	if remaining := time.Until(s.lastDitchGraceUntil); remaining > 0 {
		if !s.lastDitchGraceLog {
			s.lastDitchGraceLog = true
			s.logger.Printf("Last-ditch hibernate condition met within boot grace; deferring up to %v", remaining.Round(time.Second))
		}
		return false
	}
	return true
}

// sendLastDitchCheck enqueues EvLastDitchCheck when the trigger condition
// currently holds and the vehicle is in stand-by. The FSM guards re-evaluate
// authoritatively at transition time; this pre-check only keeps no-op events
// out of the queue (and CanEnterLowPowerState's rejection log quiet while
// e.g. a battery swap is in progress with the seatbox open). The payload
// carries the plain hibernate target so the priority guard and
// OnPowerCommand see a regular power command.
func (s *Service) sendLastDitchCheck() {
	if s.machine == nil {
		return
	}
	s.lastDitchMu.Lock()
	triggered := s.lastDitchTriggeredLocked() && s.lastDitchVehicleStandby
	s.lastDitchMu.Unlock()
	if !triggered {
		return
	}
	s.machine.Send(librefsm.Event{
		ID:      fsm.EvLastDitchCheck,
		Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetHibernate},
	})
}

// IsLastDitchTriggered is the FSM guard for EvLastDitchCheck transitions and
// the last-ditch wake routing.
func (s *Service) IsLastDitchTriggered(c *librefsm.Context) bool {
	s.lastDitchMu.Lock()
	defer s.lastDitchMu.Unlock()
	return s.lastDitchTriggeredLocked()
}

// logLastDitchTrigger reports which reserve ran low when the trigger fires.
func (s *Service) logLastDitchTrigger() {
	s.lastDitchMu.Lock()
	cbb := s.cbBatteryCharge
	cbbLow := cbb >= 0 && cbb < lastDitchHibernateCBBThreshold
	auxLow := s.auxLowLatched
	auxMv := s.auxVoltageMv
	s.lastDitchMu.Unlock()

	var reserve string
	switch {
	case cbbLow && auxLow:
		reserve = fmt.Sprintf("CBB=%d%%, aux=%d mV", cbb, auxMv)
	case cbbLow:
		reserve = fmt.Sprintf("CBB=%d%%", cbb)
	default:
		reserve = fmt.Sprintf("aux=%d mV", auxMv)
	}
	s.logger.Printf("Last-ditch hibernate: no main battery, %s — hibernating", reserve)
}

// OnLastDitchTriggered logs the trigger and records the hibernate target from
// the event payload (delegates to OnPowerCommand).
func (s *Service) OnLastDitchTriggered(c *librefsm.Context) error {
	s.logLastDitchTrigger()
	return s.OnPowerCommand(c)
}

// OnLastDitchWakeup handles a wake from suspend that routes straight to
// hibernate because the trigger condition holds (e.g. the CBB drained during
// suspend). Performs the regular wakeup bookkeeping, then sets the target.
func (s *Service) OnLastDitchWakeup(c *librefsm.Context) error {
	if err := s.OnWakeup(c); err != nil {
		return err
	}
	s.logLastDitchTrigger()
	s.fsmData.TargetPowerState = fsm.TargetHibernate
	s.fsmData.HibernateForWakeSeconds = 0
	return nil
}

// parseBool accepts the "true"/"false" form battery-service publishes.
func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// parseChargeOrUnknown returns -1 ("unknown") for unparseable input. Values
// outside 0..100 are clamped to the valid range.
func parseChargeOrUnknown(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return -1
	}
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

// parseNonNegativeInt returns -1 ("unknown") for unparseable or negative input.
func parseNonNegativeInt(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return -1
	}
	return n
}

func (s *Service) onPowerCommand(command string) error {
	s.logger.Printf("Received power command: %s", command)

	// hibernate-for:<seconds> arms a wake timer on the nRF52 and then enters
	// hibernation; the iMX6 is brought back up by the nRF52 after the duration.
	if strings.HasPrefix(command, "hibernate-for:") {
		raw := strings.TrimPrefix(command, "hibernate-for:")
		secs, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || secs == 0 {
			s.logger.Printf("Invalid hibernate-for duration: %q", raw)
			return nil
		}
		if max := uint64(s.wakeTimerMaxSeconds()); secs > max {
			s.logger.Printf("hibernate-for %d exceeds max %d; clamping", secs, max)
			secs = max
		}
		if s.machine != nil {
			s.machine.Send(librefsm.Event{
				ID: fsm.EvPowerHibernateFor,
				Payload: fsm.PowerCommandPayload{
					TargetState: fsm.TargetHibernateFor,
					WakeSeconds: uint32(secs),
				},
			})
		}
		return nil
	}

	// hibernate-cancel returns to run AND disarms any wake timer programmed on
	// the nRF52, so a previously-issued hibernate-for doesn't fire later.
	if command == "hibernate-cancel" {
		if err := s.powerManagerPub.Set("wake-timer-seconds", "0"); err != nil {
			s.logger.Printf("Failed to disarm wake timer on cancel: %v", err)
		}
		if s.machine != nil {
			s.machine.Send(librefsm.Event{
				ID:      fsm.EvPowerRun,
				Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetRun},
			})
		}
		return nil
	}

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

func (s *Service) EnterLowPowerImminent(c *librefsm.Context) error {
	s.logger.Printf("Entering low-power-imminent state (target: %s)", s.fsmData.TargetPowerState)
	// Kick the wake-timer ARM as early as possible so the ACK has time to
	// arrive before we hit EnterIssuingLowPower. Drain stale ACKs first so the
	// wait there sees only this round's response.
	if s.fsmData.TargetPowerState == fsm.TargetHibernateFor && s.fsmData.HibernateForWakeSeconds > 0 {
		select {
		case <-s.wakeTimerAcks:
		default:
		}
		val := strconv.FormatUint(uint64(s.fsmData.HibernateForWakeSeconds), 10)
		if err := s.powerManagerPub.Set("wake-timer-seconds", val); err != nil {
			s.logger.Printf("Failed to publish wake-timer-seconds: %v", err)
		} else {
			s.logger.Printf("Requested nRF wake timer: %s seconds", val)
		}
	}
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

	// hibernate-for must not poweroff without a confirmed wake source on the
	// nRF52. We wait for the wake-timer-armed ACK that bluetooth-service writes
	// when the nRF52 echoes our SET. On timeout, bail out to running.
	if s.fsmData.TargetPowerState == fsm.TargetHibernateFor && s.fsmData.HibernateForWakeSeconds > 0 {
		timeout := s.wakeTimerAckTimeout()
		s.logger.Printf("Waiting up to %v for nRF wake-timer ACK", timeout)
		select {
		case armed := <-s.wakeTimerAcks:
			if !armed {
				s.logger.Printf("nRF reported wake timer disarmed; aborting hibernate-for")
				c.Send(librefsm.Event{
					ID:      fsm.EvPowerRun,
					Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetRun},
				})
				return nil
			}
			s.logger.Printf("nRF wake timer armed; proceeding to poweroff")
		case <-time.After(timeout):
			s.logger.Printf("Timed out waiting for nRF wake-timer ACK; aborting hibernate-for")
			if err := s.powerManagerPub.Set("wake-timer-seconds", "0"); err != nil {
				s.logger.Printf("Failed to clear wake-timer-seconds on timeout: %v", err)
			}
			c.Send(librefsm.Event{
				ID:      fsm.EvPowerRun,
				Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetRun},
			})
			return nil
		}
	}

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
	case "hibernate", "hibernate-manual", "hibernate-timer", "hibernate-for":
		target = "poweroff"
	case "reboot":
		target = "reboot"
	default:
		return fmt.Errorf("unsupported power state: %s", s.fsmData.TargetPowerState)
	}

	s.fsmData.LowPowerStateIssued = true

	// Before suspending, announce the suspending state and wait until
	// bluetooth-service confirms it forwarded it to the nRF52. Receiving
	// "suspending" makes the nRF stop all USOCK TX; without that, its routine
	// traffic reaches our armed ttymxc1 wakeup and pulls the iMX6 straight back
	// out of suspend-to-RAM. A fixed delay is not enough: the Redis hop is
	// jittery, and the nRF's reply to the suspending frame itself must land
	// while we are still awake. After the confirm, a short margin lets that
	// reply drain; on timeout, bail back to running rather than suspend into
	// the wake loop.
	if target == "suspend" {
		select {
		case <-s.suspendQuiesceAcks:
		default:
		}
		if err := s.powerManagerPub.Set("state", s.mapPowerStateToRedis(s.fsmData.TargetPowerState)); err != nil {
			s.logger.Printf("Failed to announce suspending state before suspend: %v", err)
		}
		s.logger.Printf("Waiting up to %v for the suspending state to reach the nRF", suspendQuiesceTimeout)
		select {
		case <-s.suspendQuiesceAcks:
			s.logger.Printf("nRF told we are suspending; settling %v before suspend", suspendQuiesceMargin)
			time.Sleep(suspendQuiesceMargin)
		case <-time.After(suspendQuiesceTimeout):
			s.logger.Printf("No confirmation that the nRF was told; aborting suspend to avoid the wake loop")
			c.Send(librefsm.Event{
				ID:      fsm.EvPowerRun,
				Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetRun},
			})
			return nil
		}
	}

	s.logger.Printf("Issuing %s command", target)

	if target == "suspend" {
		// Block on logind's PrepareForSleep cycle so the return only means
		// "the system slept and resumed". `systemctl suspend` alone returns
		// when the job is enqueued, which used to make the FSM read a stale
		// wakeup reason and cycle through pre-suspend while the kernel was
		// still on its way down.
		if err := s.systemdClient.SuspendAndWaitResume(s.ctx, suspendEnterTimeout); err != nil {
			s.fsmData.LowPowerStateIssued = false
			if s.ctx.Err() != nil {
				return nil
			}
			s.logger.Printf("Suspend did not complete: %v; returning to running", err)
			c.Send(librefsm.Event{
				ID:      fsm.EvPowerRun,
				Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetRun},
			})
			return nil
		}
		s.handleWakeupAfterSuspend(c)
		return nil
	}

	if err := s.systemdClient.IssueCommand(target); err != nil {
		s.fsmData.LowPowerStateIssued = false
		return fmt.Errorf("failed to issue %s command: %v", target, err)
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

	if targetState == fsm.TargetSuspend {
		// Setting A: keep online, in-service scooters reachable. If
		// pm.suspend-when-online is off and a main battery is inserted while we
		// have internet connectivity, stay awake so cloud commands can still
		// reach us. Checked before the battery-active guard so the operator sees
		// the connectivity reason.
		s.settingsMu.Lock()
		suspendWhenOnline := s.suspendWhenOnline
		online := s.online
		s.settingsMu.Unlock()
		s.lastDitchMu.Lock()
		batteryPresent := s.battery0Present || s.battery1Present
		s.lastDitchMu.Unlock()
		if !suspendWhenOnline && online && batteryPresent {
			s.logger.Printf("I would like to suspend now but the setting preventeth me! Oh woe!")
			return false
		}

		// Don't suspend while a battery is actively delivering power.
		if s.batteryStateFromContext(c) == "active" {
			s.logger.Printf("Cannot enter suspend state: battery is active")
			return false
		}
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
	case fsm.TargetHibernate, fsm.TargetHibernateManual, fsm.TargetHibernateTimer, fsm.TargetHibernateFor, fsm.TargetReboot:
		return true
	}
	return false
}

// IsPowerCommandHigherPriority checks if the command in the event payload can override
// the current target power state. Guards against priority downgrades.
// Priority order: run > hibernate-manual/hibernate-for > hibernate > hibernate-timer > suspend/reboot
// hibernate-for is treated at the same tier as hibernate-manual because both
// are explicit user-initiated commands.
func (s *Service) IsPowerCommandHigherPriority(c *librefsm.Context) bool {
	newState := s.targetPowerStateFromContext(c)
	current := s.fsmData.TargetPowerState

	if newState == "run" {
		return true
	}

	switch current {
	case "hibernate-manual", "hibernate-for":
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
		// hibernate-for carries a wake duration; every other command clears it
		// so a cancelled hibernate-for doesn't leak its timer into the next
		// power request.
		if p.TargetState == fsm.TargetHibernateFor {
			s.fsmData.HibernateForWakeSeconds = p.WakeSeconds
		} else {
			s.fsmData.HibernateForWakeSeconds = 0
		}
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
	case fsm.StateSuspendImminent, fsm.StateLowPowerImminent:
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
	case "hibernate-for":
		return "hibernating-for"
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
	case "hibernate-for-imminent":
		return "hibernating-for-imminent"
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

// onWakeTimerMaxSecondsSetting absorbs pm.wake-timer-max-seconds. Out-of-range
// or unparseable values are ignored; the previous value (or compile-time
// default) keeps applying.
func (s *Service) onWakeTimerMaxSecondsSetting(value string) error {
	v, err := strconv.ParseUint(value, 10, 32)
	if err != nil || v < 60 {
		s.logger.Printf("Ignoring invalid pm.wake-timer-max-seconds=%q", value)
		return nil
	}
	s.settingsMu.Lock()
	s.wakeTimerMaxSecondsOver = uint32(v)
	s.settingsMu.Unlock()
	s.logger.Printf("pm.wake-timer-max-seconds set to %d", v)
	return nil
}

// onWakeTimerAckTimeoutSetting absorbs pm.wake-timer-ack-timeout.
func (s *Service) onWakeTimerAckTimeoutSetting(value string) error {
	d, err := time.ParseDuration(value)
	if err != nil || d < time.Second {
		s.logger.Printf("Ignoring invalid pm.wake-timer-ack-timeout=%q", value)
		return nil
	}
	s.settingsMu.Lock()
	s.wakeTimerAckTimeoutOver = d
	s.settingsMu.Unlock()
	s.logger.Printf("pm.wake-timer-ack-timeout set to %v", d)
	return nil
}

// wakeTimerMaxSeconds returns the active cap on a single hibernate-for request,
// honouring the runtime setting override before falling back to the compile-time
// default.
func (s *Service) wakeTimerMaxSeconds() uint32 {
	s.settingsMu.Lock()
	v := s.wakeTimerMaxSecondsOver
	s.settingsMu.Unlock()
	if v == 0 {
		return s.config.WakeTimerMaxSeconds
	}
	return v
}

// wakeTimerAckTimeout returns the active ACK timeout, honouring the runtime
// setting override before falling back to the compile-time default.
func (s *Service) wakeTimerAckTimeout() time.Duration {
	s.settingsMu.Lock()
	v := s.wakeTimerAckTimeoutOver
	s.settingsMu.Unlock()
	if v == 0 {
		return s.config.WakeTimerAckTimeout
	}
	return v
}

// onScheduledHibernateEnabledSetting flips the scheduler on or off.
func (s *Service) onScheduledHibernateEnabledSetting(value string) error {
	if s.scheduler == nil {
		return nil
	}
	s.scheduler.SetEnabled(value == "true")
	return nil
}

// onSuspendWhenOnlineSetting absorbs pm.suspend-when-online. When false (the
// default), a locked scooter with a main battery inserted will not suspend
// while it has internet connectivity, so it stays reachable.
func (s *Service) onSuspendWhenOnlineSetting(value string) error {
	s.settingsMu.Lock()
	s.suspendWhenOnline = (value == "true")
	s.settingsMu.Unlock()
	return nil
}

// onInternetStatusChanged tracks whether we currently have internet
// connectivity (internet.status == "connected"), consumed by the
// suspend-when-online guard.
func (s *Service) onInternetStatusChanged(value string) error {
	s.settingsMu.Lock()
	s.online = (value == "connected")
	s.settingsMu.Unlock()
	return nil
}

// onScheduledHibernateCronSetting passes a new cron expression to the scheduler.
// An empty string disables the schedule; invalid expressions are rejected by
// the scheduler itself.
func (s *Service) onScheduledHibernateCronSetting(value string) error {
	if s.scheduler == nil {
		return nil
	}
	s.scheduler.SetCron(value)
	return nil
}

// onScheduledHibernateDurationSetting updates the wake-by duration applied at
// each fire. Values that fail to parse are ignored.
func (s *Service) onScheduledHibernateDurationSetting(value string) error {
	if s.scheduler == nil {
		return nil
	}
	if value == "" {
		s.scheduler.SetDuration(0)
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		s.logger.Printf("Ignoring invalid pm.scheduled-hibernate-duration=%q: %v", value, err)
		return nil
	}
	s.scheduler.SetDuration(d)
	return nil
}

// pollGPSActiveForTimeSync polls Redis until gps.active becomes "true", then
// latches the scheduler's time-synced gate and exits. See the wiring comment
// in Run() for why a poller is used instead of a hash watcher.
func (s *Service) pollGPSActiveForTimeSync(ctx context.Context) {
	check := func() bool {
		val, err := s.redis.HGet("gps", "active")
		if err != nil {
			return false
		}
		if val == "true" && s.scheduler != nil {
			s.scheduler.SetTimeSynced(true)
			return true
		}
		return false
	}
	if check() {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if check() {
				return
			}
		}
	}
}

// onPowerStateSent is invoked when bluetooth-service confirms it forwarded a
// power state to the nRF52. Only "suspending" matters: it gates the actual
// suspend in EnterIssuingLowPower. Non-blocking; size-1 buffer, latest wins.
func (s *Service) onPowerStateSent(value string) error {
	if value != "suspending" {
		return nil
	}
	select {
	case s.suspendQuiesceAcks <- struct{}{}:
	default:
	}
	return nil
}

// onWakeTimerArmed is invoked by the power-manager hash watcher whenever
// bluetooth-service writes the wake-timer-armed field in response to an ACK
// from the nRF52. The value is a Go bool string: "true" when the timer is
// armed, "false" when it was disarmed. Non-blocking; only the latest signal
// is kept in the buffered channel.
func (s *Service) onWakeTimerArmed(value string) error {
	armed := value == "true"
	select {
	case s.wakeTimerAcks <- armed:
	default:
		// Drop oldest, push newest so the waiter always sees the latest signal.
		select {
		case <-s.wakeTimerAcks:
		default:
		}
		select {
		case s.wakeTimerAcks <- armed:
		default:
		}
	}
	return nil
}
