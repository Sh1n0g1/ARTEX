package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa is the "model-driven context compaction" mechanism introduced in norma
// v0.4.0, toggled by the user in system settings as a platform experimental
// feature. It is mutually exclusive with the built-in compaction: noaadapter.Enable
// is the sole entry point, wiring up the context takeover (Compactor), the
// Compress tool, and the three resident prompt segments in one call; not calling
// Enable means it stays off (built-in compaction works as usual). The toggle is
// resolved via each agent's injected noaEnabledFn, read once per run, so flipping
// it only affects runs started afterward — no agent rebuild needed.

// enableNoa wires noa into opts when the resolver reports it's enabled. archiveRoot
// is the persistence base directory for compressed source text (taken from the
// global workDir; every agent lands uniformly under <workDir>/noa instead of being
// scattered per task/intent directory), and sessionID names its archive
// subdirectory underneath (globally unique, so no collisions within the same base
// directory).
//
// noa is an experimental feature: a failed wire-up must never interrupt a real
// task. On error it reports via onWarn and falls back to built-in compaction. On a
// successful enable, opts.Compaction is cleared to avoid agentcore warning about
// "two context managers set at once".
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa compaction failed to enable, falling back to built-in compaction: " + err.Error())
		}
		return
	}
	// Compactor supersedes Compaction, but agentcore warns every time if both are
	// set at once; clear it explicitly.
	opts.Compaction = nil
}
