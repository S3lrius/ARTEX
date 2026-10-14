package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset is the checksum manifest produced by release.yml. It covers every zip in the Release.
const sumsAsset = "SHA256SUMS"

// maxBinarySize caps the extracted binary so a malformed zip cannot fill the disk.
const maxBinarySize = 512 << 20 // 512 MiB

// Phase is a step in the upgrade, used directly as the phase field of an SSE event.
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress is provided by the caller to push progress to the frontend. pct is meaningful
// only during download (0-100). Other phases pass -1.
type Progress func(ph Phase, pct int, msg string)

// Stage downloads the current platform's archive from the given Release, verifies it,
// and stages the new binary as artex.new.
//
// It uses the full zip rather than a bare binary for two reasons. Existing Release
// SHA256SUMS already cover only the zip, so using the zip needs no CI change and still
// works with versions already published. The zip also carries skills/, which leaves a
// path for syncing built-in skills later. The cost is a few hundred extra KB of skills.
//
// When the function returns, staging is done. The caller then shuts down gracefully and exits with ExitRestart.
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("this version has no archive for %s/%s (missing %s)", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "Fetching the checksum manifest…")
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s does not list %s; refusing to install an unverified binary", sumsAsset, name)
	}

	// Temporary files stay in the target directory so the final rename is atomic on the
	// same filesystem (a cross-device rename fails, and /tmp is often its own mount).
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("Downloading %s (%s)…", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "Checking SHA256…")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 mismatch: expected %s, got %s (download corrupted or tampered)", short(want), short(got))
	}

	prog(PhaseExtract, -1, "Extracting and smoke-testing…")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("the new version cannot run on this system: %w", err)
	}

	// The staged file's own sha256 is stored separately and checked again before the
	// swap on the next start, in case the file is changed or corrupted between staging and restart.
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("hash the new binary: %w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("write checksum: %w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("stage new version: %w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// The marker only affects automatic rollback. The staged file is already in place, so the upgrade is not aborted for this.
		prog(PhaseStaged, -1, "Warning: failed to write the upgrade marker, so this upgrade has no automatic rollback")
	}

	prog(PhaseStaged, 100, "The new version is ready. Restarting…")
	return nil
}

// fetchSums downloads and parses SHA256SUMS, returning filename → hex digest.
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, fmt.Errorf("this Release has no %s, so integrity cannot be checked; refusing to upgrade", sumsAsset)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is empty or its format is not recognized", sumsAsset)
	}
	return out, nil
}

// parseSums parses a sha256sum-style manifest and returns filename → hex digest.
//
// The first field is kept only when it is 64 hex characters. "Exactly two fields"
// is not enough: any line of two words of prose would be treated as a real entry,
// junk would land in the digest table, and a real asset could match the wrong digest.
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// The form is "<sha256>  <filename>" (sha256sum uses two spaces; shasum's binary
		// mode prefixes the filename with *).
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download writes the asset to dst, hashes it with SHA256, and reports progress from Content-Length.
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.URL)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("create temporary file: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("download interrupted: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("failed to flush to disk: %w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("incomplete download: expected %d bytes, got %d bytes", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get issues a GET constrained by the host allowlist and returns the response body.
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary pulls the artex executable out of the release archive.
//
// The archive layout is artex-<version>-<os>-<arch>/artex, but this matches on the
// base name rather than building the full path. The version already appears in the
// archive name, and one mistyped character would fail the whole upgrade. Matching
// the base name survives that kind of change.
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open release archive: %w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("write new binary: %w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("extract %s: %w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("the executable in the release archive exceeds %s; refusing to extract", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("%s inside the release archive is empty", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("release archive does not contain %s", want)
}

// checkWritable confirms the directory is writable up front. Without it, a non-root
// run, or a binary sitting in a system directory, would fail only at the swap after
// tens of MB had already been downloaded.
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("program directory %s is not writable, so automatic update cannot run (check permissions or upgrade manually): %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter counts bytes written and reports on a limited cadence, so every 32KiB chunk does not push an SSE event.
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("Downloading %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
