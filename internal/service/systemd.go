package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// systemdManager installs a systemd *user* unit.
//
// A system unit is wrong for voxen: the daemon needs the user's audio session,
// which only the user manager has. A system unit would start before login and see
// no audio devices at all.
type systemdManager struct{}

const systemdUnitName = "voxend.service"

func (m *systemdManager) Name() string { return "systemd (user)" }

func (m *systemdManager) unitPath() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnitName), nil
}

func (m *systemdManager) Install(opts Options) error {
	path, err := m.unitPath()
	if err != nil {
		return err
	}
	if err := ensureInstallDirs(opts.Config, ""); err != nil {
		return err
	}

	unit := m.render(opts)
	if err := writeFileAtomic(path, []byte(unit)); err != nil {
		return fmt.Errorf("service: write unit: %w", err)
	}

	if _, err := runCommand("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if _, err := runCommand("systemctl", "--user", "enable", systemdUnitName); err != nil {
		return err
	}
	// Starting is best effort: enabling is what makes it survive a reboot, and a
	// start failure should not leave the machine in a half-configured state
	// without the operator seeing the reason.
	if _, err := runCommand("systemctl", "--user", "restart", systemdUnitName); err != nil {
		return fmt.Errorf("service: unit installed and enabled, but it did not start: %w", err)
	}
	return nil
}

func (m *systemdManager) Uninstall(opts Options) error {
	path, err := m.unitPath()
	if err != nil {
		return err
	}

	// Uninstalling something that is not installed is a no-op, not an error: that
	// is what a repeated or scripted uninstall looks like.
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		_, _ = runCommand("systemctl", "--user", "daemon-reload")
		return nil
	}

	// Stop before disabling so a running call is torn down cleanly.
	_, _ = runCommand("systemctl", "--user", "stop", systemdUnitName)
	if _, err := runCommand("systemctl", "--user", "disable", systemdUnitName); err != nil {
		// "does not exist" means the unit file is already gone; anything else is a
		// real failure worth reporting.
		if !strings.Contains(err.Error(), "does not exist") && !strings.Contains(err.Error(), "No such file") {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: remove unit: %w", err)
	}
	_, _ = runCommand("systemctl", "--user", "daemon-reload")
	return nil
}

func (m *systemdManager) Status(opts Options) (Status, error) {
	st := Status{Supervisor: m.Name()}
	path, err := m.unitPath()
	if err != nil {
		return st, err
	}
	if _, err := os.Stat(path); err != nil {
		return st, nil
	}
	st.Installed = true

	if out, err := runCommand("systemctl", "--user", "is-enabled", systemdUnitName); err == nil {
		st.Enabled = strings.TrimSpace(out) == "enabled"
	}
	if out, err := runCommand("systemctl", "--user", "is-active", systemdUnitName); err == nil {
		st.Running = strings.TrimSpace(out) == "active"
	}
	st.Detail = path
	return st, nil
}

// render generates the unit for one installation.
//
// %t expands to the runtime directory, %h to the user's home. Using specifiers
// rather than expanded absolute paths keeps the unit portable between machines
// and avoids embedding a username that would be stale if the home moved.
func (m *systemdManager) render(opts Options) string {
	return `[Unit]
Description=voxen encrypted peer-to-peer voice daemon
Documentation=https://github.com/s4chdev/voxen
# voxen needs the user's audio session, which the user manager provides.
After=pipewire.service wireplumber.service pulseaudio.service
PartOf=graphical-session.target
Wants=pipewire.service

[Service]
Type=simple
ExecStart=` + opts.Binary + ` --foreground --config ` + opts.Config + systemdEnvBlock(opts.Env) + `
ExecStartPre=/bin/mkdir -p %t/voxen
Restart=on-failure
RestartSec=2s
TimeoutStopSec=5s
StartLimitIntervalSec=60
StartLimitBurst=5

# Hardening verified by running this unit. Two settings that look sensible here
# break it and are deliberately absent:
#   ProtectSystem=strict  makes /run/user/$UID read-only, so the IPC socket
#                         cannot be created.
#   PrivateTmp=yes        hides /tmp, which the daemon may use for its socket when
#                         the default path exceeds the platform limit.
NoNewPrivileges=yes
ProtectHome=read-only
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictSUIDSGID=yes
RestrictNamespaces=yes
RestrictRealtime=yes
LockPersonality=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
SystemCallArchitectures=native

# Every directory the daemon writes must be listed, or writes fail inside the
# namespace created by ProtectHome. These are generated from the profile the installer
# was pointed at, not hardcoded: a profile selected through VOXEN_* can live anywhere,
# and a hardcoded list made the unit fail to start with "No such file or directory"
# for every non-default profile.
ReadWritePaths=` + strings.Join(rwPaths(opts), " ") + `

[Install]
WantedBy=default.target
`
}

// rwPaths returns the directories the daemon writes, for the unit's sandbox.
//
// The runtime directory is always included: ProtectHome makes /run/user/$UID read-only
// inside the namespace, so omitting it breaks socket creation.
func rwPaths(opts Options) []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, existing := range out {
			if existing == p {
				return
			}
		}
		out = append(out, p)
	}
	// %t is the systemd specifier for the runtime directory; using it keeps the unit
	// portable rather than embedding one machine's path.
	add("%t/voxen")
	add(opts.ConfigDir)
	add(opts.StateDir)
	if opts.ConfigDir == "" && opts.StateDir == "" {
		// No profile given: fall back to the conventional layout so a unit written by
		// an older caller still works.
		add("%h/.config/voxen")
		add("%h/.local/state/voxen")
	}
	return out
}

// ensureInstallDirs creates the directories the unit lists in ReadWritePaths.
//
// systemd refuses to start a unit whose ReadWritePaths do not exist, and it would
// create them with its own (umask-derived) modes. Creating them through voxen
// keeps them private.
func ensureInstallDirs(configPath, stateDir string) error {
	if configPath == "" {
		return errors.New("service: config path is required")
	}
	configDir := filepath.Dir(configPath)
	dirs := []string{configDir}
	if stateDir != "" {
		dirs = append(dirs, stateDir)
	} else {
		dirs = append(dirs, filepath.Join(filepath.Dir(configDir), "state"))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("service: create %s: %w", dir, err)
		}
	}
	return nil
}

// systemdEnvBlock renders Environment= directives for the profile variables.
//
// The values are literal: systemd does not expand shell variables in a unit, and
// each assignment is quoted so a path containing spaces survives.
func systemdEnvBlock(env []string) string {
	lines := envLines(env)
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	for _, kv := range lines {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		b.WriteString("\nEnvironment=\"" + name + "=" + value + "\"")
	}
	return b.String()
}

var _ Manager = (*systemdManager)(nil)
