package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/openai"
)

// dualFormatHandler answers the same logical response in whichever wire
// format the request asked for — the one server both call paths talk to,
// so the parity tests below exercise identical scenarios on both.
func dualFormatHandler(quiet time.Duration, fail bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		if fail {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":{"message":"all failed","type":"kram_gateway_error","combo":"default","retryable":true,"attempts":[{"provider":"p1","class":"server_error"}]}}`))
			return
		}

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			time.Sleep(quiet)
			w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"))
			w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n"))
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		time.Sleep(quiet)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Role: "assistant", Content: "hi"}}},
			Usage:   openai.Usage{PromptTokens: 3, CompletionTokens: 1},
		})
	}
}

// callPaths runs the same scenario through both callModel paths. This is
// the drift guard: the two parallel implementations of "one model call"
// must honor the same contract — the missing-heartbeat asymmetry (#105)
// and the untyped streaming gateway error (#109) both lived exactly in
// the gap this closes.
func callPaths(t *testing.T, srvURL string, fn func(t *testing.T, path string, s *Service)) {
	t.Helper()
	for _, path := range []string{"buffered", "streaming"} {
		t.Run(path, func(t *testing.T) {
			s := &Service{
				gateway: gatewayclient.New(srvURL), heartbeatInterval: 20 * time.Millisecond,
				calibrator: newTokenCalibrator(),
				cfg:        Config{PreferStreaming: path == "streaming", MaxGatewayRounds: 1},
			}
			fn(t, path, s)
		})
	}
}

func TestParityBothPathsHeartbeatThroughSilenceAndAgreeOnResult(t *testing.T) {
	srv := httptest.NewServer(dualFormatHandler(120*time.Millisecond, false))
	defer srv.Close()

	callPaths(t, srv.URL, func(t *testing.T, path string, s *Service) {
		var heartbeats int
		result, err := s.callModel(context.Background(), "default", nil, nil, func(e Event) {
			if e.Kind == EventHeartbeat {
				heartbeats++
			}
		})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if result.Content != "hi" {
			t.Errorf("%s content = %q, want %q", path, result.Content, "hi")
		}
		if result.Usage.PromptTokens != 3 || result.Usage.CompletionTokens != 1 {
			t.Errorf("%s usage = %+v, want prompt 3 / completion 1", path, result.Usage)
		}
		if heartbeats < 2 {
			t.Errorf("%s emitted %d heartbeats through a 120ms silence with a 20ms interval, want >=2", path, heartbeats)
		}
	})
}

func TestParityBothPathsSurfaceTypedRetryableGatewayError(t *testing.T) {
	srv := httptest.NewServer(dualFormatHandler(0, true))
	defer srv.Close()

	callPaths(t, srv.URL, func(t *testing.T, path string, s *Service) {
		_, err := s.callModel(context.Background(), "default", nil, nil, nil)
		var ge *gatewayclient.GatewayError
		if !errors.As(err, &ge) {
			t.Fatalf("%s: all-candidates-failed must surface as *GatewayError, got %T: %v", path, err, err)
		}
		if !ge.Retryable || ge.Combo != "default" || len(ge.Attempts) != 1 {
			t.Errorf("%s: typed fields lost in transit: %+v", path, ge)
		}
	})
}
