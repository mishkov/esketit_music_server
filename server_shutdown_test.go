package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

const shutdownTestTimeout = 5 * time.Second

func TestServeHTTPDrainsInFlightRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	requestFinished := make(chan struct{})
	shutdownStarted := make(chan struct{})
	shutdownFinished := make(chan struct{})
	var releaseOnce sync.Once

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}

	baseServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(requestStarted)
			<-releaseRequest
			_, _ = io.WriteString(w, "finished")
			close(requestFinished)
		}),
	}
	baseServer.RegisterOnShutdown(func() {
		close(shutdownStarted)
	})
	server := &listenerHTTPServer{
		Server:           baseServer,
		listener:         listener,
		shutdownFinished: shutdownFinished,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		releaseOnce.Do(func() { close(releaseRequest) })
		_ = server.Close()
	}()

	type lifecycleResult struct {
		err              error
		shutdownFinished bool
	}
	serveResult := make(chan lifecycleResult, 1)
	go func() {
		err := serveHTTP(ctx, server, time.Second)
		result := lifecycleResult{err: err}
		select {
		case <-shutdownFinished:
			result.shutdownFinished = true
		default:
		}
		serveResult <- result
	}()

	type responseResult struct {
		statusCode int
		body       string
		err        error
	}
	responseResults := make(chan responseResult, 1)
	client := &http.Client{Timeout: shutdownTestTimeout}
	defer client.CloseIdleConnections()
	go func() {
		response, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			responseResults <- responseResult{err: err}
			return
		}
		defer response.Body.Close()

		body, err := io.ReadAll(response.Body)
		responseResults <- responseResult{
			statusCode: response.StatusCode,
			body:       string(body),
			err:        err,
		}
	}()

	waitForShutdownTestSignal(t, requestStarted, "request to start")
	cancel()
	waitForShutdownTestSignal(t, shutdownStarted, "shutdown to start")

	select {
	case result := <-serveResult:
		t.Fatalf("serveHTTP() returned before the in-flight request completed: %v", result.err)
	default:
	}

	releaseOnce.Do(func() { close(releaseRequest) })
	waitForShutdownTestSignal(t, requestFinished, "request to finish")

	select {
	case result := <-responseResults:
		if result.err != nil {
			t.Fatalf("GET error = %v", result.err)
		}
		if result.statusCode != http.StatusOK {
			t.Errorf("status = %d, want %d", result.statusCode, http.StatusOK)
		}
		if result.body != "finished" {
			t.Errorf("body = %q, want %q", result.body, "finished")
		}
	case <-time.After(shutdownTestTimeout):
		t.Fatal("timed out waiting for the in-flight response")
	}

	select {
	case result := <-serveResult:
		if result.err != nil {
			t.Fatalf("serveHTTP() error = %v, want nil", result.err)
		}
		if !result.shutdownFinished {
			t.Fatal("serveHTTP() returned before Shutdown() finished")
		}
	case <-time.After(shutdownTestTimeout):
		t.Fatal("serveHTTP() did not exit after the in-flight request completed")
	}
}

func TestServeHTTPTreatsErrServerClosedAsSuccess(t *testing.T) {
	server := &fakeGracefulHTTPServer{
		serve: func() error {
			return fmt.Errorf("wrapped: %w", http.ErrServerClosed)
		},
	}

	if err := serveHTTP(context.Background(), server, time.Second); err != nil {
		t.Fatalf("serveHTTP() error = %v, want nil", err)
	}
	if server.shutdownCalls != 0 {
		t.Errorf("Shutdown() calls = %d, want 0", server.shutdownCalls)
	}
	if server.closeCalls != 0 {
		t.Errorf("Close() calls = %d, want 0", server.closeCalls)
	}
}

func TestServeHTTPReturnsUnexpectedServeError(t *testing.T) {
	wantErr := errors.New("accept failed")
	server := &fakeGracefulHTTPServer{
		serve: func() error {
			return wantErr
		},
	}

	err := serveHTTP(context.Background(), server, time.Second)
	if !errors.Is(err, wantErr) {
		t.Fatalf("serveHTTP() error = %v, want error wrapping %v", err, wantErr)
	}
	if server.shutdownCalls != 0 {
		t.Errorf("Shutdown() calls = %d, want 0", server.shutdownCalls)
	}
	if server.closeCalls != 0 {
		t.Errorf("Close() calls = %d, want 0", server.closeCalls)
	}
}

func TestServeHTTPForceClosesAfterShutdownTimeout(t *testing.T) {
	serveStopped := make(chan struct{})
	shutdownStarted := make(chan struct{})
	forceCloseCalled := make(chan struct{})
	var forceCloseOnce sync.Once
	var stopServeOnce sync.Once
	defer stopServeOnce.Do(func() { close(serveStopped) })

	server := &fakeGracefulHTTPServer{
		serve: func() error {
			<-serveStopped
			return http.ErrServerClosed
		},
		shutdown: func(ctx context.Context) error {
			close(shutdownStarted)
			<-ctx.Done()
			return ctx.Err()
		},
		closeServer: func() error {
			forceCloseOnce.Do(func() {
				close(forceCloseCalled)
			})
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- serveHTTP(ctx, server, 10*time.Millisecond)
	}()
	cancel()

	waitForShutdownTestSignal(t, shutdownStarted, "shutdown to start")
	waitForShutdownTestSignal(t, forceCloseCalled, "forced close")

	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("serveHTTP() error = %v, want context deadline exceeded", err)
		}
	case <-time.After(shutdownTestTimeout):
		t.Fatal("serveHTTP() hung after the shutdown deadline")
	}
	if server.shutdownCalls != 1 {
		t.Errorf("Shutdown() calls = %d, want 1", server.shutdownCalls)
	}
	if server.closeCalls != 1 {
		t.Errorf("Close() calls = %d, want 1", server.closeCalls)
	}
	stopServeOnce.Do(func() { close(serveStopped) })
}

type listenerHTTPServer struct {
	*http.Server
	listener         net.Listener
	shutdownFinished chan struct{}
}

func (server *listenerHTTPServer) ListenAndServe() error {
	return server.Serve(server.listener)
}

func (server *listenerHTTPServer) Shutdown(ctx context.Context) error {
	err := server.Server.Shutdown(ctx)
	if server.shutdownFinished != nil {
		close(server.shutdownFinished)
	}
	return err
}

type fakeGracefulHTTPServer struct {
	serve         func() error
	shutdown      func(context.Context) error
	closeServer   func() error
	shutdownCalls int
	closeCalls    int
}

func (server *fakeGracefulHTTPServer) ListenAndServe() error {
	return server.serve()
}

func (server *fakeGracefulHTTPServer) Shutdown(ctx context.Context) error {
	server.shutdownCalls++
	if server.shutdown == nil {
		return nil
	}
	return server.shutdown(ctx)
}

func (server *fakeGracefulHTTPServer) Close() error {
	server.closeCalls++
	if server.closeServer == nil {
		return nil
	}
	return server.closeServer()
}

func waitForShutdownTestSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(shutdownTestTimeout):
		t.Fatalf("timed out waiting for %s", description)
	}
}
