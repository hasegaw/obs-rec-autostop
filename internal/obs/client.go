// Package obs implements the OBS WebSocket v5 RPC protocol.
package obs

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const (
	handshakeTimeout = 10 * time.Second
	writeTimeout     = 10 * time.Second
	eventBufferSize  = 256
)

// ErrClosed is returned when a request is made after Close.
var ErrClosed = errors.New("OBS WebSocket connection closed")

// Event is an OBS event, timestamped when its message was received.
type Event struct {
	Type       string
	Data       json.RawMessage
	ReceivedAt time.Time
}

// Client supports concurrent requests and delivers events in their receive order.
// Consume Events continuously: a full queue terminates the connection instead of
// dropping potentially important audio events.
type Client struct {
	conn       *websocket.Conn
	writeMu    chan struct{} // A mutex that can be acquired with a context.
	mu         sync.Mutex
	pending    map[string]chan response
	err        error
	closeOnce  sync.Once
	nextID     atomic.Uint64
	events     chan Event
	done       chan struct{}
	readerDone chan struct{}
}

type envelope struct {
	Op   int             `json:"op"`
	Data json.RawMessage `json:"d"`
}

type response struct {
	RequestID     string `json:"requestId"`
	RequestType   string `json:"requestType"`
	RequestStatus struct {
		Result bool `json:"result"`
		Code   int  `json:"code"`
	} `json:"requestStatus"`
	Data json.RawMessage `json:"responseData"`
}

// Dial connects, authenticates, and subscribes before returning. The entire
// handshake is limited to ten seconds or ctx's shorter deadline. After a
// successful Dial, use Close to end the connection; ctx only controls dialing.
func Dial(ctx context.Context, url, password string, subscriptions int) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = handshakeTimeout
	conn, resp, err := dialer.DialContext(ctx, url, nil)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, websocket.ErrBadHandshake) && resp != nil {
			return nil, fmt.Errorf("OBS WebSocket connect failed: HTTP upgrade rejected (status %d); check the host, port, and URL path", resp.StatusCode)
		}
		return nil, connectionError(ctx, "connect", err)
	}

	// Closing the socket interrupts Hello/Identified reads even if a context is
	// cancelled without a deadline. Join this watcher before transferring conn.
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopWatch:
		}
	}()
	stopWatcher := func() {
		close(stopWatch)
		<-watchDone
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetReadDeadline(deadline)
	_ = conn.SetWriteDeadline(deadline)
	conn.SetReadLimit(16 << 20)
	if err := identify(ctx, conn, password, subscriptions); err != nil {
		stopWatcher()
		_ = conn.Close()
		return nil, err
	}
	stopWatcher()
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})
	client := &Client{
		conn: conn, writeMu: make(chan struct{}, 1),
		pending: make(map[string]chan response),
		events:  make(chan Event, eventBufferSize), done: make(chan struct{}),
		readerDone: make(chan struct{}),
	}
	client.writeMu <- struct{}{}
	go client.readLoop()
	return client, nil
}

func identify(ctx context.Context, conn *websocket.Conn, password string, subscriptions int) error {
	var message envelope
	if err := conn.ReadJSON(&message); err != nil {
		return connectionError(ctx, "receive Hello", err)
	}
	if message.Op != 0 {
		return errors.New("OBS WebSocket expected Hello (v5 protocol)")
	}
	var hello struct {
		RPCVersion     int `json:"rpcVersion"`
		Authentication *struct {
			Challenge string `json:"challenge"`
			Salt      string `json:"salt"`
		} `json:"authentication"`
	}
	if json.Unmarshal(message.Data, &hello) != nil || hello.RPCVersion < 1 {
		return errors.New("OBS WebSocket received invalid Hello or unsupported RPC version")
	}
	identification := struct {
		RPCVersion         int    `json:"rpcVersion"`
		Authentication     string `json:"authentication,omitempty"`
		EventSubscriptions int    `json:"eventSubscriptions"`
	}{RPCVersion: 1, EventSubscriptions: subscriptions}
	if hello.Authentication != nil {
		if hello.Authentication.Salt == "" || hello.Authentication.Challenge == "" {
			return errors.New("OBS WebSocket received invalid authentication challenge")
		}
		secretHash := sha256.Sum256([]byte(password + hello.Authentication.Salt))
		secret := base64.StdEncoding.EncodeToString(secretHash[:])
		authHash := sha256.Sum256([]byte(secret + hello.Authentication.Challenge))
		identification.Authentication = base64.StdEncoding.EncodeToString(authHash[:])
	}
	if err := conn.WriteJSON(struct {
		Op   int `json:"op"`
		Data any `json:"d"`
	}{Op: 1, Data: identification}); err != nil {
		return connectionError(ctx, "send Identify", err)
	}
	if err := conn.ReadJSON(&message); err != nil {
		return connectionError(ctx, "receive Identified", err)
	}
	var identified struct {
		RPCVersion int `json:"negotiatedRpcVersion"`
	}
	if message.Op != 2 || json.Unmarshal(message.Data, &identified) != nil || identified.RPCVersion != 1 {
		return errors.New("OBS WebSocket expected Identified with RPC version 1")
	}
	return nil
}

// Call sends one request and decodes its response into result (unless nil).
// Cancelling ctx stops waiting; OBS may already have executed a sent request.
func (c *Client) Call(ctx context.Context, requestType string, data any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	message, err := json.Marshal(struct {
		Op   int `json:"op"`
		Data any `json:"d"`
	}{Op: 6, Data: struct {
		Type string `json:"requestType"`
		ID   string `json:"requestId"`
		Data any    `json:"requestData,omitempty"`
	}{Type: requestType, ID: id, Data: data}})
	if err != nil {
		return errors.New("encode OBS WebSocket request failed")
	}
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.write(ctx, message); err != nil {
		return err
	}
	select {
	case response := <-ch:
		if response.RequestType != requestType {
			return errors.New("OBS WebSocket response request type mismatch")
		}
		if !response.RequestStatus.Result {
			// Server comments are free text and can echo credentials/parameters.
			return fmt.Errorf("OBS request %s failed (code %d)", requestType, response.RequestStatus.Code)
		}
		if result != nil && len(response.Data) > 0 {
			if json.Unmarshal(response.Data, result) != nil {
				return errors.New("decode OBS WebSocket response failed")
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
}

func (c *Client) write(ctx context.Context, message []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	case <-c.writeMu:
	}
	defer func() { c.writeMu <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(writeTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = c.conn.SetWriteDeadline(deadline)
	// A context cancellation must also unblock an in-flight socket write.
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			c.fail(ctx.Err())
		case <-stopWatch:
		}
	}()
	err := c.conn.WriteMessage(websocket.TextMessage, message)
	close(stopWatch)
	<-watchDone
	if err != nil {
		err = connectionError(ctx, "write request", err)
		c.fail(err)
		return err
	}
	return ctx.Err()
}

func (c *Client) readLoop() {
	defer close(c.readerDone)
	defer close(c.events)
	for {
		var message envelope
		if err := c.conn.ReadJSON(&message); err != nil {
			c.fail(connectionError(context.Background(), "read message", err))
			return
		}
		receivedAt := time.Now()
		switch message.Op {
		case 5:
			var data struct {
				Type string          `json:"eventType"`
				Data json.RawMessage `json:"eventData"`
			}
			if json.Unmarshal(message.Data, &data) != nil || data.Type == "" {
				c.fail(errors.New("OBS WebSocket received malformed event"))
				return
			}
			select {
			case <-c.done:
				return
			case c.events <- Event{Type: data.Type, Data: data.Data, ReceivedAt: receivedAt}:
			default:
				c.fail(errors.New("OBS WebSocket event queue overflow; monitoring is no longer reliable"))
				return
			}
		case 7:
			var response response
			if json.Unmarshal(message.Data, &response) != nil || response.RequestID == "" {
				c.fail(errors.New("OBS WebSocket received malformed request response"))
				return
			}
			c.mu.Lock()
			ch := c.pending[response.RequestID]
			delete(c.pending, response.RequestID)
			c.mu.Unlock()
			if ch != nil {
				ch <- response
			}
		default:
			c.fail(errors.New("OBS WebSocket received unexpected message opcode"))
			return
		}
	}
}

// Events returns the event stream, closed when the reader terminates.
func (c *Client) Events() <-chan Event { return c.events }

// Done closes when the connection becomes unusable. Check Err for the cause.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the terminal connection error, or nil while connected.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close terminates the connection and waits for its reader to exit.
func (c *Client) Close() error {
	c.fail(ErrClosed)
	<-c.readerDone
	return nil
}

func (c *Client) fail(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.err = err
		c.pending = nil
		c.mu.Unlock()
		close(c.done)
		_ = c.conn.Close()
	})
}

// Never include the raw dial/read error: it can contain a URL, credentials, or
// arbitrary server-supplied close text. Numeric close codes remain diagnostic.
func connectionError(ctx context.Context, stage string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
	}
	var closed *websocket.CloseError
	if errors.As(err, &closed) {
		if closed.Code == 4009 {
			return errors.New("OBS WebSocket authentication failed; check the configured password")
		}
		return fmt.Errorf("OBS WebSocket %s failed (close code %d)", stage, closed.Code)
	}
	var dnsError *net.DNSError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameError x509.HostnameError
	var invalidCertificate x509.CertificateInvalidError
	var tlsRecordError tls.RecordHeaderError
	var reason string
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.As(err, &dnsError):
		if dnsError.IsTimeout {
			reason = "DNS lookup timed out; check the configured hostname and DNS connectivity"
		} else {
			reason = "DNS lookup failed; check the configured hostname"
		}
	case errors.As(err, &unknownAuthority):
		reason = "TLS certificate authority is not trusted"
	case errors.As(err, &hostnameError):
		reason = "TLS certificate does not match the configured hostname"
	case errors.As(err, &invalidCertificate):
		reason = "TLS certificate is invalid or expired"
	case errors.As(err, &tlsRecordError):
		reason = "TLS handshake failed; check the ws/wss URL scheme"
	case errors.Is(err, syscall.ECONNREFUSED):
		reason = "connection refused; check that the OBS WebSocket server is enabled and the host/port are correct"
	case errors.Is(err, syscall.EHOSTUNREACH):
		reason = "host is unreachable; check the destination and network connectivity"
	case errors.Is(err, syscall.ENETUNREACH):
		reason = "network is unreachable; check network connectivity"
	case errors.Is(err, syscall.ECONNRESET):
		reason = "connection was reset by the peer"
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		reason = "network access denied by the operating system or execution environment"
	case errors.As(err, &networkError) && networkError.Timeout():
		reason = "connection timed out; check the host, port, firewall, and network connectivity"
	case errors.Is(err, websocket.ErrBadHandshake):
		reason = "HTTP upgrade rejected; check the host, port, and URL path"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		reason = "connection closed before a complete response was received"
	default:
		reason = "transport or protocol error"
	}
	return fmt.Errorf("OBS WebSocket %s failed: %s", stage, reason)
}
