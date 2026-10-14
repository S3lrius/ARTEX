package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// GetSetting differs by only an error between "key missing" and "read failed": in
// both cases the value is an empty string. If a password handler treats that error
// as "password not set yet", a database blip opens the initialization path. authInit
// would let an unauthenticated request overwrite the existing admin password, and
// authStatus would send the client to /setup to do the same.
//
// These two tests close the handler's connection pool to force a read failure and
// assert that both paths fail closed.
func TestAuthStatusFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	// Close the pool so later GetSetting calls return an error rather than sql.ErrNoRows.
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	s.authStatus(w, httptest.NewRequest("GET", "/api/auth/status", nil))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (treating a read failure as uninitialized would send the user to /setup to overwrite the password); body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Initialized *bool `json:"initialized"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err == nil && payload.Initialized != nil {
		t.Fatalf("a read failure must not answer initialized, got %v", *payload.Initialized)
	}
}

func TestAuthInitFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"password":"correct horse battery"}`)
	s.authInit(w, httptest.NewRequest("POST", "/api/auth/init", body))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (allowing a read failure would let an unauthenticated request overwrite the existing password); body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("a read failure must not issue a token: %s", w.Body.String())
	}
}

func TestValidatePassword(t *testing.T) {
	for _, tc := range []struct {
		name, pw string
		wantErr  bool
	}{
		{"empty", "", true},
		{"seven characters", "1234567", true},
		{"eight characters", "12345678", false},
		{"eight non-ascii letters count as characters, not bytes", "éééééééé", false},
		{"three euro signs are 9 bytes but only 3 characters", "€€€", true},
		{"72 bytes", strings.Repeat("a", 72), false},
		{"73 bytes exceeds the bcrypt limit", strings.Repeat("a", 73), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validatePassword(tc.pw); (got != "") != tc.wantErr {
				t.Fatalf("validatePassword(%q)=%q, wantErr=%v", tc.pw, got, tc.wantErr)
			}
		})
	}
}
