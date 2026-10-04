package zappymcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type API struct{ Tokens *TokenManager }
type Instance struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	PhoneNumber *string `json:"phone_number"`
}

func (a *API) request(ctx context.Context, method, path string, body any, out any) error {
	token, err := a.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	var encoded []byte
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Tokens.APIURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := secureClient(45 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		a.Tokens.Invalidate(ctx, token)
		return fmt.Errorf("OAuth token rejected; it will be refreshed on the next call. Do not automatically retry sends")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&failure)
		if failure.Error == "" {
			failure.Error = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("Zappy HTTP %d: %s", resp.StatusCode, failure.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func (a *API) Instances(ctx context.Context) ([]Instance, error) {
	var instances []Instance
	err := a.request(ctx, http.MethodGet, "/v1/instances", nil, &instances)
	return instances, err
}

func (a *API) ResolveInstance(ctx context.Context, requested string) (Instance, error) {
	instances, err := a.Instances(ctx)
	if err != nil {
		return Instance{}, err
	}
	var connected []Instance
	for _, instance := range instances {
		if requested != "" && instance.ID == requested {
			return instance, nil
		}
		if instance.Status == "connected" {
			connected = append(connected, instance)
		}
	}
	if requested != "" {
		return Instance{}, fmt.Errorf("instance does not belong to this account")
	}
	if len(connected) != 1 {
		return Instance{}, fmt.Errorf("specify instance_id: account has %d connected instances", len(connected))
	}
	return connected[0], nil
}

func (a *API) Send(ctx context.Context, instance, to, text string) (string, error) {
	if !validUUID(instance) {
		return "", fmt.Errorf("invalid instance_id")
	}
	if strings.TrimSpace(to) == "" || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("to and message are required")
	}
	var response struct {
		ID string `json:"message_id"`
	}
	err := a.request(ctx, http.MethodPost, "/v1/instances/"+url.PathEscape(instance)+"/messages",
		map[string]string{"to": to, "type": "text", "content": text}, &response)
	if err == nil && response.ID == "" {
		err = fmt.Errorf("Zappy returned no message ID; do not retry automatically")
	}
	return response.ID, err
}
