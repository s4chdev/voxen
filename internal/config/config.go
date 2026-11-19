// Package config loads, validates and persists the voxen configuration.
//
// Configuration is YAML, stored in the per-user config directory. Unknown keys
// are rejected: a typo in a security-relevant setting must fail loudly instead
// of silently falling back to a default.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/s4chdev/voxen/internal/paths"
)

// CurrentVersion is the configuration schema version written by this build.
const CurrentVersion = 1

// Defaults for values that are not security decisions. Security limits are
// defined as constants in this package and are not user-tunable.
const (
	DefaultSampleRate      = 48000
	DefaultFrameDurationMS = 20
	DefaultBitrate         = 28000
	DefaultSTUNServer      = "stun:stun.l.google.com:19302"
)

// Hard limits. Values outside these ranges are rejected, never clamped, so an
// operator cannot accidentally weaken a session by editing YAML.
const (
	MinBitrate         = 6000
	MaxBitrate         = 510000
	MinFrameDurationMS = 10
	MaxFrameDurationMS = 60
	MaxParticipants    = 8
	MaxSTUNServers     = 4
	MaxDisplayNameLen  = 64
	MaxRoomNameLen     = 64
)

// Config is the complete validated configuration.
type Config struct {
	Version   int             `yaml:"version"`
	Identity  IdentityConfig  `yaml:"identity"`
	Signaling SignalingConfig `yaml:"signaling"`
	Audio     AudioConfig     `yaml:"audio"`
	Network   NetworkConfig   `yaml:"network"`
	Daemon    DaemonConfig    `yaml:"daemon"`
	Logging   LoggingConfig   `yaml:"logging"`
	Security  SecurityConfig  `yaml:"security"`
}

// IdentityConfig controls the local Ed25519 identity.
type IdentityConfig struct {
	DisplayName string `yaml:"display_name"`
	// File is the private key path. Relative paths are resolved against the
	// config directory.
	File string `yaml:"file"`
}

// SignalingConfig points voxen at a signaling server. voxen never trusts this
// server for media authentication; it only relays room metadata.
type SignalingConfig struct {
	URL string `yaml:"url"`
	// TLSSkipVerify disables certificate verification. It exists for local
	// testing only and is refused unless the URL is loopback.
	TLSSkipVerify bool          `yaml:"tls_skip_verify"`
	Timeout       time.Duration `yaml:"-"`
	TimeoutMS     int           `yaml:"timeout_ms"`
}

// AudioConfig controls the media pipeline. Values are normalised before use:
// all capture and playback is resampled to 48 kHz mono PCM.
type AudioConfig struct {
	InputDevice  string `yaml:"input_device"`
	OutputDevice string `yaml:"output_device"`
	SampleRate   int    `yaml:"sample_rate"`
	FrameMS      int    `yaml:"frame_ms"`
	Bitrate      int    `yaml:"bitrate_bps"`
	// EchoCancellation and NoiseSuppression are not implemented. miniaudio's
	// WebRTC preprocessing is not wired in, and silently accepting these flags
	// would tell a user their echo was handled when it was not. Setting either to
	// true is a validation error with an explanation, which is the honest
	// behaviour until the processing exists.
	EchoCancellation  bool `yaml:"echo_cancellation"`
	NoiseSuppression  bool `yaml:"noise_suppression"`
	InputGainPercent  int  `yaml:"input_gain_percent"`
	OutputGainPercent int  `yaml:"output_gain_percent"`
}

// NetworkConfig controls ICE. TURN is intentionally absent: voxen only performs
// direct peer-to-peer connections and reports failures instead of relaying.
type NetworkConfig struct {
	STUNServers []string `yaml:"stun_servers"`
	// ICETimeoutMS bounds connection establishment before a peer is reported
	// as unreachable.
	ICETimeoutMS int `yaml:"ice_timeout_ms"`
	// EnableMDNS publishes ICE candidates via multicast DNS. Disabled by
	// default so candidates are explicit.
	EnableMDNS bool `yaml:"enable_mdns"`
	// InterfaceFilter, when set, restricts ICE to named interfaces.
	InterfaceFilter []string `yaml:"interface_filter"`
}

// DaemonConfig controls local IPC.
type DaemonConfig struct {
	// Socket overrides the default IPC endpoint (Unix socket or Windows pipe).
	Socket string `yaml:"socket"`
	// StartupTimeoutMS bounds how long the CLI waits for a spawned daemon.
	StartupTimeoutMS int `yaml:"startup_timeout_ms"`
}

// LoggingConfig controls structured logging. Logs never contain key material.
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	// File, when set, receives logs in addition to the daemon's own stderr
	// handling. Relative paths are resolved against the log directory.
	File string `yaml:"file"`
}

// SecurityConfig contains explicit security policy that must not be relaxed by
// convenience flags.
type SecurityConfig struct {
	// RequireVerifiedPeers rejects any peer whose identity has not been
	// explicitly verified. voxen always sets this to true; the field exists so
	// the policy is visible and auditable in the config.
	RequireVerifiedPeers bool `yaml:"require_verified_peers"`
	// SessionTTLSeconds bounds the lifetime of a signed session statement.
	SessionTTLSeconds int `yaml:"session_ttl_seconds"`
	// MaxClockSkewSeconds is the tolerated skew when validating peer
	// statements and nonces.
	MaxClockSkewSeconds int `yaml:"max_clock_skew_seconds"`
}

// Default returns the configuration written by "voxen init".
func Default() Config {
	return Config{
		Version: CurrentVersion,
		Identity: IdentityConfig{
			File: paths.IdentityFileName,
		},
		Signaling: SignalingConfig{
			URL:       "ws://127.0.0.1:8080/ws",
			TimeoutMS: 10000,
		},
		Audio: AudioConfig{
			SampleRate:        DefaultSampleRate,
			FrameMS:           DefaultFrameDurationMS,
			Bitrate:           DefaultBitrate,
			EchoCancellation:  false,
			NoiseSuppression:  false,
			InputGainPercent:  100,
			OutputGainPercent: 100,
		},
		Network: NetworkConfig{
			STUNServers:  []string{DefaultSTUNServer},
			ICETimeoutMS: 15000,
		},
		Daemon: DaemonConfig{
			StartupTimeoutMS: 5000,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
		Security: SecurityConfig{
			RequireVerifiedPeers: true,
			SessionTTLSeconds:    300,
			MaxClockSkewSeconds:  60,
		},
	}
}

// Load reads cfg from path, applying defaults for absent fields and then
// validating the result. A missing file is an error: "voxen init" is explicit.
func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	return Parse(raw, filepath.Dir(path))
}

// Parse decodes YAML and validates it. baseDir is used to resolve relative
// paths deterministically, independent of the process working directory.
func Parse(raw []byte, baseDir string) (Config, error) {
	cfg := Default()
	// Zero the default list so YAML replaces instead of appending.
	cfg.Network.STUNServers = nil

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, errors.New("config: file is empty")
		}
		return Config{}, fmt.Errorf("config: invalid YAML: %w", err)
	}

	// A second document is always a mistake for this file format.
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return Config{}, errors.New("config: multiple YAML documents are not supported")
	}

	applyDefaults(&cfg)
	resolvePaths(&cfg, baseDir)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Version == 0 {
		cfg.Version = CurrentVersion
	}
	if cfg.Signaling.TimeoutMS == 0 {
		cfg.Signaling.TimeoutMS = 10000
	}
	if cfg.Audio.SampleRate == 0 {
		cfg.Audio.SampleRate = DefaultSampleRate
	}
	if cfg.Audio.FrameMS == 0 {
		cfg.Audio.FrameMS = DefaultFrameDurationMS
	}
	if cfg.Audio.Bitrate == 0 {
		cfg.Audio.Bitrate = DefaultBitrate
	}
	if cfg.Audio.InputGainPercent == 0 {
		cfg.Audio.InputGainPercent = 100
	}
	if cfg.Audio.OutputGainPercent == 0 {
		cfg.Audio.OutputGainPercent = 100
	}
	if len(cfg.Network.STUNServers) == 0 {
		cfg.Network.STUNServers = []string{DefaultSTUNServer}
	}
	if cfg.Network.ICETimeoutMS == 0 {
		cfg.Network.ICETimeoutMS = 15000
	}
	if cfg.Daemon.StartupTimeoutMS == 0 {
		cfg.Daemon.StartupTimeoutMS = 5000
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "text"
	}
	if cfg.Security.SessionTTLSeconds == 0 {
		cfg.Security.SessionTTLSeconds = 300
	}
	if cfg.Security.MaxClockSkewSeconds == 0 {
		cfg.Security.MaxClockSkewSeconds = 60
	}
	if cfg.Identity.File == "" {
		cfg.Identity.File = paths.IdentityFileName
	}
}

func resolvePaths(cfg *Config, baseDir string) {
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(baseDir, p)
	}
	cfg.Identity.File = resolve(cfg.Identity.File)
	cfg.Logging.File = resolve(cfg.Logging.File)
}

// Validate checks every field and returns one error describing all problems.
func (c Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.Version != CurrentVersion {
		add("version: unsupported schema version %d (want %d)", c.Version, CurrentVersion)
	}
	if len(c.Identity.DisplayName) > MaxDisplayNameLen {
		add("identity.display_name: longer than %d characters", MaxDisplayNameLen)
	}
	if c.Identity.File == "" {
		add("identity.file: must not be empty")
	}

	if c.Signaling.URL == "" {
		add("signaling.url: must not be empty")
	}
	if u, err := url.Parse(c.Signaling.URL); err != nil || u.Scheme == "" || u.Host == "" {
		add("signaling.url: must be an absolute ws:// or wss:// URL")
	} else {
		switch u.Scheme {
		case "ws", "wss":
		default:
			add("signaling.url: scheme %q is not ws or wss", u.Scheme)
		}
		if c.Signaling.TLSSkipVerify && u.Scheme == "wss" && !isLoopbackHost(u.Hostname()) {
			add("signaling.tls_skip_verify: refused for non-loopback host %q", u.Hostname())
		}
		if u.User != nil {
			add("signaling.url: credentials in the URL are not allowed; pass tokens via the environment")
		}
	}
	if c.Signaling.TimeoutMS < 1000 || c.Signaling.TimeoutMS > 120000 {
		add("signaling.timeout_ms: must be between 1000 and 120000")
	}

	if c.Audio.SampleRate != DefaultSampleRate {
		add("audio.sample_rate: voxen normalises to %d Hz; got %d", DefaultSampleRate, c.Audio.SampleRate)
	}
	if c.Audio.FrameMS < MinFrameDurationMS || c.Audio.FrameMS > MaxFrameDurationMS {
		add("audio.frame_ms: must be between %d and %d", MinFrameDurationMS, MaxFrameDurationMS)
	}
	if c.Audio.Bitrate < MinBitrate || c.Audio.Bitrate > MaxBitrate {
		add("audio.bitrate_bps: must be between %d and %d", MinBitrate, MaxBitrate)
	}
	if c.Audio.EchoCancellation {
		add("audio.echo_cancellation: not implemented yet; declaring it true would claim echo cancellation voxen does not perform")
	}
	if c.Audio.NoiseSuppression {
		add("audio.noise_suppression: not implemented yet; declaring it true would claim suppression voxen does not perform")
	}
	if c.Audio.InputGainPercent < 0 || c.Audio.InputGainPercent > 200 {
		add("audio.input_gain_percent: must be between 0 and 200")
	}
	if c.Audio.OutputGainPercent < 0 || c.Audio.OutputGainPercent > 200 {
		add("audio.output_gain_percent: must be between 0 and 200")
	}
	if len(c.Audio.InputDevice) > 128 || len(c.Audio.OutputDevice) > 128 {
		add("audio: device names must be at most 128 characters")
	}

	if len(c.Network.STUNServers) > MaxSTUNServers {
		add("network.stun_servers: at most %d entries are supported", MaxSTUNServers)
	}
	for i, server := range c.Network.STUNServers {
		if err := validateSTUNURL(server); err != nil {
			add("network.stun_servers[%d]: %v", i, err)
		}
	}
	if c.Network.ICETimeoutMS < 3000 || c.Network.ICETimeoutMS > 120000 {
		add("network.ice_timeout_ms: must be between 3000 and 120000")
	}

	if c.Daemon.Socket != "" {
		if err := validateEndpoint(c.Daemon.Socket); err != nil {
			add("daemon.socket: %v", err)
		}
	}
	if c.Daemon.StartupTimeoutMS < 1000 || c.Daemon.StartupTimeoutMS > 60000 {
		add("daemon.startup_timeout_ms: must be between 1000 and 60000")
	}

	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		add("logging.level: must be debug, info, warn or error")
	}
	switch strings.ToLower(c.Logging.Format) {
	case "text", "json":
	default:
		add("logging.format: must be text or json")
	}

	if !c.Security.RequireVerifiedPeers {
		add("security.require_verified_peers: voxen does not support unauthenticated media; this must be true")
	}
	if c.Security.SessionTTLSeconds < 30 || c.Security.SessionTTLSeconds > 3600 {
		add("security.session_ttl_seconds: must be between 30 and 3600")
	}
	if c.Security.MaxClockSkewSeconds < 5 || c.Security.MaxClockSkewSeconds > 300 {
		add("security.max_clock_skew_seconds: must be between 5 and 300")
	}

	if len(problems) > 0 {
		return fmt.Errorf("config: invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// SignalingTimeout returns the configured dial / handshake timeout.
func (c Config) SignalingTimeout() time.Duration {
	return time.Duration(c.Signaling.TimeoutMS) * time.Millisecond
}

// ICEtimeout returns the ICE establishment timeout.
func (c Config) ICEtimeout() time.Duration {
	return time.Duration(c.Network.ICETimeoutMS) * time.Millisecond
}

// FrameSamples is the number of 48 kHz samples in one Opus frame.
func (c Config) FrameSamples() int {
	return DefaultSampleRate * c.Audio.FrameMS / 1000
}

// Endpoint resolves the IPC endpoint, preferring an explicit config value over
// the platform default.
func (c Config) Endpoint(p paths.Paths) string {
	if c.Daemon.Socket != "" {
		return c.Daemon.Socket
	}
	return p.SocketPath
}

// Marshal renders the canonical YAML representation, including a header that
// warns against editing security limits.
func (c Config) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# voxen configuration (schema version ")
	buf.WriteString(fmt.Sprint(CurrentVersion))
	buf.WriteString(")\n")
	buf.WriteString("# This file is private to your user account. It contains no key material.\n")
	buf.WriteString("# Media is only sent to peers whose identity you have verified.\n\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// Save writes the configuration atomically with 0600 permissions.
func Save(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	raw, err := c.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: create directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("config: create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("config: set permissions: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("config: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("config: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: replace %s: %w", path, err)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateSTUNURL checks a STUN URI. RFC 7064 STUN URIs are opaque
// ("stun:host:port"), so net/url reports no Host component; validate the opaque
// form explicitly rather than bending it through a hierarchical parser.
func validateSTUNURL(raw string) error {
	scheme, rest, ok := strings.Cut(raw, ":")
	if !ok {
		return fmt.Errorf("%q is not a stun: or stuns: URI", raw)
	}
	switch strings.ToLower(scheme) {
	case "stun", "stuns":
	default:
		return fmt.Errorf("%q uses scheme %q, want stun or stuns", raw, scheme)
	}
	if strings.Contains(rest, "@") {
		return fmt.Errorf("%q: credentials are not allowed in a STUN URI", raw)
	}
	hostport := rest
	if i := strings.IndexByte(hostport, '?'); i >= 0 {
		query := hostport[i+1:]
		hostport = hostport[:i]
		if query != "transport=udp" && query != "transport=tcp" {
			return fmt.Errorf("%q: unsupported STUN transport query %q", raw, query)
		}
	}
	if hostport == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host == "" || strings.ContainsAny(host, " /\\") {
		return fmt.Errorf("%q has an invalid host", raw)
	}
	return nil
}

func isWindowsPipe(s string) bool {
	return strings.HasPrefix(strings.ToLower(s), `\\.\pipe\`)
}

// validateEndpoint checks that daemon.socket matches the transport the current
// platform actually uses. Accepting a Unix socket path on Windows (or a pipe
// name on Linux) would defer the error to listen time, where it reads as an
// opaque transport failure.
func validateEndpoint(socket string) error {
	if runtime.GOOS == "windows" {
		if !isWindowsPipe(socket) {
			return fmt.Errorf("%q is not a named pipe path; on Windows use \\\\.\\pipe\\<name>", socket)
		}
		return nil
	}
	if isWindowsPipe(socket) {
		return fmt.Errorf("%q is a Windows named pipe; on %s set an absolute filesystem path", socket, runtime.GOOS)
	}
	if !filepath.IsAbs(socket) {
		return fmt.Errorf("%q must be an absolute path", socket)
	}
	return nil
}
