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

	SocketPath   string
	DryRun       bool
	DefaultState string
}

func New() *Config {
	return &Config{
		RedisHost:            "localhost",
		RedisPort:            6379,
		PreSuspendDelay:      60 * time.Second,
		SuspendImminentDelay: 5 * time.Second,
		InhibitorDuration:    500 * time.Millisecond,
		HibernationTimer:     5 * 24 * time.Hour, // 5 days
		SocketPath:           "/tmp/suspend_inhibitor",
		DryRun:               false,
		DefaultState:         "suspend",
	}
}

func (c *Config) Parse() {
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
		"Default power state (run, suspend, hibernate, hibernate-manual, hibernate-timer, reboot)")

	flag.Parse()
}
