package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// HTTP surface for one-click update. Download, verification, and the binary swap
// all live in the selfupdate package. This file only owns the auth boundary,
// concurrency exclusion, progress broadcast, and telling main that it is time to exit.
//
// This process does not restart itself. After the new version is staged it exits
// with selfupdate.ExitRestart, and the supervisor (start.sh / start.bat, or the
// Docker ENTRYPOINT) starts it again.

// restartCh closes when an upgrade is ready or a rollback has finished. main then exits with ExitRestart.
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested returns a channel that closes when this process should exit and let the supervisor start it again.
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState is what selfupdate.Bootstrap concluded for this start (upgrade succeeded,
// just rolled back, or the staged file was discarded). main injects it so
// /api/update/check can tell the client how the previous upgrade ended.
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState is called once by main at startup.
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache caches the latest-version lookup from GitHub.
//
// The "update available" hint in the top bar checks on every full page load, and
// the unauthenticated GitHub API allows 60 requests per IP per hour. Without a
// cache, a few extra tabs or refreshes exhaust the quota, and a real update check
// then fails. An explicit "check for updates" can pass force and skip the cache.
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch loads the release. It exists so tests can inject it; nil uses the real GitHub lookup.
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// Failed lookups are cached briefly too. Otherwise every page load waits out a
	// timeout while GitHub is unreachable. The TTL stays short so recovery is quick.
	releaseErrTTL = 2 * time.Minute
	// Timeout for the version lookup. NewClient's 30 minute timeout is for downloading the whole package, which is far too long for a version check.
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get returns the latest Release and does not touch the network on a cache hit.
//
// The lock is held for the whole fetch. Concurrent requests queue for that one
// result instead of each calling GitHub (several tabs loading at once is exactly
// when rate limiting is most likely).
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// A cancelled request (the user closed the tab) does not mean GitHub failed.
	// Do not cache it, or the next visitor would get a confusing "cancelled" error.
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress is one progress event pushed to the client.
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // meaningful only during download; otherwise -1
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub holds one upgrade's progress and broadcasts it to SSE subscribers.
//
// running is also the mutex: another POST /api/update/apply during an upgrade
// returns 409, so two goroutines never write the same artex.new.
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin claims the upgrade. It returns false when one is already running.
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "Preparing...", Version: version}
	h.fanout(h.cur)
	return true
}

// finish ends one upgrade. A nil err means staging succeeded and a restart is pending.
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "Update failed", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "The new version is ready and restarting...", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout must be called while holding h.mu. Subscriber channels are buffered and
// drop when full. Progress is disposable, and a stuck SSE connection must not block the upgrade.
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck looks up the latest release on GitHub and compares it with the current version.
//
// The client can also call api.github.com directly (GitHub's CORS is *), but this
// endpoint is the one that counts: the download happens on the server, and an update
// is only possible when the server can reach GitHub. A browser that can connect while
// the server cannot is common (the server is on a private network, or the proxy is
// configured only in the browser). Updating would then fail, so the check should say so.
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// "Check for updates" passes force=1 to skip the cache. The app does not call this on its own.
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// A development build (dev, or git describe with a suffix) has no comparable
		// version. Allowing the update would overwrite the binary under debug with a
		// release build, so one-click update is refused.
		out["reason"] = fmt.Sprintf("current version %q is not a release build, so one-click update is disabled", current)
	}
	writeJSON(w, 200, out)
}

// updateApply downloads and stages the new version, then exits so the supervisor can restart.
//
// It returns 202 immediately and does the work on a background goroutine. Downloading
// the whole package can take minutes, and hanging that on the request would be cut off
// by a reverse-proxy timeout. Progress goes through /api/update/stream.
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion

	// Use the cache so the installed build is the version the user saw and confirmed.
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("current version %q is not a release build, so one-click update is disabled", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("already on the latest version %s", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "an update is already in progress")
		return
	}

	go func() {
		// Use s.ctx on purpose, not the request context. The request ends as soon as the
		// HTTP response returns, and a download bound to it would be cancelled immediately.
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] update failed: %v", err)
			return
		}
		log.Printf("[update] %s -> %s staged; exiting to finish the swap", current, rel.TagName)
		// Leave a moment for the last progress event to reach the client, then exit.
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback switches back to the previous version (the artex.old backup taken before the swap).
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "an update is in progress, so rollback is unavailable")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] rolled back to the previous version; exiting to finish the switch")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream pushes update progress over SSE.
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// Send the current state first so a refreshed page immediately sees an upgrade in progress.
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
