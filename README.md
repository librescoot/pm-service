# Librescoot Power Management Service

The power management service is responsible for managing power states (run, suspend, hibernate, poweroff, reboot) on the scooter. It monitors vehicle and battery state via Redis, handles power state transitions, and manages inhibitors to prevent unwanted power state changes.

## Overview

The service is designed to optimize power consumption by transitioning the scooter to appropriate power states based on its operational status. It communicates with other system components via Redis and uses systemd to execute power state changes.

## Features

- Manages transitions between power states (run, suspend, hibernate, poweroff, reboot)
- Monitors vehicle and battery state via Redis
- Handles inhibitors to prevent unwanted power state changes
- Provides a timer-based hibernation mechanism
- Publishes power state changes to Redis
- Supports delayed power state transitions with configurable timers
- Manages modem power state during low-power transitions
- Provides a Unix domain socket interface for inhibitor connections

## Architecture

The service consists of several key components:

- **Power Manager**: Handles power state transitions and systemd interactions
- **Inhibitor Manager**: Manages inhibitors that can block or delay power state changes
- **Service**: Coordinates between components and handles Redis communication

## Power States

The service supports the following power states:

- **run**: Normal operation mode
- **suspend**: Suspend to RAM (low power state with quick resume)
- **hibernate**: Power off the system
- **hibernate-manual**: Power off initiated manually
- **hibernate-timer**: Power off initiated by hibernation timer
- **reboot**: System reboot

### Power State Transitions

Power state transitions follow these priority rules:
1. run (highest priority)
2. hibernate-manual
3. hibernate
4. hibernate-timer
5. suspend/reboot (lowest priority)

The service will not transition to a lower priority state if a higher priority state is requested.

## Inhibitors

Inhibitors can be used to prevent power state changes. There are two types of inhibitors:

- **block**: Blocks power state changes completely
- **delay**: Delays power state changes for a short period

Inhibitors can be created by:
1. Connecting to the Unix domain socket at the configured path
2. Programmatically via the inhibitor manager API

The service maintains a special inhibitor for the modem to ensure proper shutdown sequence.

## Redis Communication

### Subscriptions

The service subscribes to the following Redis channels:

- `vehicle` channel for vehicle state changes
- `battery:0` channel for battery state changes

### Publications

The service publishes to the following Redis channels:

- `power-manager` channel for power state changes
- `power-manager:busy-services` for inhibitor status

### Commands

The service listens for commands on:

- `scooter:power` list for power state commands (run, suspend, hibernate, hibernate-manual, hibernate-timer, reboot)

The service issues commands on:

- `scooter:modem` list for modem control (disable)

## Dependencies

- Redis server
- D-Bus system bus
- systemd

## Building

```bash
# Build for ARM target (armv7eabi)
make build

# Build for local architecture
make build-local

# Install dependencies
make deps
```

## Installation

```bash
# Install to /usr/bin and set up systemd service
sudo make install

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

## Development

### Testing

```bash
# Run tests
make test
```

### Debugging

When debugging, you can use the `-dry-run` flag to prevent actual power state changes:

```bash
./librescoot-pm -dry-run
```

## Troubleshooting

### Common Issues

- **Service fails to start**: Check Redis connectivity and D-Bus permissions
- **Power state changes not occurring**: Check for active inhibitors using Redis (`power-manager:busy-services`)
- **Unexpected power state changes**: Check vehicle and battery state in Redis

### Logs

The service logs to standard output, which is captured by systemd when running as a service. View logs with:

```bash
journalctl -u librescoot-pm.service
```
