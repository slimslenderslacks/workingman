package main

import (
	"os"
	"path/filepath"
	"strings"
)

// kitRegisterMarker appears in an acp-kit spec.yaml whose install step writes a
// claude-acp-client that registers MCP_URL as okta-gateway and removes the
// stock sbx mcp-gateway server (it also introduced register-only mode). A kit
// without it leaves every sandbox it creates on mcp-gateway.
const kitRegisterMarker = "CLAUDE_ACP_REGISTER_ONLY"

// resolveAcpKit makes a local --acp-kit directory absolute, so the kit the
// daemon hands every acp-wrapper does not depend on the cwd orch was started
// from, and returns warnings about a kit that would leave sandboxes with the
// stock mcp-gateway instead of okta-gateway: a missing checkout, a kit older
// than the okta-gateway entrypoint, or an image tag that does not match the
// kit's IMAGE_VERSION. A value that is not a local path (a published ref) is
// returned unchanged with no checks.
func resolveAcpKit(kit, image string) (string, []string) {
	kit = strings.TrimSpace(kit)
	if kit == "" {
		return "", nil
	}
	looksLocal := filepath.IsAbs(kit) || strings.HasPrefix(kit, ".") || strings.HasPrefix(kit, "~")
	info, err := os.Stat(kit)
	if err != nil || !info.IsDir() {
		if looksLocal {
			return kit, []string{"acp-kit " + kit + " is not a directory; sandbox creation will fail"}
		}
		return kit, nil // a published OCI/git ref
	}
	if abs, err := filepath.Abs(kit); err == nil {
		kit = abs
	}
	var warns []string
	spec, err := os.ReadFile(filepath.Join(kit, "spec.yaml"))
	switch {
	case err != nil:
		warns = append(warns, "acp-kit "+kit+" has no spec.yaml; is it an acp-kit checkout?")
	case !strings.Contains(string(spec), kitRegisterMarker):
		warns = append(warns, "acp-kit "+kit+" is stale: its claude-acp-client does not register okta-gateway or remove mcp-gateway, so every sandbox it creates keeps the stock mcp-gateway MCP server; pull the kit or point ACP_KIT/--acp-kit at a current checkout")
	}
	if v, err := os.ReadFile(filepath.Join(kit, "IMAGE_VERSION")); err == nil {
		want := strings.TrimSpace(string(v))
		img := strings.TrimSpace(image)
		if want != "" && img != "" && imageTag(img) != want {
			warns = append(warns, "acp-image "+img+" does not match acp-kit "+kit+" IMAGE_VERSION "+want+"; the kit and image are out of step")
		}
	}
	return kit, warns
}

// imageTag is the tag of an image reference ("repo/name:tag" → "tag"), or ""
// when it has none.
func imageTag(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") {
		return ""
	}
	return ref[i+1:]
}
