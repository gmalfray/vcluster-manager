package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gmalfray/vcluster-manager/internal/models"
)

type releaseResponse struct {
	TagName     string `json:"tag_name"`
	PublishedAt string `json:"published_at"`
}

// ReleaseClient fetches release info from GitHub with in-memory caching.
type ReleaseClient struct {
	httpClient *http.Client

	mu       sync.RWMutex
	cached   *models.ReleaseInfo
	cachedAt time.Time
	cacheTTL time.Duration

	k8sMu       sync.RWMutex
	k8sCached   []string
	k8sCachedAt time.Time

	argoMu          sync.RWMutex
	argoCached      *models.ReleaseInfo
	argoCachedAt    time.Time
	argoVerMu       sync.RWMutex
	argoVerCached   []string
	argoVerCachedAt time.Time
}

func NewReleaseClient() *ReleaseClient {
	return &ReleaseClient{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		cacheTTL:   1 * time.Hour,
	}
}

// GetLatestVClusterRelease returns the latest vcluster release tag from GitHub.
func (c *ReleaseClient) GetLatestVClusterRelease() (*models.ReleaseInfo, error) {
	c.mu.RLock()
	if c.cached != nil && time.Since(c.cachedAt) < c.cacheTTL {
		info := *c.cached
		c.mu.RUnlock()
		return &info, nil
	}
	c.mu.RUnlock()

	req, err := http.NewRequest("GET", "https://api.github.com/repos/loft-sh/vcluster/releases/latest", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release releaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	// Parse and format date
	published := release.PublishedAt
	if t, err := time.Parse(time.RFC3339, release.PublishedAt); err == nil {
		published = t.Format("2006-01-02")
	}

	info := &models.ReleaseInfo{
		Tag:         release.TagName,
		PublishedAt: published,
	}

	c.mu.Lock()
	c.cached = info
	c.cachedAt = time.Now()
	c.mu.Unlock()

	return info, nil
}

// GetLatestArgoCDRelease returns the latest ArgoCD release tag from GitHub.
func (c *ReleaseClient) GetLatestArgoCDRelease() (*models.ReleaseInfo, error) {
	c.argoMu.RLock()
	if c.argoCached != nil && time.Since(c.argoCachedAt) < c.cacheTTL {
		info := *c.argoCached
		c.argoMu.RUnlock()
		return &info, nil
	}
	c.argoMu.RUnlock()

	req, err := http.NewRequest("GET", "https://api.github.com/repos/argoproj/argo-cd/releases/latest", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release releaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	published := release.PublishedAt
	if t, err := time.Parse(time.RFC3339, release.PublishedAt); err == nil {
		published = t.Format("2006-01-02")
	}

	info := &models.ReleaseInfo{
		Tag:         release.TagName,
		PublishedAt: published,
	}

	c.argoMu.Lock()
	c.argoCached = info
	c.argoCachedAt = time.Now()
	c.argoMu.Unlock()

	return info, nil
}

// GetAvailableArgoCDVersions returns recent ArgoCD release tags (latest patch per minor), sorted descending.
func (c *ReleaseClient) GetAvailableArgoCDVersions() ([]string, error) {
	c.argoVerMu.RLock()
	if c.argoVerCached != nil && time.Since(c.argoVerCachedAt) < c.cacheTTL {
		result := make([]string, len(c.argoVerCached))
		copy(result, c.argoVerCached)
		c.argoVerMu.RUnlock()
		return result, nil
	}
	c.argoVerMu.RUnlock()

	req, err := http.NewRequest("GET", "https://api.github.com/repos/argoproj/argo-cd/releases?per_page=50", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var releases []releaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	// Keep latest patch per minor, skip pre-releases (tags with -rc, -alpha, etc.)
	latestPatch := make(map[string]string) // "2.13" -> "v2.13.3"
	for _, r := range releases {
		tag := r.TagName
		if !strings.HasPrefix(tag, "v") {
			continue
		}
		ver := strings.TrimPrefix(tag, "v")
		if strings.ContainsAny(ver, "-+") {
			continue
		}
		parts := strings.SplitN(ver, ".", 3)
		if len(parts) != 3 {
			continue
		}
		minor := parts[0] + "." + parts[1]
		if existing, ok := latestPatch[minor]; !ok || compareVersions(ver, strings.TrimPrefix(existing, "v")) > 0 {
			latestPatch[minor] = tag
		}
	}

	var versions []string
	for _, v := range latestPatch {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool {
		return compareVersions(strings.TrimPrefix(versions[i], "v"), strings.TrimPrefix(versions[j], "v")) > 0
	})

	c.argoVerMu.Lock()
	c.argoVerCached = versions
	c.argoVerCachedAt = time.Now()
	c.argoVerMu.Unlock()

	return versions, nil
}

type ghcrTokenResponse struct {
	Token string `json:"token"`
}

type ghcrTagsResponse struct {
	Tags []string `json:"tags"`
}

// GetAvailableK8sVersions returns K8s versions (latest patch per minor), sorted descending.
// Uses the GHCR registry for loft-sh/kubernetes (the actual images used by vcluster).
func (c *ReleaseClient) GetAvailableK8sVersions() ([]string, error) {
	c.k8sMu.RLock()
	if c.k8sCached != nil && time.Since(c.k8sCachedAt) < c.cacheTTL {
		result := make([]string, len(c.k8sCached))
		copy(result, c.k8sCached)
		c.k8sMu.RUnlock()
		return result, nil
	}
	c.k8sMu.RUnlock()

	// Get anonymous token for GHCR
	tokenReq, err := http.NewRequest("GET", "https://ghcr.io/token?scope=repository:loft-sh/kubernetes:pull", nil)
	if err != nil {
		return nil, fmt.Errorf("creating token request: %w", err)
	}
	tokenResp, err := c.httpClient.Do(tokenReq)
	if err != nil {
		return nil, fmt.Errorf("fetching GHCR token: %w", err)
	}
	defer tokenResp.Body.Close()

	var tokenData ghcrTokenResponse
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenData); err != nil {
		return nil, fmt.Errorf("decoding token: %w", err)
	}

	// Fetch tags from GHCR
	tagsReq, err := http.NewRequest("GET", "https://ghcr.io/v2/loft-sh/kubernetes/tags/list?n=1000", nil)
	if err != nil {
		return nil, fmt.Errorf("creating tags request: %w", err)
	}
	tagsReq.Header.Set("Authorization", "Bearer "+tokenData.Token)

	tagsResp, err := c.httpClient.Do(tagsReq)
	if err != nil {
		return nil, fmt.Errorf("fetching tags: %w", err)
	}
	defer tagsResp.Body.Close()

	var tagsData ghcrTagsResponse
	if err := json.NewDecoder(tagsResp.Body).Decode(&tagsData); err != nil {
		return nil, fmt.Errorf("decoding tags: %w", err)
	}

	// Filter: only stable releases (vX.Y.Z), keep latest patch per minor
	// Keep the "v" prefix since GHCR images use it (ghcr.io/loft-sh/kubernetes:v1.32.8)
	latestPatch := make(map[string]string) // "1.32" -> "v1.32.3"
	for _, tag := range tagsData.Tags {
		if !strings.HasPrefix(tag, "v") {
			continue
		}
		ver := strings.TrimPrefix(tag, "v")
		if strings.ContainsAny(ver, "-+") {
			continue
		}
		parts := strings.SplitN(ver, ".", 3)
		if len(parts) != 3 {
			continue
		}
		minor := parts[0] + "." + parts[1]
		if existing, ok := latestPatch[minor]; !ok || compareVersions(ver, strings.TrimPrefix(existing, "v")) > 0 {
			latestPatch[minor] = tag // keep "v" prefix
		}
	}

	// Collect and sort descending
	var versions []string
	for _, v := range latestPatch {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool {
		return compareVersions(strings.TrimPrefix(versions[i], "v"), strings.TrimPrefix(versions[j], "v")) > 0
	})

	c.k8sMu.Lock()
	c.k8sCached = versions
	c.k8sCachedAt = time.Now()
	c.k8sMu.Unlock()

	return versions, nil
}

// compareVersions compares two semver strings (without v prefix). Returns >0 if a > b.
func compareVersions(a, b string) int {
	pa := strings.SplitN(a, ".", 3)
	pb := strings.SplitN(b, ".", 3)
	for i := 0; i < 3; i++ {
		var na, nb int
		if i < len(pa) {
			na, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			nb, _ = strconv.Atoi(pb[i])
		}
		if na != nb {
			return na - nb
		}
	}
	return 0
}
