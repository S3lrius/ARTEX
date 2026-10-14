package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestExtractTokenAcceptsQueryTokenOnlyOnSSE(t *testing.T) {
	for _, tc := range []struct {
		method, target string
		want           string
	}{
		{"GET", "/api/logs/stream?token=abc", "abc"},
		{"GET", "/api/exploration/activity/stream?task=1&token=abc", "abc"},
		{"GET", "/api/side-questions/42/events?token=abc", "abc"},
		{"GET", "/api/tasks?token=abc", ""},
		{"GET", "/api/workspace/download?path=x&token=abc", ""},
		{"POST", "/api/logs/stream?token=abc", ""},
	} {
		r := httptest.NewRequest(tc.method, tc.target, nil)
		if got := extractToken(r); got != tc.want {
			t.Errorf("%s %s: extractToken=%q want %q", tc.method, tc.target, got, tc.want)
		}
	}
}

func TestRotateJWTKeyInvalidatesOldTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), jwtKeyFilename)
	s := &Server{jwtKey: []byte("original-signing-key-for-rotation"), jwtKeyPath: path}
	old, err := signJWT(s.signingKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.rotateJWTKey(); err != nil {
		t.Fatal(err)
	}
	if verifyJWT(old, s.signingKey()) {
		t.Fatal("a token signed before rotation must stop verifying")
	}
	fresh, err := signJWT(s.signingKey())
	if err != nil {
		t.Fatal(err)
	}
	if !verifyJWT(fresh, s.signingKey()) {
		t.Fatal("a token signed after rotation must verify")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(s.signingKey()) {
		t.Fatal("the rotated key must be persisted, or a restart would revive old tokens")
	}
}

func TestAuthInitRequiresSetupToken(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	if hash, _, err := m.pg.GetSetting(authPassKey); err != nil || hash != "" {
		t.Skip("test database already has a password set")
	}
	for _, tc := range []struct{ name, serverToken, body string }{
		{"missing token", "right-token", `{"password":"correct horse battery"}`},
		{"wrong token", "right-token", `{"password":"correct horse battery","setup_token":"wrong"}`},
		{"setup closed", "", `{"password":"correct horse battery","setup_token":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{m: m, setupToken: tc.serverToken, jwtKey: []byte("init-test-signing-key")}
			w := httptest.NewRecorder()
			s.authInit(w, httptest.NewRequest("POST", "/api/auth/init", strings.NewReader(tc.body)))
			if w.Code != 403 {
				t.Fatalf("status=%d want 403; body=%s", w.Code, w.Body.String())
			}
			if hash, _, _ := m.pg.GetSetting(authPassKey); hash != "" {
				t.Fatal("a rejected init must not set the password")
			}
		})
	}
}
