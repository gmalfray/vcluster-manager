package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client is a Vault API client for managing Kubernetes auth backends.
type Client struct {
	addr  string
	token string
	mu    sync.RWMutex // protects token
	http  *http.Client
	// AppRole credentials (non-empty when using AppRole auth)
	roleID   string
	secretID string
}

// NewClient creates a Vault client with a static token.
// Prefer NewClientWithAppRole for production use.
func NewClient(addr, token string) *Client {
	return &Client{
		addr:  strings.TrimRight(addr, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// NewClientWithAppRole creates a Vault client using AppRole authentication.
// It performs the initial login immediately, then starts a background goroutine
// that renews the token at 2/3 of its lease duration, re-authenticating if renewal fails.
func NewClientWithAppRole(ctx context.Context, addr, roleID, secretID string) (*Client, error) {
	c := &Client{
		addr:     strings.TrimRight(addr, "/"),
		roleID:   roleID,
		secretID: secretID,
		http:     &http.Client{Timeout: 30 * time.Second},
	}

	leaseDuration, err := c.loginAppRole(ctx)
	if err != nil {
		return nil, fmt.Errorf("AppRole login: %w", err)
	}

	go c.renewLoop(leaseDuration)
	return c, nil
}

// loginAppRole authenticates with Vault AppRole and stores the resulting token.
// Returns the token lease duration.
func (c *Client) loginAppRole(ctx context.Context) (time.Duration, error) {
	data, err := json.Marshal(map[string]string{
		"role_id":   c.roleID,
		"secret_id": c.secretID,
	})
	if err != nil {
		return 0, fmt.Errorf("marshaling login body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.addr+"/v1/auth/approle/login", strings.NewReader(string(data)))
	if err != nil {
		return 0, fmt.Errorf("building login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decoding login response: %w", err)
	}
	if result.Auth.ClientToken == "" {
		return 0, fmt.Errorf("empty token in login response")
	}

	c.mu.Lock()
	c.token = result.Auth.ClientToken
	c.mu.Unlock()

	return time.Duration(result.Auth.LeaseDuration) * time.Second, nil
}

// renewToken calls auth/token/renew-self and returns the new lease duration.
func (c *Client) renewToken(ctx context.Context) (time.Duration, error) {
	resp, err := c.doRequest(ctx, http.MethodPost, "auth/token/renew-self", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Auth struct {
			LeaseDuration int `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decoding renew response: %w", err)
	}
	return time.Duration(result.Auth.LeaseDuration) * time.Second, nil
}

// renewLoop runs as a background goroutine: renews the token at 2/3 of lease duration,
// falling back to a full re-login if renewal fails.
func (c *Client) renewLoop(leaseDuration time.Duration) {
	for {
		sleep := leaseDuration * 2 / 3
		if sleep < 30*time.Second {
			sleep = 30 * time.Second
		}
		time.Sleep(sleep)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		newDuration, err := c.renewToken(ctx)
		cancel()

		if err == nil {
			log.Printf("[vault] token renewed (lease: %s)", newDuration)
			leaseDuration = newDuration
			continue
		}

		log.Printf("[vault] token renewal failed (%v), re-authenticating...", err)
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		leaseDuration, err = c.loginAppRole(ctx)
		cancel()
		if err != nil {
			log.Printf("[vault] AppRole re-authentication failed: %v — retrying in 1 minute", err)
			leaseDuration = time.Minute
		} else {
			log.Printf("[vault] AppRole re-authentication successful (lease: %s)", leaseDuration)
		}
	}
}

func (c *Client) doRequest(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling body: %w", err)
		}
		bodyReader = strings.NewReader(string(data))
	}

	req, err := http.NewRequestWithContext(ctx, method, c.addr+"/v1/"+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	c.mu.RLock()
	req.Header.Set("X-Vault-Token", c.token)
	c.mu.RUnlock()
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.http.Do(req)
}

// AuthBackendExists checks if a Vault auth backend is already enabled at the given path.
func (c *Client) AuthBackendExists(ctx context.Context, path string) (bool, error) {
	resp, err := c.doRequest(ctx, http.MethodGet, "sys/auth", nil)
	if err != nil {
		return false, fmt.Errorf("listing auth backends: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, nil
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("decoding response: %w", err)
	}

	key := strings.TrimRight(path, "/") + "/"
	_, exists := result[key]
	return exists, nil
}

// EnableKubernetesAuth enables the Kubernetes auth method at the given path.
func (c *Client) EnableKubernetesAuth(ctx context.Context, path string) error {
	body := map[string]string{"type": "kubernetes"}
	resp, err := c.doRequest(ctx, http.MethodPost, "sys/auth/"+path, body)
	if err != nil {
		return fmt.Errorf("enabling auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("enabling auth: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// DisableAuth disables a Vault auth backend at the given path.
func (c *Client) DisableAuth(ctx context.Context, path string) error {
	resp, err := c.doRequest(ctx, http.MethodDelete, "sys/auth/"+path, nil)
	if err != nil {
		return fmt.Errorf("disabling auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("disabling auth: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// ConfigureKubernetesAuth configures the Kubernetes auth backend with the cluster details.
func (c *Client) ConfigureKubernetesAuth(ctx context.Context, path, host, caCert, reviewerJWT string) error {
	body := map[string]interface{}{
		"kubernetes_host":        host,
		"kubernetes_ca_cert":     caCert,
		"token_reviewer_jwt":     reviewerJWT,
		"disable_iss_validation": true,
	}
	resp, err := c.doRequest(ctx, http.MethodPost, "auth/"+path+"/config", body)
	if err != nil {
		return fmt.Errorf("configuring auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("configuring auth: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// CreateCertManagerRole creates the cert-manager role in the Kubernetes auth backend.
func (c *Client) CreateCertManagerRole(ctx context.Context, path string) error {
	body := map[string]interface{}{
		"bound_service_account_names":      []string{"vault-webhook"},
		"bound_service_account_namespaces": []string{"vault-system"},
		"policies":                         []string{"cert-manager"},
		"ttl":                              "1h",
	}
	resp, err := c.doRequest(ctx, http.MethodPost, "auth/"+path+"/role/cert-manager", body)
	if err != nil {
		return fmt.Errorf("creating role: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("creating role: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// SetupVClusterAuth orchestrates the full Vault Kubernetes auth setup for a vcluster.
// It enables the backend (idempotent), configures it with the cluster CA and reviewer JWT,
// and creates the cert-manager role.
func (c *Client) SetupVClusterAuth(ctx context.Context, name, env, apiHost, caCert, reviewerJWT string) error {
	path := "kubernetes-vcluster-" + name + "-" + env
	host := apiHost

	exists, err := c.AuthBackendExists(ctx, path)
	if err != nil {
		return fmt.Errorf("checking auth backend: %w", err)
	}
	if !exists {
		if err := c.EnableKubernetesAuth(ctx, path); err != nil {
			return fmt.Errorf("enabling auth backend: %w", err)
		}
	}

	if err := c.ConfigureKubernetesAuth(ctx, path, host, caCert, reviewerJWT); err != nil {
		return fmt.Errorf("configuring auth backend: %w", err)
	}

	if err := c.CreateCertManagerRole(ctx, path); err != nil {
		return fmt.Errorf("creating cert-manager role: %w", err)
	}

	return nil
}
