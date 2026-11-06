package server

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/traffic"
)

// reloadIdentities loads every identity (with its secret headers) and pushes them
// into the recording proxy's injection set. Call on startup and after any change.
// The raw credentials go ONLY to the proxy here — never to an agent or the UI.
func (s *Server) reloadIdentities() {
	tr := s.m.Traffic()
	if tr == nil {
		return // capture off → no proxy to inject into
	}
	rows, err := s.m.pg.IdentitiesForProxy()
	if err != nil {
		log.Printf("[identities] load for proxy failed: %v", err)
		return
	}
	ids := make([]traffic.Identity, 0, len(rows))
	for _, r := range rows {
		if len(r.Headers) == 0 || len(r.Hosts) == 0 {
			continue // nothing to inject, or nowhere to inject it
		}
		ids = append(ids, traffic.Identity{Name: r.Name, Hosts: r.Hosts, Headers: r.Headers})
	}
	tr.SetIdentities(ids)
}

func (s *Server) listIdentities(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	rows, err := pg.ListIdentities()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"identities": rows}) // headers never serialized (json:"-")
}

func (s *Server) createIdentity(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		Name      string            `json:"name"`
		CompanyID *int64            `json:"company_id"`
		Hosts     []string          `json:"hosts"`
		Headers   map[string]string `json:"headers"`
		Canaries  []string          `json:"canaries"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if strings.TrimSpace(body.Name) == "" || len(body.Hosts) == 0 || len(body.Headers) == 0 {
		writeErr(w, 400, "name, hosts and headers are required")
		return
	}
	it, err := pg.CreateIdentity(strings.TrimSpace(body.Name), body.CompanyID, body.Hosts, body.Headers, body.Canaries)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.reloadIdentities()
	writeJSON(w, 201, it)
}

func (s *Server) deleteIdentity(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid id")
		return
	}
	if err := pg.DeleteIdentity(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.reloadIdentities()
	writeJSON(w, 200, map[string]any{"deleted": id})
}
