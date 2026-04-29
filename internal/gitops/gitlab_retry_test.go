package gitops

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// fakeResp builds a minimal *gitlab.Response with the given status code.
func fakeResp(code int) *gitlab.Response {
	return &gitlab.Response{Response: &http.Response{StatusCode: code}}
}

func zeroDelays(t *testing.T) {
	t.Helper()
	orig := retryDelays
	retryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { retryDelays = orig })
}

func TestWithRetry_SuccessFirstAttempt(t *testing.T) {
	zeroDelays(t)
	calls := 0
	err := withRetry(context.Background(), "op", func() (*gitlab.Response, error) {
		calls++
		return fakeResp(http.StatusOK), nil
	})
	if err != nil {
		t.Fatalf("want nil error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("want 1 call, got %d", calls)
	}
}

func TestWithRetry_NonTransientError_NoRetry(t *testing.T) {
	zeroDelays(t)
	sentinel := errors.New("bad request")
	calls := 0
	err := withRetry(context.Background(), "op", func() (*gitlab.Response, error) {
		calls++
		return fakeResp(http.StatusBadRequest), sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("want 1 call (no retry on 4xx), got %d", calls)
	}
}

func TestWithRetry_TransientThenSuccess(t *testing.T) {
	zeroDelays(t)
	calls := 0
	err := withRetry(context.Background(), "op", func() (*gitlab.Response, error) {
		calls++
		if calls < 3 {
			return fakeResp(http.StatusInternalServerError), errors.New("server error")
		}
		return fakeResp(http.StatusOK), nil
	})
	if err != nil {
		t.Fatalf("want nil error after recovery, got %v", err)
	}
	if calls != 3 {
		t.Errorf("want 3 calls, got %d", calls)
	}
}

func TestWithRetry_RateLimitThenSuccess(t *testing.T) {
	zeroDelays(t)
	calls := 0
	err := withRetry(context.Background(), "op", func() (*gitlab.Response, error) {
		calls++
		if calls == 1 {
			return fakeResp(http.StatusTooManyRequests), errors.New("rate limited")
		}
		return fakeResp(http.StatusOK), nil
	})
	if err != nil {
		t.Fatalf("want nil error after rate-limit retry, got %v", err)
	}
	if calls != 2 {
		t.Errorf("want 2 calls, got %d", calls)
	}
}

func TestWithRetry_AllAttemptsFailTransient(t *testing.T) {
	zeroDelays(t)
	sentinel := errors.New("always 500")
	calls := 0
	err := withRetry(context.Background(), "op", func() (*gitlab.Response, error) {
		calls++
		return fakeResp(http.StatusInternalServerError), sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel error after exhaustion, got %v", err)
	}
	if calls != 3 {
		t.Errorf("want 3 calls (all attempts), got %d", calls)
	}
}

func TestWithRetry_ContextCancelled(t *testing.T) {
	// Real delays so ctx cancellation matters — but cancel immediately.
	orig := retryDelays
	retryDelays = []time.Duration{time.Hour, time.Hour, time.Hour}
	t.Cleanup(func() { retryDelays = orig })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := withRetry(ctx, "op", func() (*gitlab.Response, error) {
		return fakeResp(http.StatusInternalServerError), errors.New("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestWithRetry_NetworkError_Retried(t *testing.T) {
	zeroDelays(t)
	calls := 0
	err := withRetry(context.Background(), "op", func() (*gitlab.Response, error) {
		calls++
		if calls < 3 {
			// nil response simulates a network error (no HTTP response at all)
			return nil, errors.New("connection refused")
		}
		return fakeResp(http.StatusOK), nil
	})
	if err != nil {
		t.Fatalf("want success after network retry, got %v", err)
	}
	if calls != 3 {
		t.Errorf("want 3 calls, got %d", calls)
	}
}
