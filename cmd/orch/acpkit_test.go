package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveAcpKit(t *testing.T) {
	mk := func(t *testing.T, spec, version string) string {
		dir := filepath.Join(t.TempDir(), "acp-kit")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if spec != "" {
			os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec), 0o644)
		}
		if version != "" {
			os.WriteFile(filepath.Join(dir, "IMAGE_VERSION"), []byte(version+"\n"), 0o644)
		}
		return dir
	}
	const current = "install: |\n  if [ \"${CLAUDE_ACP_REGISTER_ONLY:-}\" = 1 ]; then exit 0; fi\n"
	const image = "slimslenderslacks/claude-code-acp:0.90.0"

	t.Run("current kit, matching image", func(t *testing.T) {
		kit := mk(t, current, "0.90.0")
		if got, warns := resolveAcpKit(kit, image); got != kit || len(warns) != 0 {
			t.Errorf("resolveAcpKit = %q, %v", got, warns)
		}
	})
	t.Run("relative path made absolute", func(t *testing.T) {
		kit := mk(t, current, "0.90.0")
		t.Chdir(filepath.Dir(kit))
		got, _ := resolveAcpKit("acp-kit", image)
		if !filepath.IsAbs(got) {
			t.Errorf("resolveAcpKit(acp-kit) = %q, want absolute", got)
		}
	})
	for _, tc := range []struct {
		name, spec, version, image, want string
	}{
		{name: "stale kit", spec: "install: old\n", version: "0.90.0", image: image, want: "stale"},
		{name: "image mismatch", spec: current, version: "0.91.0", image: image, want: "IMAGE_VERSION"},
		{name: "no spec", version: "0.90.0", image: image, want: "no spec.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, warns := resolveAcpKit(mk(t, tc.spec, tc.version), tc.image)
			if len(warns) != 1 || !strings.Contains(warns[0], tc.want) {
				t.Errorf("warnings = %v, want one mentioning %q", warns, tc.want)
			}
		})
	}
	t.Run("missing local dir", func(t *testing.T) {
		_, warns := resolveAcpKit(filepath.Join(t.TempDir(), "nope"), image)
		if len(warns) != 1 {
			t.Errorf("warnings = %v, want one", warns)
		}
	})
	t.Run("published ref untouched", func(t *testing.T) {
		if got, warns := resolveAcpKit("ghcr.io/x/acp-kit:1", image); got != "ghcr.io/x/acp-kit:1" || len(warns) != 0 {
			t.Errorf("resolveAcpKit = %q, %v", got, warns)
		}
	})
}

func TestImageTag(t *testing.T) {
	for in, want := range map[string]string{
		"slimslenderslacks/claude-code-acp:0.90.0": "0.90.0",
		"localhost:5000/acp":                       "",
		"localhost:5000/acp:1.2":                   "1.2",
		"acp@sha256:abc":                           "",
		"acp":                                      "",
	} {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}
