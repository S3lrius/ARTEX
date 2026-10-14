package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// This file covers doJSON's HTTP-layer error classification.
//
// It is tested on its own because each channel adapter only handles that platform's business error code
// (DingTalk errcode, Feishu code, Telegram's ok field), while the HTTP-layer classification is done once in doJSON.
// They are two separate defenses. Without this one, a gateway that returns 503 would be treated as permanent and
// retry would be given up; a 403 would be treated as retryable and burn three backoff rounds.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 success is not an error", 200, false},
		{"429 rate limit is retryable", 429, false},
		{"408 request timeout is retryable", 408, false},
		{"500 server error is retryable", 500, false},
		{"502 gateway error is retryable", 502, false},
		{"503 unavailable is retryable", 503, false},
		{"400 bad parameters is permanent", 400, true},
		{"401 auth failure is permanent", 401, true},
		{"403 forbidden is permanent", 403, true},
		{"404 address does not exist is permanent", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx should not be an error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a non-2xx response should be an error")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d permanent classification is wrong: expected %v, got %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// The status code has to appear in the error, or the user cannot tell a bad config from a dead peer.
			// Assert the number. The wording can change; the number is the stable part.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("the error should include HTTP status %d, got %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet covers snippet: the peer's error text has to come back.
// Otherwise the user only knows that it failed, not why the peer refused.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("the error should include the peer's explanation, got %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded constrains the shape of snippet.
// The peer response is stored as-is in the last_error column and a frontend table. Multiple lines or a huge body break the layout and the payload.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// A response with newlines, a tab, and 5000 characters of overflow.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("the error should be one line, got %q", msg)
	}
	// The snippet cap is 200 characters plus a fixed prefix, so the total must be far smaller than the original response.
	if len(msg) > 400 {
		t.Errorf("the error is too long (%d bytes) and should have been cut by snippet: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse confirms the read is capped. An abnormal peer that returns a huge body
// must not be read entirely into memory (every delivery history row stores a last_error).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(err.Error()) > 400 {
		t.Errorf("an oversized response should be read with a cap and truncated; error length %d", len(err.Error()))
	}
}
