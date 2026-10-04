package zappymcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const DefaultAPIURL = "https://zappy.api.br"
const OAuthClientID = "zappy-mcp"

var ErrLoginRequired = errors.New("login required: run zappy-mcp login")

type Credentials struct {
	AccountID          string    `json:"account_id"`
	APIURL             string    `json:"api_url"`
	Resource           string    `json:"resource"`
	Issuer             string    `json:"issuer"`
	TokenEndpoint      string    `json:"token_endpoint"`
	RevocationEndpoint string    `json:"revocation_endpoint,omitempty"`
	ClientID           string    `json:"client_id"`
	AccessToken        string    `json:"access_token"`
	RefreshToken       string    `json:"refresh_token"`
	ExpiresAt          time.Time `json:"expires_at"`
}
type oauthMetadata struct {
	Issuer                  string   `json:"issuer"`
	AuthorizationEndpoint   string   `json:"authorization_endpoint"`
	TokenEndpoint           string   `json:"token_endpoint"`
	RevocationEndpoint      string   `json:"revocation_endpoint"`
	CodeChallengeMethods    []string `json:"code_challenge_methods_supported"`
	ResponseIssuerSupported bool     `json:"authorization_response_iss_parameter_supported"`
}
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
}
type TokenManager struct{ Root, AccountID, APIURL string }

func CurrentAccount(root string) (string, error) {
	var current struct {
		AccountID string `json:"account_id"`
	}
	if err := readJSON(filepath.Join(root, "current-account.json"), &current); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrLoginRequired
		}
		return "", err
	}
	if _, err := accountPath(root, current.AccountID); err != nil {
		return "", err
	}
	return current.AccountID, nil
}

func NewTokenManager(root, id string) (*TokenManager, error) {
	dir, err := accountPath(root, id)
	if err != nil {
		return nil, err
	}
	var creds Credentials
	if err := readJSON(filepath.Join(dir, "oauth.json"), &creds); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrLoginRequired
		}
		return nil, err
	}
	if creds.AccountID != id || validateURL(creds.APIURL) != nil {
		return nil, errors.New("invalid saved OAuth account")
	}
	return &TokenManager{Root: root, AccountID: id, APIURL: creds.APIURL}, nil
}

func (t *TokenManager) Token(ctx context.Context) (string, error) {
	dir, _ := accountPath(t.Root, t.AccountID)
	lock, err := lockFile(ctx, filepath.Join(dir, "oauth.lock"))
	if err != nil {
		return "", err
	}
	defer lock.Unlock()
	var creds Credentials
	if err := readJSON(filepath.Join(dir, "oauth.json"), &creds); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrLoginRequired
		}
		return "", err
	}
	if creds.AccountID != t.AccountID || creds.APIURL != t.APIURL {
		return "", errors.New("OAuth account changed")
	}
	if creds.AccessToken != "" && time.Until(creds.ExpiresAt) > time.Minute {
		return creds.AccessToken, nil
	}
	if creds.RefreshToken == "" {
		return "", ErrLoginRequired
	}
	token, err := exchangeToken(ctx, creds.TokenEndpoint, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {creds.RefreshToken},
		"client_id": {creds.ClientID}, "resource": {creds.Resource},
	})
	if err != nil {
		return "", err
	}
	creds.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		creds.RefreshToken = token.RefreshToken
	}
	creds.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	if err := writeJSON(filepath.Join(dir, "oauth.json"), creds); err != nil {
		return "", err
	}
	return creds.AccessToken, nil
}

func (t *TokenManager) Invalidate(ctx context.Context, used string) {
	dir, _ := accountPath(t.Root, t.AccountID)
	lock, err := lockFile(ctx, filepath.Join(dir, "oauth.lock"))
	if err != nil {
		return
	}
	defer lock.Unlock()
	var creds Credentials
	if readJSON(filepath.Join(dir, "oauth.json"), &creds) == nil && creds.AccessToken == used {
		creds.ExpiresAt = time.Time{}
		_ = writeJSON(filepath.Join(dir, "oauth.json"), creds)
	}
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return errors.New("invalid service URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()) {
		return nil
	}
	return errors.New("HTTPS required (HTTP is allowed only on loopback)")
}
func secureClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // Do not redirect credentials.
	}}
}
func getJSON(ctx context.Context, raw string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	resp, err := secureClient(30 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("discovery returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}
func exchangeToken(ctx context.Context, endpoint string, values url.Values) (tokenResponse, error) {
	if err := validateURL(endpoint); err != nil {
		return tokenResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := secureClient(30 * time.Second).Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	var token tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token); err != nil {
		return token, err
	}
	if resp.StatusCode != http.StatusOK {
		if token.Error == "invalid_grant" {
			return token, ErrLoginRequired
		}
		return token, fmt.Errorf("OAuth token exchange failed (HTTP %d)", resp.StatusCode)
	}
	if token.AccessToken == "" || token.ExpiresIn <= 0 || !strings.EqualFold(token.TokenType, "Bearer") {
		return token, errors.New("invalid OAuth bearer token")
	}
	return token, nil
}
func randomString() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func Login(ctx context.Context, root, apiURL string, port int, browser bool) error {
	apiURL = strings.TrimRight(apiURL, "/")
	if err := validateURL(apiURL); err != nil {
		return err
	}
	var resource struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := getJSON(ctx, apiURL+"/v1/mcp/oauth-resource", &resource); err != nil {
		return err
	}
	if resource.Resource != apiURL+"/v1/mcp" || len(resource.AuthorizationServers) != 1 {
		return errors.New("OAuth metadata does not match Zappy")
	}
	issuer := resource.AuthorizationServers[0]
	if err := validateURL(issuer); err != nil {
		return err
	}
	var metadata oauthMetadata
	if err := getJSON(ctx, issuer+"/.well-known/openid-configuration", &metadata); err != nil {
		return err
	}
	if metadata.Issuer != issuer {
		return errors.New("OAuth issuer mismatch")
	}
	for index, endpoint := range []string{metadata.AuthorizationEndpoint, metadata.TokenEndpoint, metadata.RevocationEndpoint} {
		if index == 2 && endpoint == "" {
			continue
		}
		if err := validateURL(endpoint); err != nil {
			return err
		}
		e, _ := url.Parse(endpoint)
		i, _ := url.Parse(issuer)
		if e.Scheme != i.Scheme || e.Host != i.Host {
			return errors.New("OAuth endpoints must belong to the issuer")
		}
	}
	supportsPKCE := false
	for _, method := range metadata.CodeChallengeMethods {
		supportsPKCE = supportsPKCE || method == "S256"
	}
	if !supportsPKCE {
		return errors.New("PKCE S256 required")
	}
	verifier, err := randomString()
	if err != nil {
		return err
	}
	state, err := randomString()
	if err != nil {
		return err
	}
	challenge := sha256.Sum256([]byte(verifier))
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("OAuth callback unavailable: %w", err)
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	codes := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		if r.Method != http.MethodGet || r.URL.Query().Get("state") != state {
			http.Error(w, "Invalid OAuth state.", 400)
			return
		}
		iss := r.URL.Query().Get("iss")
		if (metadata.ResponseIssuerSupported || iss != "") && iss != issuer {
			http.Error(w, "Invalid OAuth issuer.", 400)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "Authorization not granted. Close this window and retry login.", 400)
			return
		}
		select {
		case codes <- code:
			fmt.Fprint(w, "Authorization received. Close this window and check the terminal to confirm login.")
		default:
			http.Error(w, "Callback already received.", 409)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	authURL, _ := url.Parse(metadata.AuthorizationEndpoint)
	authURL.RawQuery = url.Values{
		"response_type": {"code"}, "client_id": {OAuthClientID}, "redirect_uri": {redirect},
		"scope": {"openid profile email offline_access zappy:read zappy:send"}, "state": {state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
		"resource": {resource.Resource},
	}.Encode()
	fmt.Fprintln(os.Stderr, "Authorize Zappy in your browser:", authURL.String())
	if browser {
		openBrowser(authURL.String())
	}
	var code string
	select {
	case <-ctx.Done():
		return ctx.Err()
	case code = <-codes:
	}
	token, err := exchangeToken(ctx, metadata.TokenEndpoint, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {OAuthClientID},
		"redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {resource.Resource},
	})
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/v1/mcp/account", nil)
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	resp, err := secureClient(30 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("account validation failed (HTTP %d)", resp.StatusCode)
	}
	var account struct {
		ID string `json:"account_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&account); err != nil {
		return err
	}
	dir, err := accountPath(root, account.ID)
	if err != nil {
		return err
	}
	lock, err := lockFile(ctx, filepath.Join(dir, "oauth.lock"))
	if err != nil {
		return err
	}
	defer lock.Unlock()
	creds := Credentials{
		AccountID: account.ID, APIURL: apiURL, Resource: resource.Resource, Issuer: issuer,
		TokenEndpoint: metadata.TokenEndpoint, RevocationEndpoint: metadata.RevocationEndpoint, ClientID: OAuthClientID,
		AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, ExpiresAt: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second),
	}
	if err := writeJSON(filepath.Join(dir, "oauth.json"), creds); err != nil {
		return err
	}
	currentLock, err := lockFile(ctx, filepath.Join(root, "current-account.lock"))
	if err != nil {
		return err
	}
	defer currentLock.Unlock()
	return writeJSON(filepath.Join(root, "current-account.json"), map[string]string{"account_id": account.ID})
}

func Logout(ctx context.Context, root, id string) error {
	dir, err := accountPath(root, id)
	if err != nil {
		return err
	}
	lock, err := lockFile(ctx, filepath.Join(dir, "oauth.lock"))
	if err != nil {
		return err
	}
	defer lock.Unlock()
	var creds Credentials
	if err := readJSON(filepath.Join(dir, "oauth.json"), &creds); err != nil {
		return err
	}
	if creds.RevocationEndpoint != "" && creds.RefreshToken != "" {
		if err := validateURL(creds.RevocationEndpoint); err != nil {
			return err
		}
		values := url.Values{"client_id": {creds.ClientID}, "token": {creds.RefreshToken}, "token_type_hint": {"refresh_token"}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, creds.RevocationEndpoint, strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := secureClient(30 * time.Second).Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("OAuth revocation failed (HTTP %d)", resp.StatusCode)
		}
	}
	if err := os.Remove(filepath.Join(dir, "oauth.json")); err != nil {
		return err
	}
	currentLock, err := lockFile(ctx, filepath.Join(root, "current-account.lock"))
	if err != nil {
		return err
	}
	defer currentLock.Unlock()
	current, _ := CurrentAccount(root)
	if current == id {
		return os.Remove(filepath.Join(root, "current-account.json"))
	}
	return nil
}
func openBrowser(raw string) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", raw)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", raw)
	default:
		command = exec.Command("xdg-open", raw)
	}
	command.Stderr = os.Stderr
	if command.Start() == nil {
		go command.Wait()
	}
}
