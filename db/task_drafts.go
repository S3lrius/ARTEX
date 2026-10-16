package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const MaxTaskDraftTextRunes = 16000

var ErrTaskDraftInvalid = errors.New("invalid task draft")

// TaskDraft is a recon-proposed deep dive awaiting human review. It never runs on
// its own: a human fires it, which opens a pre-seeded New Task. See db/schema.sql
// task_drafts and the suggest_task tool.
type TaskDraft struct {
	ID             int64                    `json:"id"`
	Name           string                   `json:"name"`
	TemplateNKey   string                   `json:"template_nkey"`
	CompanyID      *int64                   `json:"company_id"`
	Goal           string                   `json:"goal"`
	InterceptRules []TaskInterceptRuleInput `json:"intercept_rules"`
	SeedEndpoints  []string                 `json:"seed_endpoints"`
	Rationale      string                   `json:"rationale"`
	SourceTaskID   *int64                   `json:"source_task_id"`
	State          string                   `json:"state"`
	CreatedTaskID  *int64                   `json:"created_task_id"`
	CreatedAt      string                   `json:"created_at"`
	UpdatedAt      string                   `json:"updated_at"`
}

// TaskDraftInput is the create payload (from the suggest_task tool or the API).
type TaskDraftInput struct {
	Name           string
	TemplateNKey   string
	CompanyID      *int64
	Goal           string
	InterceptRules []TaskInterceptRuleInput
	SeedEndpoints  []string
	Rationale      string
	SourceTaskID   *int64
}

// to_jsonb() on the text[] column sidesteps driver array-scan quirks: both arrays
// come back as JSON bytes and unmarshal into Go slices the same way.
const taskDraftCols = `id, name, COALESCE(template_nkey,''), company_id, goal, intercept_rules,
	to_jsonb(seed_endpoints), rationale, source_task_id, state, created_task_id,
	created_at::text, updated_at::text`

func scanTaskDraft(row interface{ Scan(...any) error }) (TaskDraft, error) {
	var t TaskDraft
	var rulesRaw, endpointsRaw []byte
	if err := row.Scan(&t.ID, &t.Name, &t.TemplateNKey, &t.CompanyID, &t.Goal, &rulesRaw,
		&endpointsRaw, &t.Rationale, &t.SourceTaskID, &t.State, &t.CreatedTaskID,
		&t.CreatedAt, &t.UpdatedAt); err != nil {
		return t, err
	}
	t.InterceptRules = []TaskInterceptRuleInput{}
	if len(rulesRaw) > 0 {
		if err := json.Unmarshal(rulesRaw, &t.InterceptRules); err != nil {
			return t, err
		}
		if t.InterceptRules == nil {
			t.InterceptRules = []TaskInterceptRuleInput{}
		}
	}
	t.SeedEndpoints = []string{}
	if len(endpointsRaw) > 0 {
		if err := json.Unmarshal(endpointsRaw, &t.SeedEndpoints); err != nil {
			return t, err
		}
		if t.SeedEndpoints == nil {
			t.SeedEndpoints = []string{}
		}
	}
	return t, nil
}

func normalizeTaskDraftInput(in TaskDraftInput) (TaskDraftInput, error) {
	in.Name = strings.Join(strings.Fields(in.Name), " ")
	in.TemplateNKey = strings.TrimSpace(in.TemplateNKey)
	in.Goal = strings.TrimSpace(in.Goal)
	in.Rationale = strings.TrimSpace(in.Rationale)
	switch {
	case in.Name == "":
		return in, fmt.Errorf("%w: name is required", ErrTaskDraftInvalid)
	case utf8.RuneCountInString(in.Name) > MaxTaskTemplateNameRunes:
		return in, fmt.Errorf("%w: name exceeds %d characters", ErrTaskDraftInvalid, MaxTaskTemplateNameRunes)
	case utf8.RuneCountInString(in.Goal) > MaxTaskDraftTextRunes:
		return in, fmt.Errorf("%w: goal exceeds %d characters", ErrTaskDraftInvalid, MaxTaskDraftTextRunes)
	case utf8.RuneCountInString(in.Rationale) > MaxTaskDraftTextRunes:
		return in, fmt.Errorf("%w: rationale exceeds %d characters", ErrTaskDraftInvalid, MaxTaskDraftTextRunes)
	}
	if in.InterceptRules == nil {
		in.InterceptRules = []TaskInterceptRuleInput{}
	}
	if in.SeedEndpoints == nil {
		in.SeedEndpoints = []string{}
	}
	return in, nil
}

// CreateTaskDraft inserts one proposed deep dive. A partial unique index keeps at
// most one open draft per (name, source_task_id); on that conflict it returns the
// existing open draft instead of erroring, so an agent replay is idempotent.
func (d *DB) CreateTaskDraft(in TaskDraftInput) (*TaskDraft, error) {
	in, err := normalizeTaskDraftInput(in)
	if err != nil {
		return nil, err
	}
	rulesJSON, err := json.Marshal(in.InterceptRules)
	if err != nil {
		return nil, err
	}
	var templateNKey any
	if in.TemplateNKey != "" {
		templateNKey = in.TemplateNKey
	}
	t, err := scanTaskDraft(d.QueryRow(`
INSERT INTO task_drafts(name, template_nkey, company_id, goal, intercept_rules, seed_endpoints, rationale, source_task_id)
VALUES ($1,$2,$3,$4,$5::jsonb,$6::text[],$7,$8)
ON CONFLICT (name, source_task_id) WHERE state = 'open' DO NOTHING
RETURNING `+taskDraftCols,
		in.Name, templateNKey, in.CompanyID, in.Goal, string(rulesJSON),
		marshalStringArray(in.SeedEndpoints), in.Rationale, in.SourceTaskID))
	if err == sql.ErrNoRows {
		// An open draft with this (name, source_task_id) already exists — return it.
		return d.getOpenTaskDraftByName(in.Name, in.SourceTaskID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (d *DB) getOpenTaskDraftByName(name string, sourceTaskID *int64) (*TaskDraft, error) {
	t, err := scanTaskDraft(d.QueryRow(`SELECT `+taskDraftCols+`
FROM task_drafts WHERE state='open' AND name=$1 AND source_task_id IS NOT DISTINCT FROM $2`, name, sourceTaskID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListTaskDrafts returns drafts filtered by state (empty state = "open").
func (d *DB) ListTaskDrafts(state string) ([]*TaskDraft, error) {
	if state == "" {
		state = "open"
	}
	rows, err := d.Query(`SELECT `+taskDraftCols+` FROM task_drafts WHERE state=$1 ORDER BY created_at DESC, id DESC`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TaskDraft{}
	for rows.Next() {
		t, err := scanTaskDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// GetTaskDraft returns nil when id does not exist.
func (d *DB) GetTaskDraft(id int64) (*TaskDraft, error) {
	t, err := scanTaskDraft(d.QueryRow(`SELECT `+taskDraftCols+` FROM task_drafts WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// DismissTaskDraft marks an open draft dismissed and reports whether it changed.
func (d *DB) DismissTaskDraft(id int64) (bool, error) {
	result, err := d.Exec(`UPDATE task_drafts SET state='dismissed' WHERE id=$1 AND state='open'`, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// ConsumeTaskDraft marks an open draft consumed and records the task it became.
func (d *DB) ConsumeTaskDraft(id, createdTaskID int64) (bool, error) {
	result, err := d.Exec(`UPDATE task_drafts SET state='consumed', created_task_id=$2 WHERE id=$1 AND state='open'`, id, createdTaskID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}
