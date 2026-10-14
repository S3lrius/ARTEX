package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// Platform tools for the built-in Auto agent: create and edit skills, custom tools, and MCP.
// They are host tools, seeded into the tools table, bound to auto by default, and injected
// through hostTools. They reuse the existing db and filesystem logic.

func (s *Server) platformTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolCreateSkill(),
		s.toolUpdateSkillFile(),
		s.toolCreateCustomTool(),
		s.toolUpdateCustomTool(),
		s.toolCreateMCP(),
		s.toolUpdateMCP(),
		s.toolDeleteAssetsByHost(),
	}
}

// platformToolKeys are the tool keys the Auto agent gets bound by default.
var platformToolKeys = []string{
	"create_skill", "update_skill_file",
	"create_custom_tool", "update_custom_tool",
	"create_mcp", "update_mcp",
	"delete_assets_by_host",
}

// ---- assets ----

// toolDeleteAssetsByHost hard-deletes every asset tied to one host (exact match).
// Platform-level (not a per-task tool): operates on the global, cross-task asset library.
func (s *Server) toolDeleteAssetsByHost() actool.CoreTool {
	return wrTool("delete_assets_by_host",
		"Delete assets by exact host: remove that host's domains and subdomains, plus the services and endpoints under them.\n"+
			"The host must match exactly (lowercased, trimmed). This is not a fuzzy or wildcard match.\n"+
			"A root domain such as example.com also deletes its subdomains and their services and endpoints. A subdomain such as a.example.com, or an IP, deletes only that host and its services and endpoints.\n"+
			"Hard delete. It applies to the global asset library shared across tasks and cannot be undone.",
		objSchema(map[string]any{
			"host": strParam("Host to delete: a domain, subdomain, or IP. Exact match, such as example.com, a.example.com, or 1.2.3.4"),
		}, "host"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			as := s.assetStore()
			if as == nil {
				return actool.Errorf("asset library is not initialized"), nil
			}
			var a struct {
				Host string `json:"host"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Host) == "" {
				return actool.Errorf("host cannot be empty"), nil
			}
			counts, err := as.DeleteByHost(a.Host)
			if err != nil {
				return actool.Errorf("delete failed: " + err.Error()), nil
			}
			var total int64
			for _, n := range counts {
				total += n
			}
			return jsonResult(map[string]any{
				"host":            a.Host,
				"deleted":         total,
				"deleted_by_type": counts,
			})
		})
}

// ---- skills ----

func (s *Server) toolCreateSkill() actool.CoreTool {
	return wrTool("create_skill",
		"Create a new skill (writes SKILL.md, agentskills.io format). name is lowercase letters, digits, and hyphens.",
		objSchema(map[string]any{
			"name":         strParam("skill name (starts with a lowercase letter; letters, digits, and hyphens)"),
			"description":  strParam("skill description (required; what it does and when to use it)"),
			"instructions": strParam("Markdown body (optional)"),
		}, "name", "description"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Name, Description, Instructions string }
			_ = json.Unmarshal(in, &a)
			if !validSkillName(a.Name) {
				return actool.Errorf("invalid skill name (must start with a lowercase letter and contain only letters, digits, and hyphens, at most 64)"), nil
			}
			if strings.TrimSpace(a.Description) == "" {
				return actool.Errorf("description is required"), nil
			}
			path := filepath.Join(s.skillDir, a.Name)
			if _, err := os.Stat(path); err == nil {
				return actool.Errorf("skill already exists: " + a.Name), nil
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var b strings.Builder
			b.WriteString("---\n")
			fmt.Fprintf(&b, "name: %s\n", a.Name)
			fmt.Fprintf(&b, "description: %s\n", a.Description)
			b.WriteString("---\n")
			if strings.TrimSpace(a.Instructions) != "" {
				b.WriteString(a.Instructions)
			} else {
				fmt.Fprintf(&b, "## %s\n\n1. \n2. \n3. \n", a.Name)
			}
			if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(b.String()), 0o644); err != nil {
				_ = os.RemoveAll(path)
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("skill created: " + a.Name), nil
		})
}

func (s *Server) toolUpdateSkillFile() actool.CoreTool {
	return wrTool("update_skill_file",
		"Write or overwrite one file inside a skill (SKILL.md by default). Use it to change the skill or add a script or reference.",
		objSchema(map[string]any{
			"name":    strParam("skill name"),
			"file":    strParam("relative path (optional, SKILL.md by default, such as scripts/run.py)"),
			"content": strParam("full file contents"),
		}, "name", "content"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Name, File, Content string }
			_ = json.Unmarshal(in, &a)
			if !validSkillName(a.Name) {
				return actool.Errorf("invalid skill name"), nil
			}
			skillPath := filepath.Join(s.skillDir, a.Name)
			if _, err := os.Stat(skillPath); os.IsNotExist(err) {
				return actool.Errorf("skill does not exist: " + a.Name), nil
			}
			rel := strings.TrimSpace(a.File)
			if rel == "" {
				rel = "SKILL.md"
			}
			clean, msg := skillRelPath(rel)
			if msg != "" {
				return actool.Errorf("invalid path: " + msg), nil
			}
			full := filepath.Join(skillPath, clean)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("skill file written: " + a.Name + "/" + clean), nil
		})
}

// ---- custom tools ----

type customToolToolInput struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"`
	Exec        json.RawMessage `json:"exec"`
	Schema      json.RawMessage `json:"schema"`
	Agents      []string        `json:"agents"`
	Deferred    bool            `json:"deferred"`
	Enabled     *bool           `json:"enabled"`
}

func customToolSchema(keyDesc string) map[string]any {
	return objSchema(map[string]any{
		"key":         strParam(keyDesc),
		"description": strParam("description sent to the model"),
		"kind":        strParam("shell | command | script (Python only) | http. shell is a bash environment declaration (it only tells the model the tool can be called directly in bash, with no exec or schema). The other three require exec"),
		"exec":        map[string]any{"type": "object", "description": "execution spec (not needed for shell): command -> {command}; script -> {code}; http -> {method,url,headers,body,proxy,use_recording_proxy}"},
		"schema":      map[string]any{"type": "object", "description": "parameter JSON Schema (optional for shell, command, and script; required for http and must include properties)"},
		"agents":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "bound agent keys (optional)"},
		"deferred":    map[string]any{"type": "boolean", "description": "defer this tool (ignored for shell; turn on only for uncommon command, script, or http tools)"},
		"enabled":     map[string]any{"type": "boolean", "description": "enabled (default true)"},
	}, "key", "kind")
}

func toDBTool(a customToolToolInput) *db.Tool {
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	return &db.Tool{
		Key: a.Key, Description: a.Description, Schema: a.Schema, Agents: a.Agents,
		Enabled: enabled, Kind: a.Kind, Exec: a.Exec, Deferred: a.Deferred,
	}
}

func (s *Server) toolCreateCustomTool() actool.CoreTool {
	return wrTool("create_custom_tool", "Important: when you install a tool the platform does not have, call this to register it so the platform can invoke it. Creates a custom tool (shell, command, script, or http). shell is a bash environment declaration and needs only key, description, and agents, with no exec or schema.",
		customToolSchema("tool key (starts with a lowercase letter; letters, digits, and underscores)"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a customToolToolInput
			_ = json.Unmarshal(in, &a)
			a.Key = strings.TrimSpace(a.Key)
			if !reToolKey.MatchString(a.Key) {
				return actool.Errorf("key must start with a lowercase letter and contain only lowercase letters, digits, and underscores"), nil
			}
			if a.Kind != "shell" && a.Kind != "command" && a.Kind != "script" && a.Kind != "http" {
				return actool.Errorf("kind must be shell, command, script, or http"), nil
			}
			if a.Kind == "http" && !hasSchemaProps(a.Schema) {
				return actool.Errorf("an http tool must provide a parameter JSON Schema (it cannot be empty)"), nil
			}
			if exist, _ := s.m.pg.GetTool(a.Key); exist != nil {
				return actool.Errorf("that key already exists: " + a.Key), nil
			}
			if err := s.m.pg.CreateCustomTool(toDBTool(a)); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("custom tool created: " + a.Key), nil
		})
}

func (s *Server) toolUpdateCustomTool() actool.CoreTool {
	return wrTool("update_custom_tool", "Update an existing custom tool by key.",
		customToolSchema("custom tool key to update"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a customToolToolInput
			_ = json.Unmarshal(in, &a)
			existing, _ := s.m.pg.GetTool(a.Key)
			if existing == nil || existing.System {
				return actool.Errorf("only custom tools can be updated: " + a.Key), nil
			}
			if a.Kind != "shell" && a.Kind != "command" && a.Kind != "script" && a.Kind != "http" {
				return actool.Errorf("kind must be shell, command, script, or http"), nil
			}
			if a.Kind == "http" && !hasSchemaProps(a.Schema) {
				return actool.Errorf("an http tool must provide a parameter JSON Schema (it cannot be empty)"), nil
			}
			if err := s.m.pg.UpdateCustomTool(toDBTool(a)); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("custom tool updated: " + a.Key), nil
		})
}

// ---- MCP ----

type mcpToolInput struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Transport string          `json:"transport"`
	Command   string          `json:"command"`
	Args      json.RawMessage `json:"args"`
	Env       json.RawMessage `json:"env"`
	URL       string          `json:"url"`
	Enabled   *bool           `json:"enabled"`
	Insecure  *bool           `json:"insecure"`
}

func mcpSchema(withID bool) map[string]any {
	props := map[string]any{
		"name":      strParam("MCP server name"),
		"transport": strParam("stdio | http / sse"),
		"command":   strParam("stdio launch command (such as npx)"),
		"args":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "command argument array"},
		"env":       map[string]any{"type": "object", "description": "environment variables {KEY:VALUE}"},
		"url":       strParam("URL for http or sse"),
		"enabled":   map[string]any{"type": "boolean", "description": "enabled (default true)"},
		"insecure":  map[string]any{"type": "boolean", "description": "http: skip TLS certificate verification (set true for a self-signed certificate; default false)"},
	}
	required := []string{"name", "transport"}
	if withID {
		props["id"] = map[string]any{"type": "integer", "description": "MCP server id to update"}
		required = []string{"id", "name", "transport"}
	}
	return objSchema(props, required...)
}

func (a mcpToolInput) toDB() *db.MCPServer {
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	insecure := false
	if a.Insecure != nil {
		insecure = *a.Insecure
	}
	return &db.MCPServer{
		ID: a.ID, Name: a.Name, Transport: a.Transport, Command: a.Command,
		Args: a.Args, Env: a.Env, URL: a.URL, Enabled: enabled, Insecure: insecure,
	}
}

func (s *Server) toolCreateMCP() actool.CoreTool {
	return wrTool("create_mcp", "Create an MCP server (stdio, http, or sse). After creation, authorize its tools per agent.",
		mcpSchema(false),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a mcpToolInput
			_ = json.Unmarshal(in, &a)
			a.ID = 0
			if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Transport) == "" {
				return actool.Errorf("name and transport are required"), nil
			}
			id, err := s.m.pg.SaveMCP(a.toDB())
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("mcp created: id=%d name=%s", id, a.Name)), nil
		})
}

func (s *Server) toolUpdateMCP() actool.CoreTool {
	return wrTool("update_mcp", "Update an existing MCP server by id.",
		mcpSchema(true),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a mcpToolInput
			_ = json.Unmarshal(in, &a)
			if a.ID == 0 {
				return actool.Errorf("id is required"), nil
			}
			if _, err := s.m.pg.SaveMCP(a.toDB()); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("mcp updated: id=%d", a.ID)), nil
		})
}
