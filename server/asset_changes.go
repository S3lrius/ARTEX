package server

import (
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/db"
)

// ---------- asset-changes feed API ("is anything different?") ----------
//
// Read-only detection lives entirely in the DB (record_asset_change trigger +
// the scheduler gone-sweep); these handlers just serve and acknowledge the feed.

func (s *Server) assetChangesList(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	q := r.URL.Query()
	f := db.AssetChangeFilter{ChangeType: q.Get("type")}
	if v := q.Get("company_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			f.CompanyID = &id
		}
	}
	if v := q.Get("unseen"); v == "1" || v == "true" {
		f.UnseenOnly = true
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	changes, err := pg.ListAssetChanges(f)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if changes == nil {
		changes = []*db.AssetChange{}
	}
	writeJSON(w, 200, map[string]any{"changes": changes})
}

func (s *Server) assetChangesUnseenCount(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	n, err := pg.CountUnseenChanges()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"count": n})
}

func (s *Server) assetChangeSeen(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad change id")
		return
	}
	done, err := pg.MarkChangeSeen(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"seen": done})
}

func (s *Server) assetChangesSeenAll(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var companyID *int64
	if v := r.URL.Query().Get("company_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			companyID = &id
		}
	}
	n, err := pg.MarkAllChangesSeen(companyID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"cleared": n})
}
