package service

import (
	"bytes"
	"encoding/xml"
	"path/filepath"
)

// Label is the launchd job label and the plist's base name.
const Label = "com.workingman.orch"

// Config describes the background daemon.
type Config struct {
	// Binary is the absolute path to the orch executable.
	Binary string
	// Roots are the --root directories (absolute).
	Roots []string
	// ExtraArgs are further daemon flags, passed through verbatim.
	ExtraArgs []string
	// LogDir receives stdout/stderr (orch.out.log, orch.err.log).
	LogDir string
	// Path is the PATH for the job; launchd's default lacks Homebrew/Nix.
	Path string
	// RequireAC adds --require-ac (the service default).
	RequireAC bool
}

// Args is the daemon argv after the binary: headless, AC-gated, the roots,
// then the passthrough flags.
func (c Config) Args() []string {
	args := []string{"--headless"}
	if c.RequireAC {
		args = append(args, "--require-ac")
	}
	for _, r := range c.Roots {
		args = append(args, "--root", r)
	}
	return append(args, c.ExtraArgs...)
}

// PlistPath is where the LaunchAgent lives under home.
func PlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

// Plist renders the LaunchAgent property list.
func (c Config) Plist() []byte {
	var b bytes.Buffer
	str := func(indent, s string) {
		b.WriteString(indent + "<string>")
		_ = xml.EscapeText(&b, []byte(s))
		b.WriteString("</string>\n")
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
`)
	str("\t", Label)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	str("\t\t", c.Binary)
	for _, a := range c.Args() {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<true/>\n")
	b.WriteString("\t<key>ThrottleInterval</key>\n\t<integer>30</integer>\n")
	b.WriteString("\t<key>StandardOutPath</key>\n")
	str("\t", filepath.Join(c.LogDir, "orch.out.log"))
	b.WriteString("\t<key>StandardErrorPath</key>\n")
	str("\t", filepath.Join(c.LogDir, "orch.err.log"))
	if c.Path != "" {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>PATH</key>\n")
		str("\t\t", c.Path)
		b.WriteString("\t</dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}
