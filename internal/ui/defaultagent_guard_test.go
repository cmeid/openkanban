package ui

import (
	"os"
	"strings"
	"testing"
)

// TestNoGlobalDefaultAgentInSpawnPath is a build-time guard for the
// project-pin model introduced in task/extend-openkanban-to-support-launching-m:
// which agent (and thus which Claude profile / CLAUDE_CONFIG_DIR) launches is
// chosen at the PROJECT level only, via Model.resolveSpawnAgent. There is
// intentionally NO global fallback — an unpinned project refuses to spawn. That
// is the guard against "accidentally launching the wrong claude as long as you
// stay in your projects".
//
// Every spawn-resolution site (spawnAgent, promoteAndSpawnUnattached) and the
// ticket form must therefore go through the resolver, never read the global
// config.Defaults.DefaultAgent. If a future edit reintroduces that read in
// model.go — a resolution fallback, a form seed, or a settings picker — the
// guard is silently defeated. This test fails if the literal reappears.
//
// (config.Defaults.DefaultAgent still exists as a struct field and is read by
// internal/app for the opencode-server autostart decision — out of model.go's
// scope and intentionally untouched. The literal is split below so this guard
// file doesn't trip itself.)
func TestNoGlobalDefaultAgentInSpawnPath(t *testing.T) {
	forbidden := "Defaults." + "DefaultAgent"

	data, err := os.ReadFile("model.go")
	if err != nil {
		t.Fatalf("read model.go: %v", err)
	}
	if strings.Contains(string(data), forbidden) {
		t.Errorf("forbidden literal %q found in internal/ui/model.go — agent "+
			"identity must resolve via resolveSpawnAgent (the per-project pin), "+
			"never the global default. An unpinned project must refuse to spawn, "+
			"not fall back. If a future feature legitimately needs this read, "+
			"weaken this guard with a comment naming the new invariant.", forbidden)
	}
}
