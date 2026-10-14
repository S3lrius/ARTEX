package notify

import (
	"net/url"
	"testing"
	"time"
)

// The signature fixtures were computed independently with OpenSSL, not with this package's own implementation.
// Otherwise the test would only prove the code did not change, not that the algorithm is right.
//
//	TS=1700000000000, SECRET=SECtest123
//	DingTalk: printf '%s\n%s' "$TS" "$SECRET" | openssl dgst -sha256 -hmac "$SECRET" -binary | openssl base64 -A
//	      -> w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE=
//	Feishu: printf '' | openssl dgst -sha256 -hmac "$(printf '%s\n%s' "$TS" "$SECRET")" -binary | openssl base64 -A
//	      -> Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo=
const (
	signTestTSMillis = int64(1700000000000)
	signTestSecret   = "SECtest123"
	dingTalkExpected = "w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE="
	feishuExpected   = "Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo="
)

func TestDingTalkSignMatchesReference(t *testing.T) {
	got, err := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=tok", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatalf("signing failed: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("the produced address could not be parsed: %v", err)
	}
	q := u.Query()
	if q.Get("sign") != dingTalkExpected {
		t.Errorf("signature mismatch\nexpected %s\ngot %s", dingTalkExpected, q.Get("sign"))
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Errorf("the timestamp should be milliseconds and passed through unchanged, got %q", q.Get("timestamp"))
	}
	// An existing query parameter (access_token) must not be overwritten by the signature.
	if q.Get("access_token") != "tok" {
		t.Errorf("an existing query parameter was lost, got %q", q.Get("access_token"))
	}
}

func TestFeishuSignMatchesReference(t *testing.T) {
	got := feishuSign("1700000000000", signTestSecret)
	if got != feishuExpected {
		t.Errorf("signature mismatch\nexpected %s\ngot %s", feishuExpected, got)
	}
}

// TestSignAlgorithmsDiffer locks in the difference between the two algorithms. Each is the other's argument order
// (DingTalk key=secret, Feishu key=the string to sign). Copying one into the other fails verification,
// and this case keeps a later refactor from merging them into one function.
func TestSignAlgorithmsDiffer(t *testing.T) {
	ts := "1700000000000"
	dingURL, err := dingTalkSignedURL("https://example.com/hook", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	dq, _ := url.Parse(dingURL)
	if dq.Query().Get("sign") == feishuSign(ts, signTestSecret) {
		t.Fatal("DingTalk and Feishu produced the same signature, so one of the algorithms is wrong")
	}
}

func TestDingTalkNoSecretLeavesURLUntouched(t *testing.T) {
	// A robot with signing turned off must not grow timestamp or sign parameters out of nowhere.
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=tok"
	got, err := dingTalkSignedURL(hook, "", time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("the address should not change when secret is unset, got %q", got)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	ok := []string{"https://example.com/hook", "http://10.0.0.1:8080/x?y=1"}
	for _, s := range ok {
		if err := validateHTTPURL(s); err != nil {
			t.Errorf("%q should be accepted: %v", s, err)
		}
	}
	// file:// and similar schemes must not pass. http.Client's handling of them is outside the expected range.
	bad := []string{"", "file:///etc/passwd", "ftp://example.com", "https://", "gopher://x"}
	for _, s := range bad {
		if err := validateHTTPURL(s); err == nil {
			t.Errorf("%q should be rejected", s)
		}
	}
}
