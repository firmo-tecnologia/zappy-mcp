package zappymcp

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Collector struct {
	API    *API
	Store  *Store
	mu     sync.Mutex
	states map[string]listenerState
}
type listenerState struct {
	Connected     bool      `json:"connected"`
	UpdatedAt     time.Time `json:"updated_at"`
	LastMessageAt time.Time `json:"last_message_at,omitempty"`
	Error         string    `json:"error,omitempty"`
}
type listenerSession struct {
	ID        string    `json:"id"`
	StreamURL string    `json:"stream_url"`
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expires_at"`
}

// One collector per account, including when Claude and Codex run concurrently.
// Standby processes take over when the leader exits; listen can run independently.
func (c *Collector) Run(ctx context.Context) {
	c.states = map[string]listenerState{}
	lock, err := lockFile(ctx, filepath.Join(c.Store.Dir, "listener.lock"))
	if err != nil {
		return
	}
	defer lock.Unlock()
	c.supervise(ctx)
}
func (c *Collector) state(id string, connected bool, cause error, received bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.states[id]
	state.Connected, state.UpdatedAt = connected, time.Now().UTC()
	state.Error = ""
	if cause != nil {
		state.Error = cause.Error()
	}
	if received {
		state.LastMessageAt = time.Now().UTC()
	}
	c.states[id] = state
	if err := writeJSON(filepath.Join(c.Store.Dir, "listener.json"), map[string]any{
		"updated_at": time.Now().UTC(), "instances": c.states,
	}); err != nil {
		log.Printf("listener status write failed: %v", err)
	}
}
func (c *Collector) supervise(ctx context.Context) {
	workers := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	defer func() {
		for _, cancel := range workers {
			cancel()
		}
		wg.Wait()
	}()
	for ctx.Err() == nil {
		instances, err := c.API.Instances(ctx)
		if err != nil {
			c.state("_account", false, err, false)
			if errors.Is(err, ErrLoginRequired) {
				return
			}
		} else {
			c.state("_account", true, nil, false)
			wanted := map[string]bool{}
			for _, instance := range instances {
				if !validUUID(instance.ID) {
					continue
				}
				wanted[instance.ID] = true
				if _, exists := workers[instance.ID]; !exists {
					workerCtx, cancel := context.WithCancel(ctx)
					workers[instance.ID] = cancel
					wg.Add(1)
					go func(id string) { defer wg.Done(); c.watch(workerCtx, id) }(instance.ID)
				}
			}
			for id, cancel := range workers {
				if !wanted[id] {
					cancel()
					delete(workers, id)
				}
			}
		}
		if !sleep(ctx, 20*time.Second) {
			return
		}
	}
}
func (c *Collector) watch(ctx context.Context, instanceID string) {
	backoff := time.Second
	var session listenerSession
	defer c.state(instanceID, false, nil, false)
	for ctx.Err() == nil {
		if session.ID == "" || time.Now().After(session.ExpiresAt) {
			err := c.API.request(ctx, http.MethodPost, "/v1/instances/"+instanceID+"/webhook-listener/sessions",
				map[string]any{"events": []string{"message.received", "message.sent"}}, &session)
			if err != nil {
				c.state(instanceID, false, err, false)
				if !sleep(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			if !validUUID(session.ID) || session.Secret == "" || !session.ExpiresAt.After(time.Now()) {
				c.state(instanceID, false, errors.New("invalid listener session"), false)
				session = listenerSession{}
				if !sleep(ctx, 5*time.Second) {
					return
				}
				continue
			}
		}
		// Bound the lifetime even if a broken upstream never closes its stream.
		streamCtx, cancel := context.WithDeadline(ctx, session.ExpiresAt)
		connectedAt := time.Now()
		err := c.consume(streamCtx, instanceID, session)
		cancel()
		if ctx.Err() != nil {
			return
		}
		c.state(instanceID, false, err, false)
		if time.Since(connectedAt) > 5*time.Second {
			backoff = time.Second
		}
		if time.Now().After(session.ExpiresAt) {
			session = listenerSession{}
			continue // Normal expiration should renew without an artificial gap.
		}
		if errors.Is(err, errNewSession) {
			session = listenerSession{}
		}
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

var errNewSession = errors.New("listener session expired or busy")

func (c *Collector) consume(ctx context.Context, id string, session listenerSession) error {
	base, _ := url.Parse(c.API.Tokens.APIURL)
	stream, err := url.Parse(session.StreamURL)
	if err != nil {
		return err
	}
	stream = base.ResolveReference(stream)
	if stream.Scheme != base.Scheme || stream.Host != base.Host || stream.User != nil ||
		stream.Path != "/v1/webhook-listener/sessions/"+session.ID+"/stream" {
		return errors.New("untrusted listener stream URL")
	}
	token, err := c.API.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, stream.String(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := secureClient(0).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		c.API.Tokens.Invalidate(ctx, token)
	}
	if resp.StatusCode == 404 || resp.StatusCode == 410 || resp.StatusCode == 409 {
		return errNewSession
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("listener stream HTTP %d", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return errors.New("listener did not return SSE")
	}
	c.state(id, true, nil, false)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	var event string
	var data strings.Builder
	flush := func() error {
		defer func() { event = ""; data.Reset() }()
		if event != "webhook" || data.Len() == 0 {
			return nil
		}
		var frame struct {
			Body      string `json:"body"`
			Signature string `json:"signature"`
		}
		if err := json.Unmarshal([]byte(data.String()), &frame); err != nil {
			return err
		}
		mac := hmac.New(sha256.New, []byte(session.Secret))
		mac.Write([]byte(frame.Body))
		signature, err := hex.DecodeString(strings.TrimPrefix(frame.Signature, "sha256="))
		if err != nil || !strings.HasPrefix(frame.Signature, "sha256=") || !hmac.Equal(signature, mac.Sum(nil)) {
			return errors.New("invalid webhook signature")
		}
		message, err := decodeEvent([]byte(frame.Body), id, c.API.Tokens.AccountID)
		if err != nil {
			return err
		}
		if err := c.Store.Save(ctx, message); err != nil {
			return err
		}
		c.state(id, true, nil, true)
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			if data.Len() != 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line[5:], " "))
			if data.Len() > 2<<20 {
				return errors.New("listener frame too large")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	return io.EOF
}
func sleep(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
