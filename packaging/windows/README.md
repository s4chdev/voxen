# voxen Windows notes

Phase 1 ships a working CLI and daemon for Windows, built and tested in the
project CI matrix on `windows-latest` (amd64 and arm64 compile targets).

## Build

```powershell
go build -trimpath -o bin\voxen.exe  .\cmd\voxen
go build -trimpath -o bin\voxend.exe .\cmd\voxend
```

No cgo, no MSVC build tools: phase 1 is pure Go. Phase 2 adds miniaudio
(WASAPI), which uses cgo; that build will need a MinGW-w64 or LLVM toolchain.

## Layout

| Purpose | Path |
|---|---|
| Config + identity + trust store | `%APPDATA%\voxen` |
| State, PID, logs | `%LOCALAPPDATA%\voxen` (logs in `logs\`) |
| Daemon endpoint | `\\.\pipe\voxen-<hash-of-config-dir>` |

The pipe name is derived from the configuration directory so several profiles
can run side by side without colliding.

## Security

The named pipe is created with this security descriptor:

```
D:P(A;;GA;;;<current user SID>)(A;;GA;;;SY)(A;;GA;;;BA)
```

That is: deny network access, grant full access to the current user, SYSTEM and
the local Administrators group, and no inherited ACEs. Another user on the same
machine cannot connect even if they guess the pipe name.

Key files are written with a restrictive ACL via Go's `os` package defaults plus
the `%APPDATA%` per-user directory; voxen additionally refuses to start if it is
running as a different user than the one that created the profile.

## Running the daemon

`voxen start` re-executes `voxen.exe daemon` with `DETACHED_PROCESS |
CREATE_NEW_PROCESS_GROUP` and no console window, then waits for the pipe to answer.
`voxen stop` asks it to shut down over the pipe; `voxen stop --force` terminates the
PID recorded in `%LOCALAPPDATA%\voxen\voxend.pid`.

For a service deployment (phase 7), wrap `voxend.exe --foreground` in a service
host. Do **not** run the daemon as `LocalSystem`: WASAPI capture is
session-scoped and a system service cannot see the interactive user's audio
devices.

## Not yet implemented on Windows

- WASAPI audio capture and playback (phase 2).
- Service wrapper and event-log integration (phase 7).
- Code-signing of the release binaries (phase 8).
