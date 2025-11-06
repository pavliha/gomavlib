package gomavlib

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// TestWebSocketEndpoint_AuthError tests that auth errors cause the endpoint
// to block and return errTerminated instead of panicking
func TestWebSocketEndpoint_AuthError(t *testing.T) {
	// Create a server that returns 401 Unauthorized
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("Unauthorized"))
	}))
	defer server.Close()

	// Replace http:// with ws://
	wsURL := "ws" + server.URL[4:]

	endpoint := EndpointWebSocket{
		URL:                  wsURL,
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       1 * time.Second,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0, // Unlimited attempts
		HandshakeTimeout:     1 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start the endpoint in a goroutine
	provideDone := make(chan struct{})
	var provideErr error
	go func() {
		defer close(provideDone)
		_, _, provideErr = e.provide()
	}()

	// Wait a bit for the auth error to occur
	time.Sleep(300 * time.Millisecond)

	// Verify endpoint is in disconnected or reconnecting state (auth errors are treated as network errors)
	e.mu.Lock()
	state := e.state
	e.mu.Unlock()
	// Auth errors from HTTP status codes are categorized as network errors, so reconnecting
	require.Contains(t, []WebSocketState{WSStateDisconnected, WSStateReconnecting, WSStateConnecting}, state)

	// Close the endpoint
	e.close()

	// Wait for provide() to return
	select {
	case <-provideDone:
		// Should return errTerminated, not panic
		require.ErrorIs(t, provideErr, errTerminated)
	case <-time.After(2 * time.Second):
		t.Fatal("provide() did not return after close")
	}
}

// TestWebSocketEndpoint_CircuitBreakerOpen tests that when circuit breaker opens,
// the endpoint blocks and returns errTerminated instead of panicking
func TestWebSocketEndpoint_CircuitBreakerOpen(t *testing.T) {
	// Use invalid port so connections fail immediately
	wsURL := "ws://127.0.0.1:9999/test"

	endpoint := EndpointWebSocket{
		URL:                   wsURL,
		InitialRetryPeriod:    50 * time.Millisecond,
		MaxRetryPeriod:        100 * time.Millisecond,
		BackoffMultiplier:     2.0,
		MaxReconnectAttempts:  0, // Unlimited attempts
		HandshakeTimeout:      100 * time.Millisecond,
		MaxConsecutiveErrors:  3, // Open circuit breaker after 3 errors
		CircuitBreakerTimeout: 1 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start the endpoint in a goroutine
	provideDone := make(chan struct{})
	var provideErr error
	go func() {
		defer close(provideDone)
		_, _, provideErr = e.provide()
	}()

	// Wait for circuit breaker to open (3 consecutive errors)
	time.Sleep(500 * time.Millisecond)

	// Verify circuit breaker is open
	e.mu.Lock()
	isOpen := e.isCircuitBreakerOpen()
	e.mu.Unlock()
	require.True(t, isOpen, "circuit breaker should be open after consecutive errors")

	// Close the endpoint
	e.close()

	// Wait for provide() to return
	select {
	case <-provideDone:
		// Should return errTerminated, not panic
		require.ErrorIs(t, provideErr, errTerminated)
	case <-time.After(2 * time.Second):
		t.Fatal("provide() did not return after close")
	}
}

// TestWebSocketEndpoint_SuccessfulConnection tests normal connection flow
func TestWebSocketEndpoint_SuccessfulConnection(t *testing.T) {
	// Create a WebSocket server
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("Upgrade error: %v", err)
			return
		}
		defer conn.Close()

		// Keep connection open for a bit
		time.Sleep(500 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[4:]

	endpoint := EndpointWebSocket{
		URL:                  wsURL,
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       1 * time.Second,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0,
		HandshakeTimeout:     1 * time.Second,
		PingPeriod:           5 * time.Second,
		PongWait:             10 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start the endpoint in a goroutine
	provideDone := make(chan struct{})
	var provideLabel string
	var provideErr error
	go func() {
		defer close(provideDone)
		provideLabel, _, provideErr = e.provide()
	}()

	// Wait for connection to establish
	time.Sleep(200 * time.Millisecond)

	// Verify endpoint is in connected state
	e.mu.Lock()
	state := e.state
	e.mu.Unlock()
	require.Equal(t, WSStateConnected, state)

	// Close the endpoint
	e.close()

	// Wait for provide() to return
	select {
	case <-provideDone:
		// provide() may return nil if connection was established before close,
		// or errTerminated if closed during connection
		if provideErr != nil {
			require.ErrorIs(t, provideErr, errTerminated)
		}
		if provideErr == nil {
			require.NotEmpty(t, provideLabel)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provide() did not return after close")
	}
}

// TestWebSocketEndpoint_ReconnectAfterDisconnect tests reconnection logic
func TestWebSocketEndpoint_ReconnectAfterDisconnect(t *testing.T) {
	connectionCount := 0
	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connectionCount++

		// First connection: close immediately (simulate network issue)
		if connectionCount == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		// Second connection: succeed
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("Upgrade error: %v", err)
			return
		}
		defer conn.Close()

		// Keep connection open
		time.Sleep(500 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[4:]

	endpoint := EndpointWebSocket{
		URL:                  wsURL,
		InitialRetryPeriod:   100 * time.Millisecond,
		MaxRetryPeriod:       500 * time.Millisecond,
		BackoffMultiplier:    2.0,
		MaxReconnectAttempts: 0,
		HandshakeTimeout:     1 * time.Second,
		PingPeriod:           5 * time.Second,
		PongWait:             10 * time.Second,
	}

	conf, err := endpoint.init(nil)
	require.NoError(t, err)
	e := conf.(*endpointWebSocket)

	// Start the endpoint in a goroutine
	provideDone := make(chan struct{})
	var provideErr error
	go func() {
		defer close(provideDone)
		_, _, provideErr = e.provide()
	}()

	// Wait for reconnection to succeed
	time.Sleep(500 * time.Millisecond)

	// Verify we got at least 2 connection attempts
	require.GreaterOrEqual(t, connectionCount, 2, "should have attempted reconnection")

	// Verify endpoint is in connected state
	e.mu.Lock()
	state := e.state
	e.mu.Unlock()
	require.Equal(t, WSStateConnected, state)

	// Close the endpoint
	e.close()

	// Wait for provide() to return
	select {
	case <-provideDone:
		// provide() may return nil if connection was established before close,
		// or errTerminated if closed during connection/reconnection
		if provideErr != nil {
			require.ErrorIs(t, provideErr, errTerminated)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provide() did not return after close")
	}
}

// TestWebSocketEndpoint_OnlyReturnsErrTerminated verifies that provide() never
// returns errors other than errTerminated (gomavlib contract)
func TestWebSocketEndpoint_OnlyReturnsErrTerminated(t *testing.T) {
	testCases := []struct {
		name       string
		statusCode int
		setupFunc  func(e *endpointWebSocket)
	}{
		{
			name:       "401 Unauthorized",
			statusCode: http.StatusUnauthorized,
		},
		{
			name:       "403 Forbidden",
			statusCode: http.StatusForbidden,
		},
		{
			name:       "404 Not Found",
			statusCode: http.StatusNotFound,
		},
		{
			name: "Circuit Breaker Open",
			setupFunc: func(e *endpointWebSocket) {
				// Force circuit breaker to open
				e.mu.Lock()
				e.circuitBreakerOpenAt = time.Now()
				e.mu.Unlock()
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create server that returns the specified status code
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.statusCode != 0 {
					w.WriteHeader(tc.statusCode)
				}
			}))
			defer server.Close()

			wsURL := "ws" + server.URL[4:]

			endpoint := EndpointWebSocket{
				URL:                   wsURL,
				InitialRetryPeriod:    50 * time.Millisecond,
				MaxRetryPeriod:        100 * time.Millisecond,
				BackoffMultiplier:     2.0,
				MaxReconnectAttempts:  0,
				HandshakeTimeout:      200 * time.Millisecond,
				MaxConsecutiveErrors:  3,
				CircuitBreakerTimeout: 1 * time.Second,
			}

			conf, err := endpoint.init(nil)
			require.NoError(t, err)
			e := conf.(*endpointWebSocket)

			if tc.setupFunc != nil {
				tc.setupFunc(e)
			}

			// Start the endpoint in a goroutine
			provideDone := make(chan struct{})
			var provideErr error
			go func() {
				defer close(provideDone)
				_, _, provideErr = e.provide()
			}()

			// Wait a bit for the error condition to occur
			time.Sleep(200 * time.Millisecond)

			// Close the endpoint
			e.close()

			// Wait for provide() to return
			select {
			case <-provideDone:
				// CRITICAL: Must only return errTerminated, never other errors
				if provideErr != nil && !errors.Is(provideErr, errTerminated) {
					t.Fatalf("provide() returned unexpected error: %v (expected errTerminated or nil)", provideErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("provide() did not return after close")
			}
		})
	}
}
