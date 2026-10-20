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
// Why test this separately: each channel adapter only handles the platform's
// own business error codes (DingTalk's errcode, Feishu's code, Telegram's ok
// field), while **HTTP-layer** classification is done uniformly by doJSON --
// the two are independent lines of defense. Without this one, a relay
// gateway returning 503 would be treated as a permanent failure and give up
// retrying immediately; while a 403 would be treated as retryable and waste
// three rounds of backoff for nothing.

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
		{"429 rate limited is retryable", 429, false},
		{"408 request timeout is retryable", 408, false},
		{"500 server error is retryable", 500, false},
		{"502 gateway error is retryable", 502, false},
		{"503 service unavailable is retryable", 503, false},
		{"400 bad params is permanent", 400, true},
		{"401 auth failure is permanent", 401, true},
		{"403 forbidden is permanent", 403, true},
		{"404 not found is permanent", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx should not error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("non-2xx should error")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("wrong permanent determination for HTTP %d: expected %v got %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// The status code must appear in the error, or the user has no
			// way to tell whether they misconfigured something or the other
			// end is down. Asserting the number rather than Go's English
			// StatusText: this package's copy is in English (consistent with
			// the rest of the project), and the number is the
			// language-independent part that can be asserted reliably.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("error message should include the HTTP status code %d, got %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet covers snippet: the error description
// returned by the other end must be carried back, or the user only knows
// "it failed" without knowing why the other end rejected it.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("should error")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("error message should carry back the other end's description, got %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded constrains snippet's shape: the
// other end's response goes as-is into the last_error column and the
// frontend table, and multiple lines/excessive length would break the layout
// and the payload.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// A response with newlines, a tab, and 5000 characters of overlong content.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("should error")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("error message should be compressed to a single line, got %q", msg)
	}
	// snippet caps at 200 characters + a fixed prefix; the total must be far
	// smaller than the original response.
	if len(msg) > 400 {
		t.Errorf("error message too long (%d bytes), should be truncated by snippet: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse confirms reading is capped: when the
// other end misbehaves and returns an oversized body, the whole response must
// not be read into memory (every delivery-history entry stores one last_error).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("should error")
	}
	if len(err.Error()) > 400 {
		t.Errorf("an oversized response should be read with a length cap and truncated, error length is %d", len(err.Error()))
	}
}
