package config

import (
	"flag"
	"time"
)

type Config struct {
	RedisHost string
	RedisPort int

	PreSuspendDelay      time.Duration
	SuspendImminentDelay time.Duration
	InhibitorDuration    time.Duration
	HibernationTimer     time.Duration

	// WakeTimerMaxSeconds is the upper bound applied to any hibernate-for
	// duration before it is sent to the nRF52 wake timer.
	WakeTimerMaxSeconds uint32
	// WakeTimerAckTimeout is how long EnterIssuingLowPower waits for the nRF52
	// to confirm a wake timer was armed before aborting the hibernation.
	WakeTimerAckTimeout time.Duration

	SocketPath    string
	DryRun        bool
	DefaultState  string
	WakeupSources []string
}

func New() *Config {
	return &Config{
		RedisHost:            "localhost",
		RedisPort:            6379,
		PreSuspendDelay:      60 * time.Second,
		SuspendImminentDelay: 5 * time.Second,
		InhibitorDuration:    500 * time.Millisecond,
		HibernationTimer:     3 * 24 * time.Hour, // 3 days
		WakeTimerMaxSeconds:  7 * 24 * 60 * 60,   // 1 week
		WakeTimerAckTimeout:  10 * time.Second,
		SocketPath:           "/tmp/suspend_inhibitor",
		DryRun:               false,
		DefaultState:         "suspend",
		WakeupSources:        []string{"ttymxc0", "ttymxc1"},
	}
}

func (c *Config) RegisterFlags() {
	flag.StringVar(&c.RedisHost, "redis-host", c.RedisHost, "Redis host")
	flag.IntVar(&c.RedisPort, "redis-port", c.RedisPort, "Redis port")

	flag.DurationVar(&c.PreSuspendDelay, "pre-suspend-delay", c.PreSuspendDelay,
		"Delay between standby and low-power-state-imminent state")
	flag.DurationVar(&c.SuspendImminentDelay, "suspend-imminent-delay", c.SuspendImminentDelay,
		"Duration for which low-power-state-imminent state is held")
	flag.DurationVar(&c.InhibitorDuration, "inhibitor-duration", c.InhibitorDuration,
		"Duration the system is held active after suspend is issued")
	flag.DurationVar(&c.HibernationTimer, "hibernation-timer", c.HibernationTimer,
		"Duration of the hibernation timer")

	flag.StringVar(&c.SocketPath, "socket-path", c.SocketPath,
		"Path for the Unix domain socket for inhibitor connections")

	flag.BoolVar(&c.DryRun, "dry-run", c.DryRun,
		"Dry run state (don't actually issue power state changes)")

	flag.StringVar(&c.DefaultState, "default-state", c.DefaultState,
		"Fallback power state if pm.default-state setting is not configured (run, suspend, hibernate, hibernate-manual, hibernate-timer, reboot)")
}
