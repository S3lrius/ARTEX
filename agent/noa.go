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

// noa is the "model-driven context compression" mechanism introduced in norma v0.4.0.
// Users turn it on as an experimental platform feature in system settings. It is
// mutually exclusive with built-in compaction: noaadapter.Enable is the only entry
// point and attaches the context takeover (Compactor), the Compress tool, and three
// resident prompts in one call. Not calling Enable leaves it off (built-in compaction
// keeps working). The switch is resolved by the noaEnabledFn each agent injects, read
// once per run, so a toggle affects only runs started afterward and does not require
// rebuilding the agent.

// enableNoa attaches noa to opts when the resolver reports it on. archiveRoot is the
// persistent base directory for compressed originals (the global workDir; every agent
// lands under <workDir>/noa and does not scatter across task/intent directories).
// sessionID names the archive subdirectory under it (globally unique, so one base
// directory has no collisions).
//
// noa is experimental: a failed attach must not interrupt a real task. On error,
// report through onWarn and fall back to built-in compaction. On success, clear
// opts.Compaction so agentcore does not warn that two context managers are set.
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
			onWarn("noa compression failed to enable, falling back to built-in compaction: " + err.Error())
		}
		return
	}
	// Compactor overrides Compaction, but agentcore warns on every run if both are set; clear it explicitly.
	opts.Compaction = nil
}
