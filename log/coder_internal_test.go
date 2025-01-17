package log

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"cdr.dev/slog/sloggers/slogtest"
	"github.com/coder/coder/v2/agent/proto"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/agentsdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoder(t *testing.T) {
	t.Parallel()

	t.Run("V1/OK", func(t *testing.T) {
		t.Parallel()

		token := uuid.NewString()
		gotLogs := make(chan struct{})
		var closeOnce sync.Once
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v2/buildinfo" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"version": "v2.8.9"}`))
				return
			}
			defer closeOnce.Do(func() { close(gotLogs) })
			tokHdr := r.Header.Get(codersdk.SessionTokenHeader)
			assert.Equal(t, token, tokHdr)
			var req agentsdk.PatchLogs
			err := json.NewDecoder(r.Body).Decode(&req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if assert.Len(t, req.Logs, 1) {
				assert.Equal(t, "hello world", req.Logs[0].Output)
				assert.Equal(t, codersdk.LogLevelInfo, req.Logs[0].Level)
			}
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		u, err := url.Parse(srv.URL)
		require.NoError(t, err)
		log, closeLog, err := Coder(ctx, u, token)
		require.NoError(t, err)
		defer closeLog()
		log(LevelInfo, "hello %s", "world")
		<-gotLogs
	})

	t.Run("V1/ErrUnauthorized", func(t *testing.T) {
		t.Parallel()

		token := uuid.NewString()
		authFailed := make(chan struct{})
		var closeOnce sync.Once
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v2/buildinfo" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"version": "v2.8.9"}`))
				return
			}
			defer closeOnce.Do(func() { close(authFailed) })
			w.WriteHeader(http.StatusUnauthorized)
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		u, err := url.Parse(srv.URL)
		require.NoError(t, err)
		log, _, err := Coder(ctx, u, token)
		require.NoError(t, err)
		// defer closeLog()
		log(LevelInfo, "hello %s", "world")
		<-authFailed
	})

	t.Run("V1/ErrNotCoder", func(t *testing.T) {
		t.Parallel()

		token := uuid.NewString()
		handlerCalled := make(chan struct{})
		var closeOnce sync.Once
		handler := func(w http.ResponseWriter, r *http.Request) {
			defer closeOnce.Do(func() { close(handlerCalled) })
			_, _ = fmt.Fprintf(w, `hello world`)
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		u, err := url.Parse(srv.URL)
		require.NoError(t, err)
		_, _, err = Coder(ctx, u, token)
		require.ErrorContains(t, err, "get coder build version")
		require.ErrorContains(t, err, "unexpected non-JSON response")
		<-handlerCalled
	})

	// In this test, we just fake out the DRPC server.
	t.Run("V2/OK", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ld := &fakeLogDest{t: t}
		ls := agentsdk.NewLogSender(slogtest.Make(t, nil))
		logFunc, logsDone := sendLogsV2(ctx, ld, ls, slogtest.Make(t, nil))
		defer logsDone()

		// Send some logs
		for i := 0; i < 10; i++ {
			logFunc(LevelInfo, "info log %d", i+1)
		}

		// Cancel and wait for flush
		cancel()
		t.Logf("cancelled")
		logsDone()

		require.Len(t, ld.logs, 10)
	})

	// In this test, we just stand up an endpoint that does not
	// do dRPC. We'll try to connect, fail to websocket upgrade
	// and eventually give up.
	t.Run("V2/Err", func(t *testing.T) {
		t.Parallel()

		token := uuid.NewString()
		handlerDone := make(chan struct{})
		var closeOnce sync.Once
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v2/buildinfo" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"version": "v2.9.0"}`))
				return
			}
			defer closeOnce.Do(func() { close(handlerDone) })
			w.WriteHeader(http.StatusOK)
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		u, err := url.Parse(srv.URL)
		require.NoError(t, err)
		_, _, err = Coder(ctx, u, token)
		require.ErrorContains(t, err, "failed to WebSocket dial")
		require.ErrorIs(t, err, context.DeadlineExceeded)
		<-handlerDone
	})

	// In this test, we validate that a 401 error on the initial connect
	// results in a retry. When envbuilder initially attempts to connect
	// using the Coder agent token, the workspace build may not yet have
	// completed.
	t.Run("V2Retry", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		token := uuid.NewString()
		done := make(chan struct{})
		handlerSend := make(chan int)
		handler := func(w http.ResponseWriter, r *http.Request) {
			t.Logf("test handler: %s", r.URL.Path)
			if r.URL.Path == "/api/v2/buildinfo" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"version": "v2.9.0"}`))
				return
			}
			code := <-handlerSend
			t.Logf("test handler response: %d", code)
			w.WriteHeader(code)
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		u, err := url.Parse(srv.URL)
		require.NoError(t, err)
		var connectError error
		go func() {
			defer close(handlerSend)
			defer close(done)
			_, _, connectError = Coder(ctx, u, token)
		}()

		// Initial: unauthorized
		handlerSend <- http.StatusUnauthorized
		// 2nd try: still unauthorized
		handlerSend <- http.StatusUnauthorized
		// 3rd try: authorized
		handlerSend <- http.StatusOK

		cancel()

		<-done
		require.ErrorContains(t, connectError, "failed to WebSocket dial")
		require.ErrorIs(t, connectError, context.Canceled)
	})
}

type fakeLogDest struct {
	t    testing.TB
	logs []*proto.Log
}

func (d *fakeLogDest) BatchCreateLogs(ctx context.Context, request *proto.BatchCreateLogsRequest) (*proto.BatchCreateLogsResponse, error) {
	d.t.Logf("got %d logs, ", len(request.Logs))
	d.logs = append(d.logs, request.Logs...)
	return &proto.BatchCreateLogsResponse{}, nil
}
