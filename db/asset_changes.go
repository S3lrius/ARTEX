package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// AssetChange is one detected diff in the asset store. Rows are written by the
// record_asset_change trigger (new/changed/reappeared) and the scheduler gone
// sweep (gone); see db/schema.sql and server/scheduler.go. It is the backing
// data for the "Changes" feed — the "is anything different?" view.
type AssetChange struct {
	ID         int64  `json:"id"`
	AssetID    *int64 `json:"asset_id"`
	CompanyID  *int64 `json:"company_id"`
	AssetType  string `json:"asset_type"`
	Label      string `json:"label"`
	ChangeType string `json:"change_type"` // new | changed | reappeared | gone
	Field      string `json:"field"`       // changed only: which attribute
	OldValue   string `json:"old_value"`
	NewValue   string `json:"new_value"`
	DetectedAt string `json:"detected_at"`
	Seen       bool   `json:"seen"`
}

// AssetChangeFilter narrows the feed. Zero-value fields are ignored.
type AssetChangeFilter struct {
	CompanyID  *int64
	ChangeType string // one of the change_type values; "" = any
	UnseenOnly bool
	Limit      int // 0 => default 500, capped at 2000
}

const assetChangeCols = `id, asset_id, company_id, asset_type, label, change_type,
	COALESCE(field,''), COALESCE(old_value,''), COALESCE(new_value,''),
	detected_at::text, seen`

func scanAssetChange(row interface{ Scan(...any) error }) (AssetChange, error) {
	var c AssetChange
	err := row.Scan(&c.ID, &c.AssetID, &c.CompanyID, &c.AssetType, &c.Label, &c.ChangeType,
		&c.Field, &c.OldValue, &c.NewValue, &c.DetectedAt, &c.Seen)
	return c, err
}

// ListAssetChanges returns the change feed newest-first, filtered.
func (d *DB) ListAssetChanges(f AssetChangeFilter) ([]*AssetChange, error) {
	var conds []string
	var args []any
	if f.CompanyID != nil {
		args = append(args, *f.CompanyID)
		conds = append(conds, fmt.Sprintf("company_id = $%d", len(args)))
	}
	if f.ChangeType != "" {
		args = append(args, f.ChangeType)
		conds = append(conds, fmt.Sprintf("change_type = $%d", len(args)))
	}
	if f.UnseenOnly {
		conds = append(conds, "seen = false")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 500
	}
	if limit > 2000 {
		limit = 2000
	}
	args = append(args, limit)
	rows, err := d.Query(`SELECT `+assetChangeCols+` FROM asset_changes`+where+
		fmt.Sprintf(" ORDER BY detected_at DESC, id DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AssetChange{}
	for rows.Next() {
		c, err := scanAssetChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// CountUnseenChanges returns the number of unseen changes (for the nav badge).
func (d *DB) CountUnseenChanges() (int, error) {
	var n int
	err := d.QueryRow(`SELECT count(*) FROM asset_changes WHERE seen = false`).Scan(&n)
	return n, err
}

// MarkChangeSeen marks one change seen; reports whether a row changed.
func (d *DB) MarkChangeSeen(id int64) (bool, error) {
	res, err := d.Exec(`UPDATE asset_changes SET seen = true WHERE id = $1 AND seen = false`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkAllChangesSeen clears the unseen flag across the feed, optionally scoped to
// one company. Returns the number of rows cleared.
func (d *DB) MarkAllChangesSeen(companyID *int64) (int64, error) {
	var res sql.Result
	var err error
	if companyID != nil {
		res, err = d.Exec(`UPDATE asset_changes SET seen = true WHERE seen = false AND company_id = $1`, *companyID)
	} else {
		res, err = d.Exec(`UPDATE asset_changes SET seen = true WHERE seen = false`)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SweepGoneAssets writes a 'gone' change for every asset whose last_seen is older
// than goneDays and that has not already been reported gone since it was last
// seen. Returns the number of new 'gone' rows. Idempotent: keyed off last_seen,
// so a reappeared-then-quiet asset becomes eligible to be marked gone again.
func (d *DB) SweepGoneAssets(goneDays int) (int64, error) {
	if goneDays <= 0 {
		goneDays = 7
	}
	res, err := d.Exec(`
INSERT INTO asset_changes(asset_id, company_id, asset_type, label, change_type)
SELECT a.id, a.company_id, a.type, asset_label(a), 'gone'
FROM assets a
WHERE a.last_seen < now() - make_interval(days => $1)
  AND NOT EXISTS (
    SELECT 1 FROM asset_changes c
    WHERE c.asset_id = a.id AND c.change_type = 'gone' AND c.detected_at > a.last_seen)`, goneDays)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
