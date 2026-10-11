package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type testRoundTrip func(*http.Request) (*http.Response, error)

func (f testRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type testHTTPServer struct {
	URL    string
	closed atomic.Bool
}

func (s *testHTTPServer) Close() { s.closed.Store(true) }

var testAgentPort atomic.Int32

// Exercise the agent's HTTP handlers and client without opening a socket or
// allowing tests to reach an agent running on the user's machine.
func testAgentServer(t *testing.T, handler http.Handler) *testHTTPServer {
	t.Helper()
	host := fmt.Sprintf("127.0.0.1:%d", 50000+testAgentPort.Add(1))
	srv := &testHTTPServer{URL: "http://" + host}
	previous := http.DefaultTransport
	http.DefaultTransport = testRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != host {
			if prev, ok := previous.(testRoundTrip); ok {
				return prev(req)
			}
			return nil, errors.New("no test agent at this address")
		}
		if srv.closed.Load() {
			return nil, errors.New("test agent is offline")
		}
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		r := req.Clone(req.Context())
		r.RemoteAddr = local
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Result(), nil
	})
	t.Cleanup(func() { srv.Close(); http.DefaultTransport = previous })
	return srv
}
