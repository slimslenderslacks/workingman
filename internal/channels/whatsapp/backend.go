package whatsapp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/slimslenderslacks/work/internal/channels"
)

// Backend selects which WhatsApp transport a channel uses.
type Backend string

const (
	// BackendCloud is the Meta WhatsApp Cloud API (webhook + Graph API). The default.
	BackendCloud Backend = "cloud"
	// BackendBridge is the Baileys bridge: a personal number linked by QR
	// code, no public webhook.
	BackendBridge Backend = "bridge"
)

// Option keys inside channels.ChannelConfig.Options.
const (
	// ModeOptionsKey selects the Backend: `mode: cloud | bridge`. (Not to be
	// confused with access.mode, which is bot vs self-chat.)
	ModeOptionsKey = "mode"
	// BridgeOptionsKey holds the BridgeOptions block.
	BridgeOptionsKey = "bridge"
)

// ParseBackend reads options.mode. Absent or empty means cloud.
func ParseBackend(options map[string]any) (Backend, error) {
	raw, ok := options[ModeOptionsKey]
	if !ok || raw == nil {
		return BackendCloud, nil
	}
	s, isStr := raw.(string)
	if !isStr {
		return "", fmt.Errorf("whatsapp: options.%s must be %q or %q", ModeOptionsKey, BackendCloud, BackendBridge)
	}
	switch b := Backend(strings.ToLower(strings.TrimSpace(s))); b {
	case "", BackendCloud:
		return BackendCloud, nil
	case BackendBridge:
		return BackendBridge, nil
	default:
		return "", fmt.Errorf("whatsapp: options.%s: %q is not one of %q, %q", ModeOptionsKey, s, BackendCloud, BackendBridge)
	}
}

// BridgeOptions is the `bridge:` block under a WhatsApp channel's options:
//
//	channels:
//	  whatsapp:
//	    type: whatsapp
//	    options:
//	      mode: bridge
//	      access:                    # same policy block as the cloud backend
//	        mode: self-chat
//	        self_ids: ["15551234567"]
//	      bridge:
//	        # node: /usr/local/bin/node        # default: node on PATH (>= 18)
//	        # port: 0                          # 0 = pick a free loopback port
//	        # session_dir: ~/.workingman/whatsapp/session
//	        # install_dir: ~/.workingman/whatsapp/bridge
//	        # send_read_receipts: false        # blue ticks for admitted messages
//
// The bridge only ever binds 127.0.0.1.
type BridgeOptions struct {
	Node             string `yaml:"node"`
	Port             int    `yaml:"port"`
	SessionDir       string `yaml:"session_dir"`
	InstallDir       string `yaml:"install_dir"`
	SendReadReceipts bool   `yaml:"send_read_receipts"`
}

// ParseBridgeOptions strictly decodes options.bridge (unknown keys are errors)
// and fills defaults, expanding a leading "~" in directories.
func ParseBridgeOptions(options map[string]any) (BridgeOptions, error) {
	var o BridgeOptions
	if raw, ok := options[BridgeOptionsKey]; ok && raw != nil {
		data, err := yaml.Marshal(raw)
		if err != nil {
			return o, fmt.Errorf("whatsapp: options.%s: %w", BridgeOptionsKey, err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&o); err != nil && !errors.Is(err, io.EOF) {
			return BridgeOptions{}, fmt.Errorf("whatsapp: options.%s: %w", BridgeOptionsKey, err)
		}
	}
	if o.Port < 0 || o.Port > 65535 {
		return BridgeOptions{}, fmt.Errorf("whatsapp: options.%s.port: %d is not a port", BridgeOptionsKey, o.Port)
	}
	var err error
	if o.SessionDir, err = dirOrDefault(o.SessionDir, DefaultSessionDir); err != nil {
		return BridgeOptions{}, err
	}
	if o.InstallDir, err = dirOrDefault(o.InstallDir, DefaultInstallDir); err != nil {
		return BridgeOptions{}, err
	}
	return o, nil
}

func dirOrDefault(dir string, def func() (string, error)) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return def()
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("whatsapp: resolve home dir: %w", err)
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	return dir, nil
}

func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("whatsapp: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".workingman", "whatsapp"), nil
}

// DefaultSessionDir is where the bridge keeps its linked-device credentials:
// ~/.workingman/whatsapp/session. Treat it like a password: anyone holding it
// can act as the account.
func DefaultSessionDir() (string, error) {
	d, err := stateDir()
	return filepath.Join(d, "session"), err
}

// DefaultInstallDir is where the embedded bridge is extracted and its npm
// dependencies installed: ~/.workingman/whatsapp/bridge.
func DefaultInstallDir() (string, error) {
	d, err := stateDir()
	return filepath.Join(d, "bridge"), err
}

// FactoryDeps are the daemon-supplied pieces the WhatsApp factory needs.
type FactoryDeps struct {
	// LogDir receives whatsapp-bridge.log. Pass the directory of the audit
	// log so the bridge's output lands beside it. Default: ~/.workingman/whatsapp.
	LogDir string
	Logger *slog.Logger
	// Cloud builds the Cloud API backend. Nil means this build has none, and
	// `mode: cloud` is reported as an error.
	Cloud channels.Factory
}

// Factory returns the channels.Factory for `type: whatsapp`: it reads
// options.mode and builds the bridge backend here, or defers to deps.Cloud.
func Factory(deps FactoryDeps) channels.Factory {
	return func(name string, spec channels.ChannelConfig, creds channels.Credentials) (channels.Channel, error) {
		backend, err := ParseBackend(spec.Options)
		if err != nil {
			return nil, err
		}
		switch backend {
		case BackendBridge:
			return newBridgeFromSpec(name, spec, deps)
		default:
			if deps.Cloud == nil {
				return nil, errors.New("whatsapp: the cloud backend is not available in this build; set options.mode: bridge")
			}
			return deps.Cloud(name, spec, creds)
		}
	}
}
