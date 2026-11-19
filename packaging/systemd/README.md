# Running voxend under a supervisor

`voxen start` detaches a daemon that lives until you log out. That is fine for a
one-off call and wrong for daily use: it does not restart after a crash and does
not come back after a reboot.

The fix is a **user-level** supervisor:

```sh
voxen service install     # write the unit, enable it, start it
voxen service status      # installed? enabled? running?
voxen service uninstall   # stop and remove
```

The generated files are also kept in this directory for reference. They are the
same content `voxen service install` writes.

## Why not a system service?

A system service (systemd system unit, launchd daemon, Windows service) starts
before login, in a session with **no audio devices**. The daemon would run
perfectly and hear nothing. Every platform therefore uses its per-user mechanism:

| Platform | Mechanism | Location |
|---|---|---|
| Linux | `systemd --user` unit | `~/.config/systemd/user/voxend.service` |
| macOS | LaunchAgent | `~/Library/LaunchAgents/sh.voxen.voxend.plist` |
| Windows | logon task (`schtasks`) | Task Scheduler, per-user |

## Linux: systemd user unit

The unit is verified by actually running it, and two directives that look like
good hardening are deliberately absent because they break the daemon:

- **`ProtectSystem=strict`** makes `/run/user/$UID` read-only inside the unit's
  mount namespace, so the IPC socket cannot be created. Observed failure:
  `listen unix /run/user/1000/voxen/voxend.sock: bind: read-only file system`.
- **`PrivateTmp=yes`** gives the daemon a private `/tmp`. When the configured
  socket path exceeds the platform length limit, voxen falls back to a `/tmp`
  path; the CLI, outside the namespace, would then look in the wrong place.

`ReadWritePaths` must list every directory the daemon writes, including the
runtime directory. `ProtectHome=read-only` makes it read-only otherwise.

### Manual install

```sh
mkdir -p ~/.config/systemd/user ~/.local/bin
install -m 0755 voxend ~/.local/bin/voxend
install -m 0755 voxen  ~/.local/bin/voxen
voxen init                       # creates private config and state directories
cp packaging/systemd/voxend.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now voxend.service
systemctl --user status voxend.service
```

### Verifying it actually works

```sh
systemctl --user is-enabled voxend.service   # expect: enabled
systemctl --user is-active  voxend.service   # expect: active
voxen status                                 # the CLI must see the daemon

# crash recovery
kill -9 "$(systemctl --user show -p MainPID --value voxend.service)"
sleep 3
voxen status                                 # expect: running, with a new PID

# a clean stop must stay stopped, not restart
voxen stop
sleep 3
systemctl --user is-active voxend.service    # expect: inactive
```

An automated version of exactly this is
`VOXEN_TEST_SYSTEMD=1 go test ./tests/integration/ -run TestSystemdServiceLifecycle`.
It is opt-in because it installs a real unit into the running user session.

## macOS: LaunchAgent

```sh
mkdir -p ~/Library/LaunchAgents ~/.local/bin
install -m 0755 voxend ~/.local/bin/voxend
voxen init
voxen service install            # writes the plist and loads it
launchctl list | grep voxen      # confirm it is registered
```

`LimitLoadToSessionType` is `Aqua` so the agent only runs in a graphical session
where CoreAudio exists. `KeepAlive` uses `SuccessfulExit=false`, so a crash
restarts it but `voxen stop` does not fight the supervisor.

**Not verified on real hardware.** The plist renders correctly and is covered by
a content test, but launchd behaviour has not been exercised here.

## Windows: logon task

```powershell
voxen init
voxen service install
schtasks /Query /TN voxen-daemon /FO LIST
```

`schtasks` cannot set environment variables for a task, so a profile selected
through `VOXEN_*` variables is not supported under the Windows supervisor; the
task uses the default layout.

**Not verified on real hardware.**

## Troubleshooting

| Symptom | Cause |
|---|---|
| Unit exits `226/NAMESPACE` | A `ReadWritePaths` directory does not exist. Run `voxen init` first. |
| `bind: read-only file system` | The runtime directory is missing from `ReadWritePaths`. |
| `voxen status` says not running while systemd says active | The supervisor and CLI resolved different profiles. Re-run `voxen service install` from the shell whose environment you use. |
| Service runs but hears nothing | A system unit instead of a user unit. Check with `systemctl --user status`. |
