// Package client talks HTTP over a local Unix socket, never a TCP port.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"rosetta/internal/identity"
	"rosetta/internal/protocol"
	"rosetta/internal/store"
)

type Client struct {
	http *http.Client
}

func New(socketPath string) *Client {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		ResponseHeaderTimeout: 5 * time.Second,
	}
	return &Client{http: &http.Client{Transport: transport}}
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Health(ctx context.Context) (protocol.Health, error) {
	return c.health(ctx, false)
}

// LifecycleHealth permits the previous lifecycle-compatible daemon version only
// so an upgraded CLI can stop it before starting the new binary.
func (c *Client) LifecycleHealth(ctx context.Context) (protocol.Health, error) {
	return c.health(ctx, true)
}

func (c *Client) health(ctx context.Context, allowPrevious bool) (protocol.Health, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := c.request(ctx, "GET", "/v1/health", nil)
	if err != nil {
		return protocol.Health{}, err
	}
	defer resp.Body.Close()
	var health protocol.Health
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&health); err != nil {
		return health, err
	}
	compatible := health.Protocol == protocol.Version || (allowPrevious && health.Protocol == 3)
	if !compatible || (health.Status != "ready" && health.Status != "stopping") {
		return health, fmt.Errorf("daemon is not ready or has an incompatible protocol")
	}
	return health, nil
}

// Shutdown addresses an observed instance, never a possibly recycled PID.
func (c *Client) Shutdown(ctx context.Context, instanceID string) error {
	body, err := json.Marshal(protocol.ShutdownRequest{InstanceID: instanceID})
	if err != nil {
		return err
	}
	resp, err := c.request(ctx, "POST", "/v1/shutdown", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var health protocol.Health
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&health); err != nil {
		return err
	}
	if (health.Protocol != protocol.Version && health.Protocol != 3) || health.InstanceID != instanceID || health.Status != "stopping" {
		return fmt.Errorf("invalid shutdown acknowledgment")
	}
	return nil
}

func (c *Client) Chat(ctx context.Context, req protocol.ChatRequest, onEvent func(protocol.Event) error) error {
	scope, err := identity.Resolve(req.User, req.Session)
	if err != nil {
		return err
	}
	req.User, req.Session = scope.User, scope.Session
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute+5*time.Second)
	defer cancel()
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := c.request(ctx, "POST", "/v1/chat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	seq, runID, sessionID := 0, "", ""
	for scanner.Scan() {
		var event protocol.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return fmt.Errorf("invalid stream event: %w", err)
		}
		seq++
		if seq == 1 {
			runID, sessionID = event.RunID, event.SessionID
			if event.Type != protocol.Started {
				return fmt.Errorf("stream did not start with turn.started")
			}
		}
		if event.Version != protocol.Version || event.RunID == "" || event.RunID != runID || event.SessionID == "" || event.SessionID != sessionID || event.User != req.User || event.Session != req.Session || event.Seq != seq {
			return fmt.Errorf("stream identity or sequence mismatch")
		}
		switch event.Type {
		case protocol.Started:
			if seq != 1 {
				return fmt.Errorf("duplicate turn.started")
			}
		case protocol.TextDelta, protocol.Completed, protocol.Failed, protocol.RoundStarted, protocol.ToolStarted, protocol.ToolFinished:
		default:
			return fmt.Errorf("unsupported event type: %s", event.Type)
		}
		if err := onEvent(event); err != nil {
			return err
		}
		if event.Type == protocol.Failed {
			return fmt.Errorf("turn failed: %s", event.Error)
		}
		if event.Type == protocol.Completed {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("stream disconnected before turn.completed; result is incomplete")
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://rosetta"+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon (start 'rosetta start [agent]' first): %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var apiErr protocol.Error
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&apiErr); err != nil {
			return nil, fmt.Errorf("daemon returned HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s: %s", apiErr.Code, apiErr.Message)
	}
	return resp, nil
}

// History queries always include the selected user; IDs/cursors do not select ownership.
func (c *Client) Sessions(ctx context.Context, user, after string, limit int) (store.SessionPage, error) {
	var page store.SessionPage
	query := url.Values{"user": {user}, "after": {after}, "limit": {strconv.Itoa(limit)}}
	err := c.query(ctx, "/v1/sessions?"+query.Encode(), &page)
	return page, err
}
func (c *Client) History(ctx context.Context, user, session string, after int64, limit int) (store.HistoryPage, error) {
	var page store.HistoryPage
	query := url.Values{"user": {user}, "session": {session}, "after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(limit)}}
	err := c.query(ctx, "/v1/history?"+query.Encode(), &page)
	return page, err
}
func (c *Client) query(ctx context.Context, path string, target any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := c.request(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, store.MaxPageBytes+1))
	if err != nil {
		return err
	}
	if len(data) > store.MaxPageBytes {
		return fmt.Errorf("history response exceeds page limit")
	}
	return json.Unmarshal(data, target)
}
