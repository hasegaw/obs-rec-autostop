package obs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type testRequest struct {
	Type string          `json:"requestType"`
	ID   string          `json:"requestId"`
	Data json.RawMessage `json:"requestData"`
}

func testServer(t *testing.T, handler func(*websocket.Conn)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		handler(conn)
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func serverWrite(t *testing.T, conn *websocket.Conn, op int, data any) bool {
	t.Helper()
	if err := conn.WriteJSON(struct {
		Op   int `json:"op"`
		Data any `json:"d"`
	}{Op: op, Data: data}); err != nil {
		t.Errorf("server write: %v", err)
		return false
	}
	return true
}

func serverIdentify(t *testing.T, conn *websocket.Conn) bool {
	t.Helper()
	if !serverWrite(t, conn, 0, map[string]any{"rpcVersion": 1}) {
		return false
	}
	var message envelope
	if err := conn.ReadJSON(&message); err != nil {
		t.Errorf("read Identify: %v", err)
		return false
	}
	if message.Op != 1 {
		t.Errorf("Identify opcode: %d", message.Op)
		return false
	}
	var data map[string]any
	if err := json.Unmarshal(message.Data, &data); err != nil {
		t.Errorf("decode Identify: %v", err)
		return false
	}
	if _, exists := data["authentication"]; exists {
		t.Error("authentication should be omitted for unauthenticated server")
		return false
	}
	return serverWrite(t, conn, 2, map[string]any{"negotiatedRpcVersion": 1})
}

func serverRequest(t *testing.T, conn *websocket.Conn) (testRequest, bool) {
	t.Helper()
	var message envelope
	if err := conn.ReadJSON(&message); err != nil {
		t.Errorf("read request: %v", err)
		return testRequest{}, false
	}
	var request testRequest
	if message.Op != 6 || json.Unmarshal(message.Data, &request) != nil || request.ID == "" {
		t.Error("invalid request envelope")
		return testRequest{}, false
	}
	return request, true
}

func serverResponse(t *testing.T, conn *websocket.Conn, request testRequest, ok bool, code int, data any) bool {
	t.Helper()
	return serverWrite(t, conn, 7, map[string]any{
		"requestId": request.ID, "requestType": request.Type,
		"requestStatus": map[string]any{"result": ok, "code": code, "comment": "untrusted secret"},
		"responseData":  data,
	})
}

func dialTest(t *testing.T, url string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := Dial(ctx, url, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func awaitDone(t *testing.T, client *Client) {
	t.Helper()
	select {
	case <-client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("client did not terminate")
	}
}

func TestDialAuthentication(t *testing.T) {
	url := testServer(t, func(conn *websocket.Conn) {
		serverWrite(t, conn, 0, map[string]any{
			"rpcVersion": 1,
			"authentication": map[string]string{
				"salt":      "lM1GncleQOaCu9lT1yeUZhFYnqhsLLP1G5lAGo3ixaI=",
				"challenge": "+IxH4CnCiqpX1rM9scsNynZzbOe4KhDeYcTNS3PDaeY=",
			},
		})
		var message envelope
		if err := conn.ReadJSON(&message); err != nil {
			t.Errorf("read Identify: %v", err)
			return
		}
		var data struct {
			RPCVersion     int    `json:"rpcVersion"`
			Authentication string `json:"authentication"`
			Subscriptions  int    `json:"eventSubscriptions"`
		}
		if err := json.Unmarshal(message.Data, &data); err != nil {
			t.Errorf("decode Identify: %v", err)
			return
		}
		if message.Op != 1 || data.RPCVersion != 1 || data.Subscriptions != 65536 {
			t.Errorf("unexpected Identify parameters: %+v", data)
		}
		if data.Authentication != "1Ct943GAT+6YQUUX47Ia/ncufilbe6+oD6lY+5kaCu4=" {
			t.Error("incorrect authentication challenge response")
		}
		serverWrite(t, conn, 2, map[string]any{"negotiatedRpcVersion": 1})
		_, _, _ = conn.ReadMessage()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := Dial(ctx, url, "supersecretpassword", 65536)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.Err() != nil {
		t.Fatalf("new connection error: %v", client.Err())
	}
}

func TestAuthenticationFailureRedactsDetails(t *testing.T) {
	url := testServer(t, func(conn *websocket.Conn) {
		serverWrite(t, conn, 0, map[string]any{"rpcVersion": 1})
		var message envelope
		if err := conn.ReadJSON(&message); err != nil {
			t.Errorf("read Identify: %v", err)
			return
		}
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4009, "secret-password"), time.Now().Add(time.Second))
	})
	_, err := Dial(context.Background(), url, "secret-password", 0)
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("expected authentication failure, got %v", err)
	}
	if strings.Contains(err.Error(), "secret-password") || strings.Contains(err.Error(), url) {
		t.Fatalf("credentials leaked: %v", err)
	}
	_, err = Dial(context.Background(), "not-a-url:secret-password", "secret-password", 0)
	if err == nil || strings.Contains(err.Error(), "secret-password") {
		t.Fatalf("invalid URL was not redacted: %v", err)
	}
}

func TestCallConcurrentResponsesAndEvent(t *testing.T) {
	url := testServer(t, func(conn *websocket.Conn) {
		if !serverIdentify(t, conn) {
			return
		}
		first, ok := serverRequest(t, conn)
		if !ok {
			return
		}
		second, ok := serverRequest(t, conn)
		if !ok {
			return
		}
		if first.ID == second.ID {
			t.Error("request IDs must be unique")
		}
		serverWrite(t, conn, 5, map[string]any{
			"eventType": "InputVolumeMeters", "eventData": map[string]any{"inputs": []any{}},
		})
		serverResponse(t, conn, second, true, 100, map[string]any{"echo": second.Type})
		serverResponse(t, conn, first, true, 100, map[string]any{"echo": first.Type})
		_, _, _ = conn.ReadMessage()
	})
	client := dialTest(t, url)
	started := time.Now()
	var wg sync.WaitGroup
	for _, name := range []string{"GetRecordStatus", "GetVersion"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var result struct {
				Echo string `json:"echo"`
			}
			if err := client.Call(ctx, name, map[string]any{"test": true}, &result); err != nil {
				t.Errorf("Call %s: %v", name, err)
			} else if result.Echo != name {
				t.Errorf("response for %s matched %s", name, result.Echo)
			}
		}(name)
	}
	wg.Wait()
	select {
	case event := <-client.Events():
		if event.Type != "InputVolumeMeters" || string(event.Data) != `{"inputs":[]}` {
			t.Errorf("event mismatch: %+v", event)
		}
		if event.ReceivedAt.Before(started) || event.ReceivedAt.After(time.Now()) {
			t.Errorf("invalid receive timestamp: %v", event.ReceivedAt)
		}
	case <-time.After(time.Second):
		t.Fatal("event not received")
	}
}

func TestCallTimeoutLateResponseAndNextCall(t *testing.T) {
	releaseResponse := make(chan struct{})
	url := testServer(t, func(conn *websocket.Conn) {
		if !serverIdentify(t, conn) {
			return
		}
		first, ok := serverRequest(t, conn)
		if !ok {
			return
		}
		<-releaseResponse
		serverResponse(t, conn, first, true, 100, nil)
		second, ok := serverRequest(t, conn)
		if !ok {
			return
		}
		serverResponse(t, conn, second, true, 100, map[string]any{"outputActive": true})
		_, _, _ = conn.ReadMessage()
	})
	client := dialTest(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err := client.Call(ctx, "GetVersion", nil, nil)
	cancel()
	close(releaseResponse)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var result struct {
		Active bool `json:"outputActive"`
	}
	if err := client.Call(ctx, "GetRecordStatus", nil, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Active {
		t.Fatal("next request did not receive its response")
	}
}

func TestCallFailureAndCloseUnblocksPending(t *testing.T) {
	pendingReceived := make(chan struct{})
	url := testServer(t, func(conn *websocket.Conn) {
		if !serverIdentify(t, conn) {
			return
		}
		request, ok := serverRequest(t, conn)
		if !ok {
			return
		}
		serverResponse(t, conn, request, false, 500, nil)
		if _, ok := serverRequest(t, conn); !ok {
			return
		}
		close(pendingReceived)
		_, _, _ = conn.ReadMessage()
	})
	client := dialTest(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Call(ctx, "StopRecord", nil, nil); err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "untrusted secret") {
		t.Fatalf("expected redacted failed request, got %v", err)
	}
	result := make(chan error, 1)
	go func() { result <- client.Call(ctx, "GetVersion", nil, nil) }()
	select {
	case <-pendingReceived:
	case <-ctx.Done():
		t.Fatal("pending request not sent")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("close error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("pending request did not unblock")
	}
	if _, open := <-client.Events(); open {
		t.Fatal("Events channel remained open after Close")
	}
	if err := client.Call(context.Background(), "GetVersion", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Call after Close: %v", err)
	}
}

func TestDialHonorsTimeoutAndCancellation(t *testing.T) {
	for _, phase := range []string{"Hello", "Identified"} {
		for _, timeout := range []bool{true, false} {
			name := phase + "/cancel"
			if timeout {
				name = phase + "/timeout"
			}
			t.Run(name, func(t *testing.T) {
				waiting := make(chan struct{})
				url := testServer(t, func(conn *websocket.Conn) {
					if phase == "Identified" {
						serverWrite(t, conn, 0, map[string]any{"rpcVersion": 1})
						var message envelope
						if err := conn.ReadJSON(&message); err != nil {
							t.Errorf("read Identify: %v", err)
							return
						}
					}
					close(waiting)
					_, _, _ = conn.ReadMessage()
				})
				ctx, cancel := context.WithCancel(context.Background())
				if timeout {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 40*time.Millisecond)
				}
				defer cancel()
				result := make(chan error, 1)
				go func() {
					client, err := Dial(ctx, url, "", 0)
					if client != nil {
						_ = client.Close()
					}
					result <- err
				}()
				select {
				case <-waiting:
				case <-time.After(time.Second):
					t.Fatal("handshake never started")
				}
				want := context.DeadlineExceeded
				if !timeout {
					cancel()
					want = context.Canceled
				}
				select {
				case err := <-result:
					if !errors.Is(err, want) {
						t.Fatalf("expected %v, got %v", want, err)
					}
				case <-time.After(time.Second):
					t.Fatal("Dial ignored context cancellation")
				}
			})
		}
	}
}

func TestRemoteDisconnectAndOverflow(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		name := "disconnect"
		if overflow {
			name = "overflow"
		}
		t.Run(name, func(t *testing.T) {
			url := testServer(t, func(conn *websocket.Conn) {
				if !serverIdentify(t, conn) {
					return
				}
				if overflow {
					for i := 0; i < eventBufferSize+1; i++ {
						if !serverWrite(t, conn, 5, map[string]any{"eventType": "InputVolumeMeters", "eventData": map[string]any{"index": i}}) {
							return
						}
					}
					_, _, _ = conn.ReadMessage()
				}
			})
			client := dialTest(t, url)
			awaitDone(t, client)
			if client.Err() == nil {
				t.Fatal("disconnection has no error")
			}
			if overflow && !strings.Contains(client.Err().Error(), "overflow") {
				t.Fatalf("expected event overflow error, got %v", client.Err())
			}
			if err := client.Call(context.Background(), "GetVersion", nil, nil); err == nil {
				t.Fatal("request succeeded after terminal error")
			}
		})
	}
}

func TestConnectionErrorDiagnosticsRedactDetails(t *testing.T) {
	const secret = "private-password-and-hostname"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"refused", syscall.ECONNREFUSED, "connection refused"},
		{"host unreachable", syscall.EHOSTUNREACH, "host is unreachable"},
		{"network unreachable", syscall.ENETUNREACH, "network is unreachable"},
		{"reset", syscall.ECONNRESET, "connection was reset"},
		{"access denied", syscall.EACCES, "network access denied"},
		{"permission denied", syscall.EPERM, "network access denied"},
		{"DNS", &net.DNSError{Err: secret, Name: secret}, "DNS lookup failed"},
		{"DNS timeout", &net.DNSError{Err: secret, Name: secret, IsTimeout: true}, "DNS lookup timed out"},
		{"timeout", &net.OpError{Op: secret, Net: secret, Err: syscall.ETIMEDOUT}, "connection timed out"},
		{"handshake", websocket.ErrBadHandshake, "HTTP upgrade rejected"},
		{"CA", x509.UnknownAuthorityError{Cert: &x509.Certificate{}}, "TLS certificate authority is not trusted"},
		{"hostname", x509.HostnameError{Certificate: &x509.Certificate{}, Host: secret}, "TLS certificate does not match"},
		{"certificate", x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired, Detail: secret}, "TLS certificate is invalid or expired"},
		{"TLS record", tls.RecordHeaderError{Msg: secret}, "TLS handshake failed"},
		{"EOF", io.EOF, "connection closed before a complete response"},
		{"short EOF", io.ErrUnexpectedEOF, "connection closed before a complete response"},
		{"auth", &websocket.CloseError{Code: 4009, Text: secret}, "authentication failed"},
		{"close", &websocket.CloseError{Code: 1006, Text: secret}, "close code 1006"},
		{"unknown", errors.New(secret), "transport or protocol error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := connectionError(context.Background(), "connect", fmt.Errorf("%s: %w", secret, tc.err))
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked private details: %v", err)
			}
		})
	}
}

func TestDialHTTPStatusDiagnostics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("secret response body"))
	}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/secret-path"
	_, err := Dial(context.Background(), url, "secret-password", 0)
	if err == nil || !strings.Contains(err.Error(), "HTTP upgrade rejected (status 403)") {
		t.Fatalf("expected HTTP status diagnostic, got %v", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("HTTP error leaked private details: %v", err)
	}
}
