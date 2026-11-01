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

- `run`, `suspend`, `hibernate`, `hibernate-manual`, `hibernate-auto`, `hibernate-timer`, and `reboot`
- `hibernate-for:<seconds>` to request a timed hibernation
- `hibernate-cancel` to return to `run` and disarm the wake timer

Explicit `hibernate`, `hibernate-manual`, and `hibernate-for:<seconds>` requests are accepted only in `parked` or `stand-by`. Ordinary explicit requests use the manual-hibernation priority. From `parked`, PM records the intent and asks vehicle-service to prepare a graceful shutdown; actual power-off still requires `stand-by`. Timed preparation preserves its duration and requires the nRF to echo that duration. No GPS fix or wall-clock synchronization is needed for a relative timed request.

`hibernate-auto`, the idle timer, reserve-power protection, and scheduled hibernation do not initiate vehicle locking. Scheduled hibernation retains its clock-validity gate and deferred wake-by deadline.

Vehicle preparation uses `scooter:state` command `prepare-hibernate:<request-id>` and a matching `power-manager[hibernate-request-id]`. Vehicle rejection reports `hibernate-preparation-failed:<request-id>` on `scooter:power`. Preparation times out after 30 seconds. Cancellation, shutdown interruption, and PM restart invalidate outstanding preparation; cancellation also disarms the wake timer and does not unlock the vehicle.

It accepts `ondemand`, `powersave`, and `performance` from `scooter:governor`.

Power state is published in the `power-manager` hash. It also carries `wake-timer-seconds`, the exact nRF echo `wake-timer-ack-seconds`, and the legacy `wake-timer-armed` telemetry flag. Explicit request progress is exposed as `hibernate-status` (`idle`, `preparing-vehicle`, `preparing-power`, `waiting-inhibitors`, `waiting-wake-timer`, `powering-off`, `rejected`, `cancelled`, or `failed`) and `hibernate-error`. Command-queue acceptance is not confirmation of shutdown. Active inhibitor summaries are published under `power-manager:busy-services`.

Redis/Valkey inhibitors are stored as JSON values in the `power:inhibits` hash and synchronized when the `power:inhibits` channel is published. Inhibitors may be `delay`, `suspend-only`, or the default blocking type. Blocking inhibitors are honored by automatic last-ditch hibernation as well as explicit transitions. A `suspend-only` inhibitor does not block any hibernation/poweroff path, including last-ditch; services performing an OTA install, boot-region write, activation, or commit that cannot tolerate power loss must hold a blocking inhibitor for the whole critical section. The current inhibitor protocol does not communicate update phase, resumability, or reserve-power deadlines. The local inhibitor listener uses the path selected by `--socket-path`. A connection is registered as a blocking inhibitor before its acknowledgment is sent.

Low-power entry is guarded by live vehicle and battery state. In particular, suspend is restricted to stand-by and is not entered while a main battery is present or active. Battery presence and remote-access policy are rechecked during preparation and after the nRF suspend acknowledgement. When battery recovery or changed suspend eligibility cancels a pending transition, PM restores any modem it disabled for that transition.

Automatic reserve-power hibernation is cancelled if its emergency condition clears before poweroff, restoring the previous target. This includes recovery of a charged main battery during the countdown or inhibitor wait. The condition is checked again immediately before poweroff. Battery recovery does not cancel explicit hibernation, timer, or reboot requests. These are system-changing operations and should be issued only by trusted services.

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
| `pm.suspend-when-online` | `true` | Only relevant without a main battery. "Online" means at least one provider field in `remote-access` is `connected` (radio-gaga/Sunshine, uplink-service, or a custom provider), not mere modem registration. Default `true` allows suspend while remotely reachable; `false` stays awake while remote access is connected, at the cost of draining the auxiliary battery within a few days. |
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

The image recipe installs `/usr/bin/pm-service` and `librescoot-pm.service`. The unit runs as `root`, requires `valkey.service`, starts after the vehicle, settings, and Valkey services, and restarts on failure.

The unit uses `Type=notify`: readiness is sent only after the inhibitor socket,
FSM and Redis watchers are initialized. It orders PM before battery-service;
enabling the unit also installs a battery-service requirement on PM. Battery
startup therefore waits for readiness and fails if PM cannot start. Existing
battery retry handling remains available for transient runtime failures. No
readiness notification is needed when running PM outside systemd.

Production operation requires:

- Redis or Valkey and the vehicle/battery publishers that supply state;
- a system D-Bus and systemd/logind capable of the requested power actions;
- permission to access the configured Unix socket and power-management sysfs interfaces; and
- the Bluetooth/nRF52 path for timed hibernation wake-timer handshakes.

Deploy PM, vehicle-service's correlated preparation handler, and bluetooth-service's `wake-timer-ack-seconds` publisher together. Timed hibernation fails closed if the matching bridge is unavailable. The nRF wire protocol uses its existing duration echo.

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

Original Librescoot code in this repository is available, at your option, under
[AGPL-3.0-only](LICENSE) or [CC-BY-NC-SA-4.0](LICENSE.CC-BY-NC-SA-4.0).
Third-party code retains its existing license.

Made with ❤️ by the Librescoot community

## Clock coordinator core

`clock-coordinator` is a separate executable. This core is not wired into the
production units or chrony configuration yet. Chronyd remains the only system
clock writer; configure it without any automatic `makestep` directive before
using the coordinator's recovery policy. Keep chronyd running during source
probes. The coordinator does not set the system clock directly.

The daemon requires root and exclusively locks `/run/clock-coordinator/owner.lock`.
Its state and socket are in that root-owned, non-group/world-writable directory.
The lock file must not be removed while the daemon is running. A restart in the
same boot reloads the episode and spent budget; an in-flight command becomes
`unknown`. `--run-dir` changes the runtime directory and `--refclock-dir` changes
the directory for the chrony sockets. Run `clock-coordinator --wait=10s` as a
bounded local startup wait. It exits successfully when quality becomes available
or when the deadline expires; absence of a clock source never blocks startup.

Configure chrony to create these three SOCK refclocks in `/run/chrony` (or the
`--refclock-dir` directory), with the indicated reference IDs:

```
refclock SOCK /run/chrony/nrf.sock refid NRF1 precision 1 poll 0 filter 1 minsamples 1 maxsamples 1
refclock SOCK /run/chrony/gps.sock refid GPS1 precision 1 poll 0 filter 1 minsamples 1 maxsamples 1
refclock SOCK /run/chrony/user.sock refid USR1 precision 1 poll 0 filter 1 minsamples 1 maxsamples 1
```

SOCK has no per-sample uncertainty field. The one-second configured precision
is a conservative floor for the accepted producer uncertainty. The packet uses
the native Linux timeval layout; this assumes chronyd uses the native ABI of
its process (32-bit timeval on ARMv7 and 64-bit timeval on 64-bit Linux).

The socket protocol is one UTF-8 JSON object followed by a newline per Unix
stream connection at `/run/clock-coordinator/control.sock`; the response is one
JSON object and newline. The socket mode is 0600. Requests are limited to 4096
bytes, 16 concurrent connections, and four seconds per connection. Unknown
fields are rejected. The response contains `ok`, optional `error`, and `quality`.

- `{"op":"status"}` returns the boot ID and generation needed by producers,
  source (`unknown`, `local`, `nrf`, `gps`, `user`, or `ntp`), `external`,
  `age_ns`, `residual_seconds`, `uncertainty_seconds`, `episode`, `outcome`,
  `preparing`, `nrf_eligible`, and `nrf_deadline_boot_ns`. Eligibility is a
  snapshot of recovery admission;
  a later submission is rechecked against the deadline and current state.
  Established external time disables nRF recovery for the rest of the boot,
  including coordinator restarts. `external` identifies a selected NTP or GPS source, not an
  unconditional trust assertion. NTP classification also checks the selected
  peer's reference ID using `chronyc ntpdata`, excluding local references.
  Consumers must check age, residual and
  uncertainty. Unknown/in-flight outcomes do not publish source quality.
- `{"op":"submit","boot_id":"...","generation":1,"source":"nrf",
  "utc_unix_ns":1700000000000000000,"receipt_boot_ns":10000000000,
  "sample_age_ns":0,"uncertainty_ns":1000000000}` submits a sample. Sources
  are `nrf`, `gps` (already corrected by the producer), and `user` (explicit
  Bluetooth user time). `utc_unix_ns` is the UTC value at sample time;
  `receipt_boot_ns` is the producer's CLOCK_BOOTTIME at receipt, and
  `sample_age_ns` is the age already elapsed at receipt. An optional
  `deadline_boot_ns` rejects a sample at or after that CLOCK_BOOTTIME deadline.
  The coordinator adds
  subsequent CLOCK_BOOTTIME elapsed time. Both age fields include suspend;
  samples older than 30 seconds, future receipt times, old boot/generation
  tokens and uncertainties over one second are rejected. GPS must not be a
  fallback value derived from the system clock. A successful submission means
  the datagram was delivered, not that chronyd selected the source. User and
  nRF samples are holdover, never external confirmation.
- `{"op":"prepare"}` fences new dispatches and waits up to 3.5 seconds for an
  in-flight command. An unresolved outcome aborts preparation and returns an
  error; PM must not suspend on that error. `{"op":"abort"}` cancels a
  successful preparation. `{"op":"resume","boot_id":"...","generation":1}`
  completes it using the token obtained before preparation and increments the
  generation even if no clock loss was found. Producers must fetch the new token.

Episodes have a 90-second admission window. A selected fresh source, residual of
at least one second, and uncertainty at most five seconds are required for the
one immediate, parameterless `chronyc makestep`. A second known pre-dispatch
start failure exhausts the attempt. A timeout or other error after process start
is `unknown`; it is never blindly retried. A fresh tracking observation after
at least ten seconds, residual below 0.1 seconds and uncertainty below five
seconds reconciles it. A single late external episode per boot is possible after
an earlier deadline for a fresh large external residual. A resume episode is
opened only when CLOCK_BOOTTIME advanced over two seconds more than UTC during
preparation/suspend. A command admitted before a deadline may complete later;
there is no persistent chrony step permission.
