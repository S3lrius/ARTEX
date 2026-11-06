package db

import (
	"database/sql"
	"encoding/json"
	"time"
)

// Identity is an operator-supplied authenticated test session. Headers (the real
// Cookie/Authorization) is a SECRET: it is loaded only by IdentitiesForProxy, for
// the recording proxy, and is never returned by the list/API path.
type Identity struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	CompanyID *int64            `json:"company_id,omitempty"`
	Hosts     []string          `json:"hosts"`
	Headers   map[string]string `json:"-"` // secret; never serialized to the UI
	Canaries  []string          `json:"canaries"`
	CreatedAt time.Time         `json:"created_at"`
}

func nullableID(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func idPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// CreateIdentity inserts an authenticated identity. headers holds the real session
// and is stored but never echoed back on read.
func (d *DB) CreateIdentity(name string, companyID *int64, hosts []string, headers map[string]string, canaries []string) (*Identity, error) {
	hostsJSON, _ := json.Marshal(hosts)
	canJSON, _ := json.Marshal(canaries)
	hdrJSON, _ := json.Marshal(headers)
	var it Identity
	var cid sql.NullInt64
	if err := d.QueryRow(`
INSERT INTO identities(name, company_id, hosts, headers, canaries)
VALUES($1,$2,$3,$4,$5) RETURNING id, name, company_id, created_at`,
		name, nullableID(companyID), string(hostsJSON), string(hdrJSON), string(canJSON)).
		Scan(&it.ID, &it.Name, &cid, &it.CreatedAt); err != nil {
		return nil, err
	}
	it.CompanyID = idPtr(cid)
	it.Hosts = hosts
	it.Canaries = canaries
	return &it, nil // Headers deliberately not returned
}

// ListIdentities returns identities WITHOUT their secret headers (for the UI/API).
func (d *DB) ListIdentities() ([]Identity, error) {
	rows, err := d.Query(`SELECT id, name, company_id, hosts, canaries, created_at FROM identities ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var it Identity
		var cid sql.NullInt64
		var hostsB, canB []byte
		if err := rows.Scan(&it.ID, &it.Name, &cid, &hostsB, &canB, &it.CreatedAt); err != nil {
			return nil, err
		}
		it.CompanyID = idPtr(cid)
		_ = json.Unmarshal(hostsB, &it.Hosts)
		_ = json.Unmarshal(canB, &it.Canaries)
		out = append(out, it)
	}
	return out, rows.Err()
}

// DeleteIdentity removes an identity by id.
func (d *DB) DeleteIdentity(id int64) error {
	_, err := d.Exec(`DELETE FROM identities WHERE id=$1`, id)
	return err
}

// IdentitiesForProxy returns every identity INCLUDING its secret headers, for
// pushing into the recording proxy's injection set. Never exposed over the API.
func (d *DB) IdentitiesForProxy() ([]Identity, error) {
	rows, err := d.Query(`SELECT id, name, company_id, hosts, headers, canaries, created_at FROM identities ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var it Identity
		var cid sql.NullInt64
		var hostsB, canB []byte
		var hdr string
		if err := rows.Scan(&it.ID, &it.Name, &cid, &hostsB, &hdr, &canB, &it.CreatedAt); err != nil {
			return nil, err
		}
		it.CompanyID = idPtr(cid)
		_ = json.Unmarshal(hostsB, &it.Hosts)
		_ = json.Unmarshal(canB, &it.Canaries)
		if hdr != "" {
			_ = json.Unmarshal([]byte(hdr), &it.Headers)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
