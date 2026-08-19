package openai

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestClassifyHTTPStatusBuckets(t *testing.T) {
	cases := []struct {
		status int
		want   FailureClass
	}{
		{429, ClassRateLimit},
		{401, ClassAuth},
		{403, ClassAuth},
		{400, ClassInvalidRequest},
		{404, ClassInvalidRequest},
		{422, ClassInvalidRequest},
		{500, ClassServerError},
		{502, ClassServerError},
		{503, ClassServerError},
		{418, ClassUnknown}, // a real status Classify has no bucket for
	}
	for _, c := range cases {
		got := Classify(c.status, errors.New("upstream returned a non-2xx status"))
		if got != c.want {
			t.Errorf("Classify(%d, err) = %q, want %q", c.status, got, c.want)
		}
	}
}

func TestClassifyNoResponseReached(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"context deadline", context.DeadlineExceeded, ClassTimeout},
		{"context canceled", context.Canceled, ClassCancelled},
		{"wrapped deadline", errWrap(context.DeadlineExceeded), ClassTimeout},
		{"net timeout", &net.DNSError{IsTimeout: true}, ClassTimeout},
		{"generic network error", errors.New("dial tcp: connection refused"), ClassNetwork},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(0, c.err)
			if got != c.want {
				t.Errorf("Classify(0, %v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

func TestClassifyNoStatusNoError(t *testing.T) {
	if got := Classify(0, nil); got != ClassUnknown {
		t.Errorf("Classify(0, nil) = %q, want %q", got, ClassUnknown)
	}
}

func errWrap(err error) error {
	return errors.Join(errors.New("request failed"), err)
}

func TestFailureClassRetryable(t *testing.T) {
	retryable := []FailureClass{ClassNetwork, ClassTimeout, ClassServerError, ClassRateLimit}
	for _, c := range retryable {
		if !c.Retryable() {
			t.Errorf("%q.Retryable() = false, want true", c)
		}
	}
	notRetryable := []FailureClass{ClassAuth, ClassInvalidRequest, ClassContentRejected, ClassCancelled, ClassUnknown}
	for _, c := range notRetryable {
		if c.Retryable() {
			t.Errorf("%q.Retryable() = true, want false", c)
		}
	}
}

func TestFailureClassCountsAgainstBreaker(t *testing.T) {
	counts := []FailureClass{ClassNetwork, ClassTimeout, ClassServerError, ClassRateLimit, ClassAuth, ClassContentRejected, ClassUnknown}
	for _, c := range counts {
		if !c.CountsAgainstBreaker() {
			t.Errorf("%q.CountsAgainstBreaker() = false, want true", c)
		}
	}
	doesNotCount := []FailureClass{ClassInvalidRequest, ClassCancelled}
	for _, c := range doesNotCount {
		if c.CountsAgainstBreaker() {
			t.Errorf("%q.CountsAgainstBreaker() = true, want false", c)
		}
	}
}
