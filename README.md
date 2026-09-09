# Librescoot Power Management Service

Part of the [Librescoot](https://librescoot.org/) open-source platform.

The Power Management Service coordinates the vehicle's low-power lifecycle. It observes vehicle, battery, reserve-power, connectivity, settings, and inhibitor state through Redis or Valkey; publishes its state through the same IPC bus; and asks systemd to suspend, power off, or reboot only after the relevant guards have passed.
## Capabilities

- Manages `run`, `suspend`, `hibernate`, `hibernate-manual`, `hibernate-timer`, `hibernate-for`, and `reboot` requests through a finite-state machine.
- Watches vehicle and both main-battery slots, auxiliary and control-board battery telemetry, connectivity, settings, and power-manager acknowledgements.
- Publishes current power-manager state and active-inhibitor information.
- Implements a configurable hibernation timer and scheduled hibernation settings.
- Coordinates `hibernate-for` with the nRF52 wake timer and aborts the power-off path if the timer is not acknowledged.
- Uses systemd's D-Bus interfaces to issue power actions and observes suspend/resume.
- Supports local Unix-socket inhibitors and Redis/Valkey-backed inhibitors.
- Can apply `ondemand`, `powersave`, or `performance` CPU-governor requests.

## Operation and interfaces

The service consumes list commands from `scooter:power`:

- `run`, `suspend`, `hibernate`, `hibernate-manual`, `hibernate-timer`, and `reboot`
- `hibernate-for:<seconds>` to request a timed hibernation
- `hibernate-cancel` to return to `run` and disarm the wake timer

It accepts `ondemand`, `powersave`, and `performance` from `scooter:governor`.

Power state is published in the `power-manager` hash. The same hash carries the nRF52 wake-timer request/acknowledgement fields used for timed hibernation. Active inhibitor summaries are published under `power-manager:busy-services`.

Redis/Valkey inhibitors are stored as JSON values in the `power:inhibits` hash and synchronized when the `power:inhibits` channel is published. Inhibitors may be `delay`, `suspend-only`, or the default blocking type. Blocking inhibitors are honored by automatic last-ditch hibernation as well as explicit transitions. A `suspend-only` inhibitor does not block any hibernation/poweroff path, including last-ditch; services performing an OTA install, boot-region write, activation, or commit that cannot tolerate power loss must hold a blocking inhibitor for the whole critical section. The current inhibitor protocol does not communicate update phase, resumability, or reserve-power deadlines. The local inhibitor listener uses the path selected by `--socket-path`.

Low-power entry is guarded by live vehicle and battery state. In particular, suspend is restricted to stand-by and is not entered while a main battery is present or active. Hibernation and reboot are system-changing operations; their requests should be issued only by trusted services.

## Configuration

Command-line flags provide the service configuration:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--redis-host` | `localhost` | Redis/Valkey host |
| `--redis-port` | `6379` | Redis/Valkey port |
| `--default-state` | `suspend` | Fallback low-power target |
| `--hibernation-timer` | `72h` | Idle hibernation timer |
| `--pre-suspend-delay` | `1m` | Delay before the suspend-imminent state |
| `--suspend-imminent-delay` | `5s` | Duration of the suspend-imminent state |
| `--inhibitor-duration` | `500ms` | Post-suspend delay-inhibitor duration |
| `--socket-path` | `/tmp/suspend_inhibitor` | Unix socket for local inhibitors |
| `--dry-run` | `false` | Log power actions instead of issuing them |
| `--version` | — | Print the build version and exit |

The service also watches these fields in the `settings` hash:

| Setting | Default | Purpose |
| --- | --- | --- |
| `pm.last-ditch-hibernate-enabled` | `true` | Enables automatic reserve-power last-ditch hibernation. `false` suppresses only this trigger; explicit/manual hibernate, hibernate-for, timers, suspend, reboot, and default-state handling are unchanged. Invalid values are logged and fail safe to enabled. |
| `pm.hibernation-timer` | command-line value | Idle hibernation timer in seconds. |
| `pm.default-state` | command-line value | Default low-power target. |
| `pm.suspend-when-online` | `true` | Allows suspend while remotely reachable. |
| `pm.wake-timer-max-seconds` | configured cap | Maximum hibernate-for wake interval. |
| `pm.wake-timer-ack-timeout` | configured timeout | nRF wake-timer acknowledgement timeout. |
| `pm.scheduled-hibernate-enabled` | `false` | Enables cron-based hibernation. |
| `pm.scheduled-hibernate-cron` | empty | Scheduled hibernation cron expression. |
| `pm.scheduled-hibernate-duration` | empty | Scheduled hibernation duration. |

A valid `pm.default-state` overrides the command-line fallback. Boolean settings use the lowercase `true`/`false` form.

## Build and test

A Go toolchain is required. The default target cross-compiles a Linux ARMv7 executable.

```sh
make build       # bin/pm-service for ARMv7
make build-host  # bin/pm-service for the current host
make test
```

`make lint` and `make clean` are also available.

## Deployment and runtime dependencies

The image recipe installs `/usr/bin/pm-service` and `librescoot-pm.service`. The unit runs as `root`, requires `valkey.service`, starts after the vehicle, battery, settings, and Valkey services, and restarts on failure.

Production operation requires:

- Redis or Valkey and the vehicle/battery publishers that supply state;
- a system D-Bus and systemd/logind capable of the requested power actions;
- permission to access the configured Unix socket and power-management sysfs interfaces; and
- the Bluetooth/nRF52 path for timed hibernation wake-timer handshakes.

Use `--dry-run` for integration checks that must not change the machine power state.

```sh
systemctl status librescoot-pm.service
journalctl -u librescoot-pm.service
```

## Operational and security notes

- This process can suspend, power off, reboot, and change CPU governor settings. Limit its systemd and Redis/Valkey command surfaces to trusted principals.
- Treat access to `scooter:power`, `scooter:governor`, `power:inhibits`, and the inhibitor socket as privileged.
- Preserve the nRF52 wake-timer path before using `hibernate-for`; the service deliberately abandons that transition when it cannot confirm the timer was armed.
- Review inhibitors and `power-manager` state before diagnosing a deferred transition. Send `SIGTERM` or `SIGINT` for graceful shutdown.

## License

This project is licensed under the [GNU Affero General Public License v3.0](LICENSE).

Made with ❤️ by the Librescoot community
