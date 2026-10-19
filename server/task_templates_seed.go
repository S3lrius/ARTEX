package server

import (
	_ "embed"
	"encoding/json"
	"log"

	"github.com/Autumn-27/artex/db"
)

//go:embed task_templates_seed.json
var taskTemplatesSeedJSON []byte

// seededTaskTemplate mirrors one entry in task_templates_seed.json.
type seededTaskTemplate struct {
	Name           string                      `json:"name"`
	Description    string                      `json:"description"`
	Goal           string                      `json:"goal"`
	InterceptRules []db.TaskInterceptRuleInput `json:"intercept_rules"`
}

// seedTaskTemplates installs the built-in deep-dive task-template library ONCE
// (first-insert-only by name, like seedOrchestrationTools). Templates edited in
// the UI are preserved across restarts; a fresh install gets the full library.
// Source of truth: task_templates_seed.json (exported from a working instance).
func (s *Server) seedTaskTemplates() {
	pg := s.m.PG()
	if pg == nil {
		return
	}
	var tpls []seededTaskTemplate
	if err := json.Unmarshal(taskTemplatesSeedJSON, &tpls); err != nil {
		log.Printf("[seed] task templates: bad embedded JSON: %v", err)
		return
	}
	created := 0
	for _, t := range tpls {
		ok, err := pg.SeedTaskTemplate(db.TaskTemplateInput{
			Name:           t.Name,
			Description:    t.Description,
			Goal:           t.Goal,
			InterceptRules: t.InterceptRules,
		})
		if err != nil {
			log.Printf("[seed] task template %q: %v", t.Name, err)
			continue
		}
		if ok {
			created++
		}
	}
	if created > 0 {
		log.Printf("[seed] task templates: installed %d new (of %d)", created, len(tpls))
	}
}
