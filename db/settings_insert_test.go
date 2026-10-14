package db

import (
	"fmt"
	"testing"
	"time"
)

// InsertSettingIfAbsent is the backstop for auth.password_hash: the caller's
// GetSetting check can fail because the database errors, or a concurrent request
// can slip in (bcrypt takes tens of milliseconds), so the "set only once" guarantee
// must sit on the primary-key constraint rather than an if in the caller.
func TestInsertSettingIfAbsentDoesNotOverwrite(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	// Use a test-only key so the real auth.password_hash in the dev database is never touched.
	key := fmt.Sprintf("test.insert_if_absent.%d", time.Now().UnixNano())
	defer func() { _, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, key) }()

	inserted, err := d.InsertSettingIfAbsent(key, "first")
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("first insert should return inserted=true")
	}

	inserted, err = d.InsertSettingIfAbsent(key, "second")
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("an existing key should return inserted=false")
	}

	got, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		t.Fatalf("GetSetting: ok=%v err=%v", ok, err)
	}
	if got != "first" {
		t.Fatalf("value was overwritten to %q, want %q", got, "first")
	}
}
