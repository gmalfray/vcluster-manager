package keycloak

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Client wraps the Keycloak Admin REST API using client_credentials flow.
type Client struct {
	baseURL       string
	realm         string
	clientID      string
	clientSecret  string
	domainPreprod string // base domain for preprod ArgoCD URLs, e.g. "preprod.example.com"
	domainProd    string // base domain for prod ArgoCD URLs, e.g. "example.com"
	httpClient    *http.Client

	mu    sync.Mutex
	cache *tokenCache
}

type tokenCache struct {
	accessToken string
	expiry      time.Time
}

func NewClient(baseURL, realm, clientID, clientSecret, domainPreprod, domainProd string) *Client {
	return &Client{
		baseURL:       baseURL,
		realm:         realm,
		clientID:      clientID,
		clientSecret:  clientSecret,
		domainPreprod: domainPreprod,
		domainProd:    domainProd,
		httpClient:    &http.Client{Timeout: 5 * time.Second},
	}
}

// getToken returns a valid access token, refreshing it if expired.
func (c *Client) getToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Return cached token if still valid (with 30s margin)
	if c.cache != nil && time.Now().Add(30*time.Second).Before(c.cache.expiry) {
		return c.cache.accessToken, nil
	}

	data := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}

	resp, err := c.httpClient.PostForm(
		c.baseURL+"/auth/realms/"+c.realm+"/protocol/openid-connect/token",
		data,
	)
	if err != nil {
		return "", fmt.Errorf("keycloak token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("keycloak auth failed (HTTP %d): %s", resp.StatusCode, body)
	}

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding keycloak token response: %w", err)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("keycloak auth failed: %s", result.Error)
	}

	c.cache = &tokenCache{
		accessToken: result.AccessToken,
		expiry:      time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}

	return c.cache.accessToken, nil
}

// doRequest executes an authenticated HTTP request against the Keycloak Admin API.
func (c *Client) doRequest(method, path string, body io.Reader) (*http.Response, error) {
	token, err := c.getToken()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.httpClient.Do(req)
}

// ClientExists checks if an OIDC client already exists.
func (c *Client) ClientExists(clientID string) (bool, error) {
	resp, err := c.doRequest("GET",
		fmt.Sprintf("/auth/admin/realms/%s/clients?clientId=%s", c.realm, url.QueryEscape(clientID)),
		nil,
	)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	var clients []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&clients); err != nil {
		return false, err
	}
	return len(clients) > 0, nil
}

// CreateArgoClient creates an ArgoCD OIDC client in Keycloak.
func (c *Client) CreateArgoClient(clientID, argocdURL string) error {
	client := map[string]interface{}{
		"clientId":                  clientID,
		"name":                     clientID,
		"description":              fmt.Sprintf("ArgoCD OIDC client pour %s", argocdURL),
		"enabled":                  true,
		"publicClient":             true,
		"protocol":                 "openid-connect",
		"rootUrl":                  argocdURL,
		"baseUrl":                  "/applications",
		"redirectUris":             []string{argocdURL + "/auth/callback", "http://localhost:8085/auth/callback"},
		"webOrigins":               []string{argocdURL},
		"directAccessGrantsEnabled": true,
		"standardFlowEnabled":       true,
		"implicitFlowEnabled":       false,
		"serviceAccountsEnabled":    false,
		"frontchannelLogout":        true,
		"attributes":               map[string]string{"post.logout.redirect.uris": argocdURL + "/*"},
		"protocolMappers":          argocdProtocolMappers(),
		"defaultClientScopes":      []string{"profile", "roles", "groups", "basic", "email"},
		"optionalClientScopes":     []string{"address", "phone", "offline_access", "microprofile-jwt"},
	}

	body, err := json.Marshal(client)
	if err != nil {
		return err
	}

	resp, err := c.doRequest("POST",
		fmt.Sprintf("/auth/admin/realms/%s/clients", c.realm),
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 201 || resp.StatusCode == 409 {
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("create client failed (HTTP %d): %s", resp.StatusCode, string(respBody))
}

// DeleteClient deletes a Keycloak OIDC client by clientId.
func (c *Client) DeleteClient(clientID string) error {
	// First find the internal ID
	resp, err := c.doRequest("GET",
		fmt.Sprintf("/auth/admin/realms/%s/clients?clientId=%s", c.realm, url.QueryEscape(clientID)),
		nil,
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var clients []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&clients); err != nil {
		return err
	}
	if len(clients) == 0 {
		return nil // already gone
	}

	delResp, err := c.doRequest("DELETE",
		fmt.Sprintf("/auth/admin/realms/%s/clients/%s", c.realm, clients[0].ID),
		nil,
	)
	if err != nil {
		return err
	}
	defer delResp.Body.Close()

	if delResp.StatusCode >= 300 {
		return fmt.Errorf("delete client failed (HTTP %d)", delResp.StatusCode)
	}
	return nil
}

// CreateArgoCDClients creates OIDC clients for a vcluster based on scope ("preprod", "prod", or "both").
func (c *Client) CreateArgoCDClients(name, scope string) error {
	type clientConfig struct {
		clientID string
		url      string
	}
	var configs []clientConfig
	if scope == "preprod" || scope == "both" {
		configs = append(configs, clientConfig{
			clientID: fmt.Sprintf("argocd-k8s-%s-preprod", name),
			url:      fmt.Sprintf("https://argocd.%s.%s", name, c.domainPreprod),
		})
	}
	if scope == "prod" || scope == "both" {
		configs = append(configs, clientConfig{
			clientID: fmt.Sprintf("argocd-k8s-%s", name),
			url:      fmt.Sprintf("https://argocd.%s.%s", name, c.domainProd),
		})
	}

	for _, cfg := range configs {
		exists, err := c.ClientExists(cfg.clientID)
		if err != nil {
			return fmt.Errorf("checking client %s: %w", cfg.clientID, err)
		}
		if exists {
			continue
		}
		if err := c.CreateArgoClient(cfg.clientID, cfg.url); err != nil {
			return fmt.Errorf("creating client %s: %w", cfg.clientID, err)
		}
	}
	return nil
}

// DeleteArgoCDClients deletes both preprod and prod OIDC clients.
func (c *Client) DeleteArgoCDClients(name string) error {
	for _, clientID := range []string{
		fmt.Sprintf("argocd-k8s-%s-preprod", name),
		fmt.Sprintf("argocd-k8s-%s", name),
	} {
		if err := c.DeleteClient(clientID); err != nil {
			return fmt.Errorf("deleting client %s: %w", clientID, err)
		}
	}
	return nil
}

func argocdProtocolMappers() []map[string]interface{} {
	notes := []struct {
		name string
		note string
	}{
		{"Client ID", "client_id"},
		{"Client IP Address", "clientAddress"},
		{"Client Host", "clientHost"},
	}

	var mappers []map[string]interface{}
	for _, n := range notes {
		mappers = append(mappers, map[string]interface{}{
			"name":            n.name,
			"protocol":        "openid-connect",
			"protocolMapper":  "oidc-usersessionmodel-note-mapper",
			"consentRequired": false,
			"config": map[string]string{
				"user.session.note":         n.note,
				"id.token.claim":            "true",
				"introspection.token.claim": "true",
				"access.token.claim":        "true",
				"claim.name":               n.note,
				"jsonType.label":            "String",
			},
		})
	}
	return mappers
}
