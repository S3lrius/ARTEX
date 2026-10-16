package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// suggestTaskDescription explains the tool to the model.
const suggestTaskDescription = "Propose a follow-up deep-dive task for a human to review and fire (it does NOT run it). Use this at the end of recon when you have identified a concrete, in-scope target worth a focused deep dive — instead of only narrating the conclusion in a fact. Fill name, the company, exact allow-rules (one per in-scope host), the documented endpoints to probe, and a one-line rationale. A human reviews the draft in the Suggested deep-dives inbox and clicks Fire to open a pre-filled New Task. Calling it again with the same name does not create a duplicate."

// suggestTaskSchema is the tool's JSON input schema.
func suggestTaskSchema() map[string]any {
	ruleItem := objSchema(map[string]any{
		"action":  map[string]any{"type": "string", "enum": []any{"allow", "block"}, "description": "allow (whitelist this host) or block"},
		"kind":    map[string]any{"type": "string", "description": "exact_domain | fuzzy_domain | exact_url | fuzzy_url | exact_ip | fuzzy_ip | cidr. Use exact_domain for a single in-scope host."},
		"pattern": strParam("the host/url/ip the rule matches, e.g. api.example.com"),
		"note":    strParam("optional note"),
		"enabled": map[string]any{"type": "boolean", "description": "default true"},
	}, "action", "kind", "pattern")
	return objSchema(map[string]any{
		"name":          strParam("short task name, e.g. 'Example - api BOLA deep dive'"),
		"template_nkey": strParam("optional lowercase task-template key to apply (e.g. 'deep dive — endpoint'); omit for none"),
		"company_id":    map[string]any{"type": "integer", "description": "the company whose scope this task targets"},
		"goal":          strParam("optional goal text; when omitted the applied template's goal is used"),
		"intercept_rules": map[string]any{
			"type": "array", "description": "exact allow-rules (one per in-scope host) that lock the deep dive's scope; add block rules for out-of-scope siblings",
			"items": ruleItem,
		},
		"seed_endpoints": map[string]any{
			"type": "array", "description": "documented endpoints to probe, e.g. '/v1/customers'",
			"items": map[string]any{"type": "string"},
		},
		"rationale": strParam("one line: why this deep dive is worth firing"),
	}, "name")
}

// toolSuggestTask is the recon-proposes-a-deep-dive host tool. It writes a
// task_drafts row (a proposal); a human fires it from the Suggested deep-dives
// inbox. Bound to the worker + planner (see seedOrchestrationTools).
func (s *Server) toolSuggestTask() actool.CoreTool {
	return wrTool("suggest_task", suggestTaskDescription, suggestTaskSchema(),
		func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var args struct {
				Name           string                      `json:"name"`
				TemplateNKey   string                      `json:"template_nkey"`
				CompanyID      json.RawMessage             `json:"company_id"`
				Goal           string                      `json:"goal"`
				InterceptRules []db.TaskInterceptRuleInput `json:"intercept_rules"`
				SeedEndpoints  []string                    `json:"seed_endpoints"`
				Rationale      string                      `json:"rationale"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			in := db.TaskDraftInput{
				Name:           args.Name,
				TemplateNKey:   args.TemplateNKey,
				Goal:           args.Goal,
				InterceptRules: args.InterceptRules,
				SeedEndpoints:  args.SeedEndpoints,
				Rationale:      args.Rationale,
			}
			if id := parseProfileID(args.CompanyID); id > 0 {
				in.CompanyID = &id
			}
			// Provenance: the recon task that produced this proposal rides in on ctx.
			if ri := agent.RunInfoFrom(ctx); ri.TaskID > 0 {
				tid := ri.TaskID
				in.SourceTaskID = &tid
			}
			draft, err := s.m.pg.CreateTaskDraft(in)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if draft == nil {
				return actool.Errorf("failed to record the draft"), nil
			}
			return jsonResult(map[string]any{"draft_id": draft.ID, "state": draft.State})
		})
}

// ---------- task-drafts REST API ----------

func (s *Server) taskDraftsList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	drafts, err := pg.ListTaskDrafts(r.URL.Query().Get("state"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if drafts == nil {
		drafts = []*db.TaskDraft{}
	}
	writeJSON(w, 200, map[string]any{"drafts": drafts})
}

func (s *Server) taskDraftCreate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Name           string                      `json:"name"`
		TemplateNKey   string                      `json:"template_nkey"`
		CompanyID      *int64                      `json:"company_id"`
		Goal           string                      `json:"goal"`
		InterceptRules []db.TaskInterceptRuleInput `json:"intercept_rules"`
		SeedEndpoints  []string                    `json:"seed_endpoints"`
		Rationale      string                      `json:"rationale"`
		SourceTaskID   *int64                      `json:"source_task_id"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	draft, err := pg.CreateTaskDraft(db.TaskDraftInput{
		Name:           req.Name,
		TemplateNKey:   req.TemplateNKey,
		CompanyID:      req.CompanyID,
		Goal:           req.Goal,
		InterceptRules: req.InterceptRules,
		SeedEndpoints:  req.SeedEndpoints,
		Rationale:      req.Rationale,
		SourceTaskID:   req.SourceTaskID,
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, draft)
}

func (s *Server) taskDraftDismiss(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad draft id")
		return
	}
	ok, err := pg.DismissTaskDraft(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"dismissed": ok})
}

func (s *Server) taskDraftConsume(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad draft id")
		return
	}
	var req struct {
		CreatedTaskID int64 `json:"created_task_id"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.CreatedTaskID <= 0 {
		writeErr(w, 400, "created_task_id is required")
		return
	}
	done, err := pg.ConsumeTaskDraft(id, req.CreatedTaskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"consumed": done})
}
