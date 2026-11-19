package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// launchdManager installs a per-user LaunchAgent.
//
// A LaunchDaemon would run as root before login and see no audio devices, so the
// agent runs in the Aqua session for the logged-in user.
type launchdManager struct{}

const launchdLabel = "sh.voxen.voxend"

func (m *launchdManager) Name() string { return "launchd (LaunchAgent)" }

func (m *launchdManager) plistPath() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func (m *launchdManager) Install(opts Options) error {
	path, err := m.plistPath()
	if err != nil {
		return err
	}
	home, err := homeDir()
	if err != nil {
		return err
	}
	logDir := filepath.Join(home, "Library", "Logs", "voxen")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return fmt.Errorf("service: create log directory: %w", err)
	}
	configDir := filepath.Dir(opts.Config)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("service: create config directory: %w", err)
	}

	plist := m.render(opts, logDir)
	if err := writeFileAtomic(path, []byte(plist)); err != nil {
		return fmt.Errorf("service: write plist: %w", err)
	}

	// unload-then-load is what applies a changed plist; load alone fails if the
	// job is already registered.
	_, _ = runCommand("launchctl", "unload", path)
	if _, err := runCommand("launchctl", "load", "-w", path); err != nil {
		return fmt.Errorf("service: load agent: %w", err)
	}
	return nil
}

func (m *launchdManager) Uninstall(opts Options) error {
	path, err := m.plistPath()
	if err != nil {
		return err
	}
	_, _ = runCommand("launchctl", "unload", "-w", path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: remove plist: %w", err)
	}
	return nil
}

func (m *launchdManager) Status(opts Options) (Status, error) {
	st := Status{Supervisor: m.Name()}
	path, err := m.plistPath()
	if err != nil {
		return st, err
	}
	if _, err := os.Stat(path); err != nil {
		return st, nil
	}
	st.Installed = true
	st.Enabled = true // a loaded LaunchAgent is enabled by definition
	st.Detail = path

	if out, err := runCommand("launchctl", "list", launchdLabel); err == nil {
		st.Running = strings.Contains(out, "\"PID\"")
	}
	return st, nil
}

// render builds the plist.
//
// KeepAlive is set with SuccessfulExit=false so a clean shutdown is respected but
// a crash restarts. LimitLoadToSessionType=Aqua keeps the agent out of contexts
// where audio is unavailable.
func (m *launchdManager) render(opts Options, logDir string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + launchdLabel + `</string>

    <key>ProgramArguments</key>
    <array>
        <string>` + opts.Binary + `</string>
        <string>--foreground</string>
        <string>--config</string>
        <string>` + opts.Config + `</string>
    </array>

    <key>RunAtLoad</key>
    <true/>

    <!-- Restart on crash, but honour a clean shutdown from "voxen stop". -->
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>

    <key>ThrottleInterval</key>
    <integer>2</integer>

    <!-- Audio capture requires an interactive session; never start at boot. -->
    <key>LimitLoadToSessionType</key>
    <string>Aqua</string>

    <key>ProcessType</key>
    <string>Interactive</string>

    <key>StandardOutPath</key>
    <string>` + filepath.Join(logDir, "voxend.out.log") + `</string>
    <key>StandardErrorPath</key>
    <string>` + filepath.Join(logDir, "voxend.err.log") + `</string>

    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin</string>` + launchdEnvEntries(opts.Env) + `
    </dict>
</dict>
</plist>
`
}

// launchdEnvEntries renders the profile variables as plist dict entries.
func launchdEnvEntries(env []string) string {
	var b strings.Builder
	for _, kv := range envLines(env) {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		b.WriteString("\n        <key>" + name + "</key>\n        <string>" + value + "</string>")
	}
	return b.String()
}

var _ Manager = (*launchdManager)(nil)
