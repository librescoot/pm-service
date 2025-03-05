# Rescoot Power Management Service

The power management service is responsible for managing power states (suspend, hibernate, poweroff, reboot) on the Rescoot scooter.

## Features

- Manages transitions between power states (run, suspend, hibernate, reboot)
- Monitors vehicle and battery state via Redis
- Handles inhibitors to prevent unwanted power state changes
- Provides a timer-based hibernation mechanism
- Publishes power state changes to Redis

## Dependencies

- Redis server
- D-Bus system bus

## Building

```bash
# Build for ARM target (armv7eabi)
make build

# Build for local architecture
make build-local
```

## Installation

```bash
# Install to /usr/bin and set up systemd service
sudo make install
```

## Deployment

```bash
# Deploy to development device
make deploy-dev

# Deploy to production device
make deploy-prod
```

## Configuration

The service can be configured using command-line flags:

```
  -dry-run
        Dry run (don't actually issue power state changes)
  -default-state string
        Default power state (run, suspend, hibernate, hibernate-manual, hibernate-timer, reboot) (default "suspend")
  -hibernation-timer duration
        Duration of the hibernation timer (default 5d)
  -ignore-services
        Ignore the state of currently active services when evaluating if the system can be suspended
  -inhibitor-duration duration
        Duration the system is held active after suspend is issued (default 500ms)
  -pre-suspend-delay duration
        Delay between standby and low-power-state-imminent state (default 1m0s)
  -redis-host string
        Redis host (default "localhost")
  -redis-port int
        Redis port (default 6379)
  -socket-path string
        Path for the Unix domain socket for inhibitor connections (default "/tmp/suspend_inhibitor")
  -suspend-imminent-delay duration
        Duration for which low-power-state-imminent state is held (default 5s)
```

## Redis Communication

### Subscriptions

- `vehicle` channel for vehicle state changes
- `battery:0` channel for battery state changes

### Publications

- `power-manager` channel for power state changes
- `power-manager:busy-services` for inhibitor status

### Commands

- `scooter:power` list for power state commands (run, suspend, hibernate, hibernate-manual, hibernate-timer, reboot)

## Power States

- **run**: Normal operation
- **suspend**: Suspend to RAM
- **hibernate**: Power off
- **hibernate-manual**: Power off initiated manually
- **hibernate-timer**: Power off initiated by hibernation timer
- **reboot**: System reboot

## Inhibitors

Inhibitors can be used to prevent power state changes. There are two types of inhibitors:

- **block**: Blocks power state changes completely
- **delay**: Delays power state changes for a short period

Inhibitors can be created by connecting to the Unix domain socket at the configured path.
