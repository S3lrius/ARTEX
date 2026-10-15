package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// Container tools: a user supplies a Dockerfile in the UI, the server builds it into
// an artex-tool/<key> image on the configured Docker daemon (a rootless daemon in the
// sandbox, reached via DOCKER_HOST), and the agent can then invoke that image as a
// tool. Building is a HUMAN-ONLY operation: pgBuildContainerTool is a plain HTTP
// handler, never a CoreTool, so the agent cannot reach it, and create_custom_tool
// (the agent's platform tool) does not accept kind=container. The agent can only
// *call* container tools a human built, and each run is locked down below.

const toolImagePrefix = "artex-tool/"

// containerExec is the stored exec spec for a kind=container tool.
type containerExec struct {
	Image     string `json:"image"`      // must start with artex-tool/ (built by us)
	Args      string `json:"args"`       // arg template with {param} placeholders
	Network   string `json:"network"`    // docker network; empty = daemon default
	CPUs      string `json:"cpus"`       // e.g. "1.0"; empty = default
	Memory    string `json:"memory"`     // e.g. "512m"; empty = default
	PidsLimit int    `json:"pids_limit"` // <=0 = 256
	TimeoutMs int    `json:"timeout_ms"`
}

// runContainerTool runs one invocation as `docker run` with a locked-down profile,
// reusing the Bash runner so it inherits the timeout, proxy env and output overflow
// handling. Only images this server built (artex-tool/ prefix) may run.
func (s *Server) runContainerTool(ctx context.Context, execRaw json.RawMessage, params map[string]any, tc *actool.ToolContext) (actool.Result, error) {
	var spec containerExec
	_ = json.Unmarshal(execRaw, &spec)
	if !strings.HasPrefix(spec.Image, toolImagePrefix) {
		return actool.Errorf(fmt.Sprintf("refusing to run image %q: container tools may only run images built by this server (%s*)", spec.Image, toolImagePrefix)), nil
	}
	pids := spec.PidsLimit
	if pids <= 0 {
		pids = 256
	}
	// The rootless daemon is Podman (behind a Docker-compatible socket). Podman's compat
	// API rejects the attach/hijack an attached `docker run` needs ("unable to upgrade to
	// tcp, received 409"), so run detached and collect the result: start the container,
	// wait for it to exit, print its combined logs, remove it, and exit with its code -
	// the same observable output as an attached run, but portable across daemons.
	var b strings.Builder
	b.WriteString("cid=$(docker run -d --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges")
	fmt.Fprintf(&b, " --pids-limit %d", pids)
	if spec.Network != "" {
		fmt.Fprintf(&b, " --network %s", shellQuote(spec.Network))
	}
	if spec.CPUs != "" {
		fmt.Fprintf(&b, " --cpus %s", shellQuote(spec.CPUs))
	}
	if spec.Memory != "" {
		fmt.Fprintf(&b, " --memory %s", shellQuote(spec.Memory))
	}
	fmt.Fprintf(&b, " %s", shellQuote(spec.Image))
	if strings.TrimSpace(spec.Args) != "" {
		b.WriteString(" ")
		b.WriteString(renderTemplate(spec.Args, params, shellQuote))
	}
	// Close the run subshell, then wait for exit, emit the logs, remove the container, and
	// surface its exit code. (If the context deadline kills this command first, the detached
	// container can outlive it; --pids-limit and the read-only/no-cap profile bound the blast
	// radius, and a leaked one is reaped by the next `docker rm`/daemon restart.)
	b.WriteString(`); test -n "$cid" || exit 1; code=$(docker wait "$cid" 2>/dev/null); docker logs "$cid" 2>&1; docker rm -f "$cid" >/dev/null 2>&1; exit "${code:-1}"`)
	bashIn, _ := json.Marshal(map[string]any{"command": b.String()})
	if spec.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeoutOr(spec.TimeoutMs, 300000))
		defer cancel()
	}
	return actool.NewBash().Call(ctx, bashIn, tc)
}

// ---------- build endpoint (human-only; streams the build log as SSE) ----------

type buildToolReq struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Dockerfile  string          `json:"dockerfile"`
	Args        string          `json:"args"` // run-time arg template
	Agents      []string        `json:"agents"`
	Network     string          `json:"network"`
	CPUs        string          `json:"cpus"`
	Memory      string          `json:"memory"`
	PidsLimit   int             `json:"pids_limit"`
	TimeoutMs   int             `json:"timeout_ms"`
}

// pgBuildContainerTool builds an image from a submitted Dockerfile and, on success,
// registers a kind=container tool pointing at it. The response is an SSE stream of
// build output so the UI can show progress; the final event is build-ok or build-err.
func (s *Server) pgBuildContainerTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req buildToolReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	req.Key = strings.TrimSpace(req.Key)
	if !reToolKey.MatchString(req.Key) {
		writeErr(w, 400, "key must start with a lowercase letter and contain only lowercase letters, digits, or underscores")
		return
	}
	if strings.TrimSpace(req.Dockerfile) == "" {
		writeErr(w, 400, "dockerfile is empty")
		return
	}
	if exist, _ := pg.GetTool(req.Key); exist != nil {
		writeErr(w, 409, "a tool with this key already exists")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	send := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, strings.ReplaceAll(data, "\n", "\\n"))
		flusher.Flush()
	}

	// Write the Dockerfile to an isolated build context under the data dir.
	sum := sha256.Sum256([]byte(req.Dockerfile))
	tag := toolImagePrefix + req.Key + ":" + hex.EncodeToString(sum[:])[:12]
	ctxDir := filepath.Join(s.m.dir, ".toolbuild", req.Key)
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		send("build-err", err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(req.Dockerfile), 0o644); err != nil {
		send("build-err", err.Error())
		return
	}

	// docker build, stderr merged into stdout via sh -c so one pipe carries the log.
	buildCtx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	shCmd := "docker build -t " + shellQuote(tag)
	if req.Network != "" {
		shCmd += " --network " + shellQuote(req.Network)
	}
	shCmd += " " + shellQuote(ctxDir) + " 2>&1"
	cmd := exec.CommandContext(buildCtx, "sh", "-c", shCmd)
	cmd.Env = os.Environ() // honours DOCKER_HOST (rootless tool daemon) if set
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		send("build-err", err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		send("build-err", err.Error())
		return
	}
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		send("build-log", sc.Text())
	}
	if err := cmd.Wait(); err != nil {
		send("build-err", "docker build failed: "+err.Error())
		return
	}

	cspec := containerExec{
		Image: tag, Args: req.Args, Network: req.Network,
		CPUs: req.CPUs, Memory: req.Memory, PidsLimit: req.PidsLimit, TimeoutMs: req.TimeoutMs,
	}
	execRaw, _ := json.Marshal(cspec)
	agents := req.Agents
	if len(agents) == 0 {
		agents = []string{"worker"}
	}
	if err := pg.CreateCustomTool(&db.Tool{
		Key: req.Key, Description: req.Description, Schema: req.Schema,
		Agents: agents, Enabled: true, Kind: "container", Exec: execRaw,
	}); err != nil {
		send("build-err", "image built but registering the tool failed: "+err.Error())
		return
	}
	send("build-ok", tag)
}
