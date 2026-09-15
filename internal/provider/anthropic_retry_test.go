package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// fakeReply is one scripted response from the fake Messages API.
type fakeReply func(w http.ResponseWriter)

const overloadedBody = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`

// httpError replies with a plain (non-streaming) API error.
func httpError(status int, body string) fakeReply {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

// sseStream replies 200 with the given server-sent events, in order.
func sseStream(events ...[2]string) fakeReply {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range events {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev[0], ev[1])
		}
	}
}

// helloStream is a complete, successful streamed reply with one text block.
var helloStream = sseStream(
	[2]string{"message_start", messageStart},
	[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
	[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`},
	[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`},
	[2]string{"message_stop", `{"type":"message_stop"}`},
)

// fakeAnthropic serves the scripted replies in order (repeating the last) and
// returns a provider pointed at it plus the request counter. The SDK's own
// retries are disabled so every request is one CreateMessage attempt, and the
// backoff is zeroed, counting how often CreateMessage decides to wait.
func fakeAnthropic(t *testing.T, replies ...fakeReply) (*AnthropicProvider, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var requests, backoffs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(requests.Add(1))
		if n > len(replies) {
			n = len(replies)
		}
		replies[n-1](w)
	}))
	t.Cleanup(srv.Close)

	orig := anthropicRetryBackoff
	anthropicRetryBackoff = func(int) time.Duration { backoffs.Add(1); return 0 }
	t.Cleanup(func() { anthropicRetryBackoff = orig })

	p := &AnthropicProvider{client: anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test-key"),
		option.WithMaxRetries(0),
	)}
	return p, &requests, &backoffs
}

func createHello(t *testing.T, p *AnthropicProvider) (*MessageResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.CreateMessage(ctx, MessageParams{
		Model:     "claude-sonnet-5",
		MaxTokens: 100,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
}

func assertHello(t *testing.T, resp *MessageResponse, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("CreateMessage error = %v, want success after retry", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "hello" {
		t.Fatalf("response content = %+v, want one text block %q", resp.Content, "hello")
	}
}

// An overloaded (529) attempt followed by a good one must return the good
// response. The error from the failed attempt used to leak into the next one,
// so a retry could never succeed.
func TestCreateMessage_RetryAfterOverloadedSucceeds(t *testing.T) {
	p, requests, _ := fakeAnthropic(t, httpError(529, overloadedBody), helloStream)

	resp, err := createHello(t, p)
	assertHello(t, resp, err)
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

// A rate-limited (429) attempt is retried the same way.
func TestCreateMessage_RetryAfterRateLimitSucceeds(t *testing.T) {
	p, requests, _ := fakeAnthropic(t,
		httpError(429, `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`),
		helloStream)

	resp, err := createHello(t, p)
	assertHello(t, resp, err)
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

// An overloaded error that arrives mid-stream (after a 200) is retried, and
// the partial first attempt doesn't leak into the successful response.
func TestCreateMessage_RetryAfterMidStreamOverloadSucceeds(t *testing.T) {
	midStream := sseStream(
		[2]string{"message_start", messageStart},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial "}}`},
		[2]string{"error", overloadedBody},
	)
	p, requests, _ := fakeAnthropic(t, midStream, helloStream)

	resp, err := createHello(t, p)
	assertHello(t, resp, err)
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

// A non-retryable error fails straight away, without waiting.
func TestCreateMessage_NonRetryableErrorFailsFast(t *testing.T) {
	p, requests, backoffs := fakeAnthropic(t,
		httpError(400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`),
		helloStream)

	if _, err := createHello(t, p); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("CreateMessage error = %v, want the 400", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
	if got := backoffs.Load(); got != 0 {
		t.Errorf("backoffs = %d, want 0", got)
	}
}

// When every attempt is overloaded, CreateMessage gives up after maxRetries
// attempts, returns the error, and doesn't sleep after the final attempt.
func TestCreateMessage_RetriesExhausted(t *testing.T) {
	p, requests, backoffs := fakeAnthropic(t, httpError(529, overloadedBody))

	if _, err := createHello(t, p); err == nil || !strings.Contains(err.Error(), "529") {
		t.Fatalf("CreateMessage error = %v, want the 529", err)
	}
	if got := requests.Load(); got != 3 {
		t.Errorf("requests = %d, want 3", got)
	}
	if got := backoffs.Load(); got != 2 {
		t.Errorf("backoffs = %d, want 2 (none after the last attempt)", got)
	}
}
