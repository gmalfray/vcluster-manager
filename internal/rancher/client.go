package rancher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// errForbidden is returned by createRegistrationToken when Rancher responds with 403.
// This happens transiently after cluster creation while Rancher propagates the Cluster Owner role.
var errForbidden = errors.New("forbidden")

// Client is a Rancher API v3 client for importing/removing clusters.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient creates a Rancher API client.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// URL returns the configured Rancher URL.
func (c *Client) URL() string {
	return c.baseURL
}

// clusterResponse represents a Rancher cluster object from the API.
type clusterResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"` // pending, waiting, provisioning, active, etc.
}

// clusterListResponse represents the /v3/clusters list response.
type clusterListResponse struct {
	Data []clusterResponse `json:"data"`
}

// registrationTokenResponse represents a cluster registration token.
type registrationTokenResponse struct {
	ID                  string `json:"id"`
	ManifestURL         string `json:"manifestUrl"`
	InsecureManifestURL string `json:"insecureManifestUrl"`
	Command             string `json:"command"`
}

// registrationTokenListResponse represents the list of registration tokens.
type registrationTokenListResponse struct {
	Data []registrationTokenResponse `json:"data"`
}

// ImportCluster creates an imported cluster in Rancher and returns the clusterID and manifest URL.
// The manifest URL points to the YAML that must be applied inside the vcluster to register it.
func (c *Client) ImportCluster(name string) (clusterID, manifestURL string, err error) {
	clusterName := "vcluster-" + name

	// 1. Create the imported cluster
	body := fmt.Sprintf(`{"name":%q,"description":"vCluster %s managed by vcluster-manager"}`, clusterName, name)
	req, err := http.NewRequest("POST", c.baseURL+"/v3/clusters", strings.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("creating cluster: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("creating cluster: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var cluster clusterResponse
	if err := json.NewDecoder(resp.Body).Decode(&cluster); err != nil {
		return "", "", fmt.Errorf("decoding cluster response: %w", err)
	}
	clusterID = cluster.ID

	// 2. Create a registration token via POST.
	// Rancher assigns the Cluster Owner role to the creating user asynchronously after cluster
	// creation. Until that propagation completes, POST /v3/clusterregistrationtokens returns 403.
	// We retry up to 10 times with 5s between attempts (up to ~50s total wait).
	const (
		maxAttempts  = 10
		retryDelay   = 5 * time.Second
		initialDelay = 2 * time.Second
	)
	time.Sleep(initialDelay)

	var tokenURL string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		tokenURL, err = c.createRegistrationToken(clusterID)
		if err == nil {
			break
		}
		if !errors.Is(err, errForbidden) {
			return clusterID, "", fmt.Errorf("creating registration token for cluster %s: %w", clusterID, err)
		}
		if attempt < maxAttempts {
			log.Printf("Rancher: 403 on token creation for %s (attempt %d/%d, role not yet propagated), retrying in %s...", clusterID, attempt, maxAttempts, retryDelay)
			time.Sleep(retryDelay)
		}
	}
	if err != nil {
		return clusterID, "", fmt.Errorf("creating registration token for cluster %s after %d attempts: %w", clusterID, maxAttempts, err)
	}
	if tokenURL == "" {
		return clusterID, "", fmt.Errorf("cluster %s created but registration token returned no manifest URL", clusterID)
	}
	return clusterID, tokenURL, nil
}

// createRegistrationToken explicitly creates a Rancher cluster registration token and returns its manifestUrl.
// This is more reliable than waiting for the auto-created token which Rancher generates asynchronously.
func (c *Client) createRegistrationToken(clusterID string) (string, error) {
	body := fmt.Sprintf(`{"type":"clusterRegistrationToken","clusterId":%q}`, clusterID)
	req, err := http.NewRequest("POST", c.baseURL+"/v3/clusterregistrationtokens", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("Rancher: 403 creating token for %s: %s", clusterID, string(respBody))
		return "", errForbidden
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	log.Printf("Rancher: createRegistrationToken response for %s: %s", clusterID, string(respBody))

	var token registrationTokenResponse
	if err := json.Unmarshal(respBody, &token); err != nil {
		return "", err
	}

	// Try all available URL fields (manifestUrl, insecureManifestUrl, or parsed from command).
	if url := extractManifestURL(token); url != "" {
		return url, nil
	}

	// manifestUrl is populated asynchronously by Rancher after token creation.
	// Poll the token by ID until the URL appears (max 30s).
	if token.ID == "" {
		return "", fmt.Errorf("token created but no manifest URL and no ID to poll")
	}
	log.Printf("Rancher: manifest URL not yet ready for token %s, polling...", token.ID)
	for i := 0; i < 10; i++ {
		time.Sleep(3 * time.Second)
		url, err := c.getRegistrationTokenByID(token.ID)
		if err != nil {
			log.Printf("Rancher: error polling token %s: %v", token.ID, err)
			continue
		}
		if url != "" {
			return url, nil
		}
		log.Printf("Rancher: manifest URL still empty for token %s (attempt %d/10)", token.ID, i+1)
	}
	return "", fmt.Errorf("token %s created but manifest URL never populated after 30s", token.ID)
}

// getRegistrationTokenByID fetches a registration token by its ID and returns its manifest URL.
func (c *Client) getRegistrationTokenByID(tokenID string) (string, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/v3/clusterregistrationtokens/"+tokenID, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var token registrationTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return "", err
	}
	return extractManifestURL(token), nil
}

// extractManifestURL returns the manifest URL from a token, trying all available fields.
// The manifestUrl may be empty but command always contains "kubectl apply -f {url}".
func extractManifestURL(t registrationTokenResponse) string {
	if t.ManifestURL != "" {
		return t.ManifestURL
	}
	if t.InsecureManifestURL != "" {
		return t.InsecureManifestURL
	}
	// Parse from command: "kubectl apply -f <url>"
	if t.Command != "" {
		parts := strings.Fields(t.Command)
		for i, p := range parts {
			if p == "-f" && i+1 < len(parts) {
				return parts[i+1]
			}
		}
	}
	return ""
}

// getManifestURL fetches the cluster registration token manifest URL.
// Tries both the cluster sub-resource endpoint and the global endpoint as fallback.
func (c *Client) getManifestURL(clusterID string) (string, error) {
	// Primary: cluster sub-resource
	url, err := c.getManifestURLFromEndpoint(c.baseURL+"/v3/clusters/"+clusterID+"/clusterregistrationtokens", clusterID)
	if err == nil && url != "" {
		return url, nil
	}
	if err != nil {
		log.Printf("Rancher: cluster subresource endpoint error for %s: %v", clusterID, err)
	}
	// Fallback: global endpoint filtered by clusterId
	url, err = c.getManifestURLFromEndpoint(c.baseURL+"/v3/clusterregistrationtokens?clusterId="+clusterID, clusterID)
	if err != nil {
		return "", err
	}
	return url, nil
}

func (c *Client) getManifestURLFromEndpoint(endpoint, clusterID string) (string, error) {
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	log.Printf("Rancher: tokens response from %s: %s", endpoint, string(body))

	var tokens registrationTokenListResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return "", err
	}

	for _, t := range tokens.Data {
		if url := extractManifestURL(t); url != "" {
			return url, nil
		}
	}
	return "", nil
}

// DeleteCluster removes a cluster from Rancher by its cluster ID.
func (c *Client) DeleteCluster(clusterID string) error {
	req, err := http.NewRequest("DELETE", c.baseURL+"/v3/clusters/"+clusterID, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("deleting cluster: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("deleting cluster: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// ClusterInfo holds the result of a cluster lookup.
type ClusterInfo struct {
	ID    string
	State string // pending, waiting, provisioning, active, etc.
}

// FindClusterByName searches for a cluster by name (vcluster-{name} pattern).
// Returns the cluster info if found.
func (c *Client) FindClusterByName(name string) (info ClusterInfo, found bool, err error) {
	clusterName := "vcluster-" + name

	req, err := http.NewRequest("GET", c.baseURL+"/v3/clusters?name="+clusterName, nil)
	if err != nil {
		return ClusterInfo{}, false, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return ClusterInfo{}, false, fmt.Errorf("searching cluster: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return ClusterInfo{}, false, fmt.Errorf("searching cluster: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var list clusterListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return ClusterInfo{}, false, fmt.Errorf("decoding response: %w", err)
	}

	for _, cl := range list.Data {
		if cl.Name == clusterName {
			return ClusterInfo{ID: cl.ID, State: cl.State}, true, nil
		}
	}
	return ClusterInfo{}, false, nil
}

// WaitForClusterActive polls the Rancher API until the cluster state is "active" or timeout.
func (c *Client) WaitForClusterActive(clusterID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest("GET", c.baseURL+"/v3/clusters/"+clusterID, nil)
		if err != nil {
			return fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)

		resp, err := c.http.Do(req)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		var cluster clusterResponse
		json.NewDecoder(resp.Body).Decode(&cluster)
		resp.Body.Close()

		if cluster.State == "active" {
			return nil
		}

		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("cluster %s did not become active within %v", clusterID, timeout)
}

// DownloadManifest fetches the registration manifest YAML from the given URL.
func (c *Client) DownloadManifest(manifestURL string) ([]byte, error) {
	req, err := http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("downloading manifest: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return io.ReadAll(resp.Body)
}
