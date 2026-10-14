package tool

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Ripgrep integration: the Grep tool uses an `rg` binary already on PATH
// (faster, .gitignore-aware, full ripgrep syntax) and otherwise the pure-Go
// implementation. It does not download or install anything.
//
// Env controls:
//   - NORMA_DISABLE_RIPGREP=1  force the pure-Go implementation (also used by tests)

var (
	rgOnce     sync.Once
	rgResolved string
)

// RipgrepPath returns the ripgrep binary on PATH, or "" when the pure-Go
// grep should be used. The lookup runs at most once per process.
func RipgrepPath() string {
	if os.Getenv("NORMA_DISABLE_RIPGREP") != "" {
		return ""
	}
	rgOnce.Do(func() { rgResolved = resolveRipgrep() })
	return rgResolved
}

// EnsureRipgrep looks up ripgrep once at startup so the first Grep call does
// not pay for the PATH search. Returns the resolved path ("" = Go fallback).
func EnsureRipgrep() string { return RipgrepPath() }

func resolveRipgrep() string {
	if p, err := exec.LookPath("rg"); err == nil {
		return p
	}
	return ""
}

// ripgrepReq carries the parsed Grep parameters for the ripgrep backend.
type ripgrepReq struct {
	pattern, glob, typ, mode         string
	ignoreCase, multiline, showLines bool
	before, after                    int
	root                             string
	headLimit                        *int
	offset                           int
}

// runWithRipgrep runs the query through the rg binary and formats the result.
// Returns ok=false when rg failed for a real reason (not "no matches"), so the
// caller falls back to the Go implementation.
func runWithRipgrep(rg string, r ripgrepReq, tc *ToolContext) (Result, bool) {
	args := []string{"--color", "never"}
	if r.ignoreCase {
		args = append(args, "-i")
	}
	if r.multiline {
		args = append(args, "-U", "--multiline-dotall")
	}
	switch r.mode {
	case "files_with_matches":
		args = append(args, "-l")
	case "count":
		args = append(args, "-c")
	default: // content
		args = append(args, "--no-heading")
		if r.showLines {
			args = append(args, "-n")
		}
		if r.before > 0 {
			args = append(args, "-B", strconv.Itoa(r.before))
		}
		if r.after > 0 {
			args = append(args, "-A", strconv.Itoa(r.after))
		}
	}
	if r.glob != "" {
		args = append(args, "--glob", r.glob)
	}
	if r.typ != "" {
		if _, known := rgFileTypes[strings.ToLower(r.typ)]; known {
			args = append(args, "--type", r.typ)
		} else {
			args = append(args, "--glob", "*."+r.typ) // unknown type → extension glob
		}
	}
	// Run inside root so printed paths are root-relative (matching the Go impl).
	dir, target := r.root, "."
	if st, err := os.Stat(r.root); err == nil && !st.IsDir() {
		dir, target = filepath.Dir(r.root), filepath.Base(r.root)
	}
	// `-e pattern --` keeps a pattern/path starting with '-' from being read as a flag.
	args = append(args, "-e", r.pattern, "--", target)

	cmd := exec.Command(rg, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return Text("No matches found"), true // rg exit 1 = no matches
		}
		return Result{}, false // real failure → Go fallback
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return Text("No matches found"), true
	}
	lines := strings.Split(trimmed, "\n")
	headLimit := 250
	if r.headLimit != nil {
		headLimit = *r.headLimit
	}
	kept, more := applyHeadOffset(lines, headLimit, r.offset)
	res := strings.Join(kept, "\n")
	if len(kept) > 0 {
		res += "\n"
	}
	if more > 0 {
		res += fmt.Sprintf("... [%d more result(s) — page with offset=%d, or head_limit=0 for all] ...\n", more, r.offset+len(kept))
	}
	return Text(Capture(tc, res)), true
}
