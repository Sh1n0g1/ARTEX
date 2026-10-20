// Package selfupdate implements ARTEX's one-click in-page update: pulling a
// new binary from a GitHub Release, verifying it, staging it, and swapping it
// in atomically on the next startup.
//
// Overall division of labor (see start.sh / start.bat):
//
//	startup script = a dumb supervisor loop, responsible only for "decide whether to relaunch based on the exit code after the process exits"
//	this package   = all the error-prone logic (download / SHA256 verification / smoke test / swap / rollback on failure)
//
// Swapping is done in Go rather than in the scripts because SHA256
// verification and the smoke test would need two separate implementations in
// sh and bat (sha256sum / shasum / certutil), and this is exactly the step
// that must not go wrong: swap in a binary that cannot run, and the
// supervisor will faithfully keep relaunching it forever, leaving the user to
// fix it by hand on the machine.
//
// One complete upgrade spans three process startups:
//
//	(1) the old server receives /api/update/apply -> downloads and verifies -> stages artex.new -> exit 75
//	(2) the script relaunches the old version -> Bootstrap finds artex.new -> verifies+smoke tests -> swaps it in -> exit 75
//	(3) the script relaunches, now running the new version -> Bootstrap counts an attempt -> clears the marker once startup succeeds
//
// A failure at any step falls back to the old version: at (2), if
// verification fails the staged build is deleted and the old version keeps
// running; at (3), if it fails to survive long enough to clear the marker for
// 3 consecutive attempts (crashing counts as not starting), artex.old is
// automatically swapped back in.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the exit code (EX_TEMPFAIL) meaning "please relaunch me,
// supervisor". The startup script reruns immediately on seeing it, without
// counting it toward crash backoff. 0 means the user stopped it normally (the
// script exits its loop); anything else is treated as a crash.
const ExitRestart = 75

// maxAttempts is how many startup attempts are allowed after a swap. Each
// startup of the new version increments the counter by 1; the marker is
// cleared once it survives settleDelay. Crashing maxAttempts times in a row
// means the new version simply cannot start, triggering an automatic
// rollback.
const maxAttempts = 3

// Paths lists every file involved in one upgrade, all kept in the
// **directory containing the executable**. Deliberately not CWD: when run as
// a service the working directory may be / or anything else, and using CWD
// would put the staged file somewhere else, breaking the swap logic outright.
type Paths struct {
	Dir     string // directory containing the executable
	Current string // the binary currently running        artex      / artex.exe
	New     string // the staged new version               artex.new  / artex.new.exe
	Sum     string // the new version's sha256 (hex)        artex.new.sha256 / artex.new.exe.sha256
	Old     string // the old version backed up before swapping   artex.old  / artex.old.exe
	Marker  string // the upgrade status marker             artex.upgrade.json
}

// ResolvePaths derives every upgrade path from the current executable.
//
// On Windows, .new/.old must also carry the .exe suffix, or both the smoke
// test and running the swapped-in binary would fail; the suffix is stripped
// and reattached so the naming is symmetric across platforms.
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

// marker records the progress of one swap, used to trigger an automatic
// rollback if the new version cannot start.
type marker struct {
	From     string `json:"from"`     // version before the upgrade
	To       string `json:"to"`       // target version
	Attempts int    `json:"attempts"` // number of startup attempts since the swap
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

// cleanStaged removes the staged build. Taken on a successful swap, a failed
// verification, or a user cancellation alike, so a leftover artex.new is
// never retried on the next startup.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two version strings, returning -1/0/1 (a<b /
// a==b / a>b). ok=false means at least one side is not a comparable version
// string (e.g. a local dev build's "dev", or git describe's
// "0.3.7-2-gabc1234-dirty"); callers should then disable one-click update, or
// a build in development would get "upgraded" to a release and overwrite
// uncommitted changes.
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

// parseVersion parses a version string of the form "v0.3.7" / "0.3.7" into a
// [3]int.
//
// Only accepts a clean three-part form: for non-tag builds, build.sh uses git
// describe to produce suffixed versions like "0.3.7-2-gabc1234", and these
// must be judged incomparable rather than treated as 0.3.7 -- otherwise a dev
// build could be mistaken for "already up to date" or overwritten by a
// release.
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

// InDocker reports whether the process is running in a container. Under
// Docker, a swap writes to the container's writable layer, so rebuilding the
// container with `docker compose up -d` reverts to the version baked into the
// image -- this is expected behavior (the user is pulling a new image at that
// point anyway), but the frontend needs this to explain it clearly.
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
