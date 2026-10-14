// Package selfupdate implements ARTEX one-click update from the page: pull a new
// binary from a GitHub Release, verify it, stage it, and swap it in atomically
// on the next start.
//
// Split of work (see start.sh / start.bat):
//
//	start script = a simple supervisor loop that only decides, from the exit code, whether to launch again
//	this package = all the easy-to-get-wrong logic (download / SHA256 check / smoke test / swap / rollback on failure)
//
// The swap lives in Go rather than the script because a sha256 check and a smoke test
// would have to be written twice, once for sh and once for bat (sha256sum / shasum / certutil),
// and that is the step that must not go wrong. If a binary that cannot run is installed,
// the supervisor will faithfully keep launching it, and the user has to log in and recover by hand.
//
// One full upgrade takes three process starts:
//
//	1. The old server receives /api/update/apply → download and verify → stage artex.new → exit 75
//	2. The script relaunches the old build → Bootstrap finds artex.new → verify and smoke-test → swap → exit 75
//	3. The script relaunches, now on the new build → Bootstrap records one attempt → the marker is cleared after a successful start
//
// Any failed step falls back to the old build: if step 2 fails verification, the staged file is deleted and the old build keeps running; if step 3 crashes 3 times before the marker is cleared (it starts and then dies), artex.old is swapped back automatically.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the "please relaunch me" exit code for the supervisor (EX_TEMPFAIL).
// The start script reruns immediately when it sees it, and does not count it as a crash backoff.
// 0 means the user stopped normally (the script leaves the loop). Anything else is treated as a crash.
const ExitRestart = 75

// maxAttempts is how many starts are allowed after a swap. Each start of the new build
// increments the count, and surviving settleDelay clears the marker. Crashing maxAttempts
// times in a row means the new build cannot stay up, so it is rolled back automatically.
const maxAttempts = 3

// Paths is every file one upgrade touches. They all live in the directory that
// contains the executable. The working directory is deliberately not used: a service
// may run with CWD / or any other path, and a staged file dropped there would make
// the swap logic miss it.
type Paths struct {
	Dir     string // directory containing the executable
	Current string // binary that is running now        artex      / artex.exe
	New     string // staged new version                 artex.new  / artex.new.exe
	Sum     string // sha256 of the new version (hex)    artex.new.sha256 / artex.new.exe.sha256
	Old     string // previous version, backed up before the swap   artex.old  / artex.old.exe
	Marker  string // upgrade state marker               artex.upgrade.json
}

// ResolvePaths derives every upgrade path from the current executable.
//
// On Windows, .new and .old must also keep the .exe suffix, or the smoke test and
// the process after the swap both fail. The suffix is stripped and then reattached
// so the two platforms name files the same way.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // ".exe" on Windows, usually empty on Unix
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker records how far one swap has gotten, so a new build that cannot stay up triggers an automatic rollback.
type marker struct {
	From     string `json:"from"`     // version before the upgrade
	To       string `json:"to"`       // target version
	Attempts int    `json:"attempts"` // starts attempted since the swap
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged removes the staged files. A successful swap, a failed check, and a
// user cancel all go through it, so a leftover artex.new is not tried again on the next start.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two version numbers and returns -1/0/1 (a<b / a==b / a>b).
// ok=false means at least one side is not a comparable version (for example a local
// "dev" build, or "0.3.7-2-gabc1234-dirty" from git describe). The caller should
// disable one-click update in that case, or a development build would be "upgraded"
// to a release and uncommitted changes would be overwritten.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion parses a version of the form "v0.3.7" / "0.3.7" into [3]int.
//
// Only a clean three-part version is accepted. On a non-tag build, build.sh uses
// git describe and produces versions with a suffix such as "0.3.7-2-gabc1234".
// Those must be treated as not comparable, rather than as 0.3.7. Otherwise a
// development build would be mistaken for "already current" or overwritten by a release.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker reports whether the process is running in a container. A swap under Docker
// writes the container's writable layer, and `docker compose up -d` rebuilding the
// container returns to the version baked into the image. That is expected (the user
// is pulling a new image then), but the frontend needs to be able to say so.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
