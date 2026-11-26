package api

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
)

// Mode is how the API listens: [ModeOff], [ModeUnix] or [ModeLoopback].
type Mode string

const (
	ModeOff      Mode = "off"
	ModeUnix     Mode = "unix"
	ModeLoopback Mode = "loopback"
)

// DefaultLoopbackAddr is the --api=loopback address when --api-listen is
// not given. The port is fixed so bookmarks keep working.
const DefaultLoopbackAddr = "127.0.0.1:7078"

// File names under the workingman directory (~/.workingman).
const (
	SocketFile = "api.sock"
	TokenDir   = "api"
	TokenFile  = "token"
	// InfoFile records the address a loopback listener on port 0 picked.
	InfoFile = "api.json"
)

// SocketPath is the default unix socket for a workingman directory.
func SocketPath(dir string) string { return filepath.Join(dir, SocketFile) }

// TokenPath is the token file for a workingman directory.
func TokenPath(dir string) string { return filepath.Join(dir, TokenDir, TokenFile) }

// InfoPath is where a random-port loopback listener records its address.
func InfoPath(dir string) string { return filepath.Join(dir, InfoFile) }

// Config is the listener configuration the daemon is given.
type Config struct {
	Mode Mode
	// Listen is the socket path (unix) or host:port (loopback).
	Listen string
	// TokenPath is the bearer token file, created when missing.
	TokenPath string
	// InfoPath is where the chosen address is written when a loopback
	// listener asks for port 0. Empty disables it.
	InfoPath string
}

// Enabled reports whether the configuration asks for a listener.
func (c Config) Enabled() bool { return c.Mode == ModeUnix || c.Mode == ModeLoopback }

// ParseConfig turns the --api and --api-listen flag values into a Config,
// filling in the defaults under dir (~/.workingman). It refuses an unknown
// mode, an --api-listen without a listener, and a TCP address that is not
// loopback.
func ParseConfig(mode, listen, dir string) (Config, error) {
	m := Mode(strings.TrimSpace(mode))
	if m == "" {
		m = ModeOff
	}
	listen = strings.TrimSpace(listen)
	cfg := Config{Mode: m, TokenPath: TokenPath(dir)}
	switch m {
	case ModeOff:
		if listen != "" {
			return Config{}, fmt.Errorf("--api-listen %q needs --api=unix or --api=loopback", listen)
		}
		return Config{Mode: ModeOff}, nil
	case ModeUnix:
		if listen == "" {
			listen = SocketPath(dir)
		}
		abs, err := filepath.Abs(listen)
		if err != nil {
			return Config{}, fmt.Errorf("--api-listen %q: %w", listen, err)
		}
		cfg.Listen = abs
	case ModeLoopback:
		if listen == "" {
			listen = DefaultLoopbackAddr
		}
		if err := checkLoopback(listen); err != nil {
			return Config{}, err
		}
		cfg.Listen = listen
		cfg.InfoPath = InfoPath(dir)
	default:
		return Config{}, fmt.Errorf("unknown --api value %q (expected unix|loopback|off)", mode)
	}
	return cfg, nil
}

// checkLoopback accepts host:port only when host is a loopback IP or
// "localhost". An empty host (":7078") would bind every interface.
func checkLoopback(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--api-listen %q: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("--api-listen %q: missing port", addr)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("--api-listen %q: the API only listens on loopback (127.0.0.1, [::1] or localhost)", addr)
}
