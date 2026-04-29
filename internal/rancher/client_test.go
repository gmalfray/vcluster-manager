package rancher

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestClient creates a Client pointed at the given test server.
func newTestClient(srv *httptest.Server) *Client {
	return NewClient(srv.URL, "test-token")
}

// --- extractManifestURL ---

func TestExtractManifestURL_ManifestURL(t *testing.T) {
	tok := registrationTokenResponse{ManifestURL: "https://rancher/manifest.yaml"}
	if got := extractManifestURL(tok); got != "https://rancher/manifest.yaml" {
		t.Errorf("want manifestUrl, got %q", got)
	}
}

func TestExtractManifestURL_InsecureFallback(t *testing.T) {
	tok := registrationTokenResponse{InsecureManifestURL: "http://rancher/manifest.yaml"}
	if got := extractManifestURL(tok); got != "http://rancher/manifest.yaml" {
		t.Errorf("want insecureManifestUrl, got %q", got)
	}
}

func TestExtractManifestURL_ParsedFromCommand(t *testing.T) {
	tok := registrationTokenResponse{
		Command: "kubectl apply -f https://rancher/from-cmd.yaml",
	}
	if got := extractManifestURL(tok); got != "https://rancher/from-cmd.yaml" {
		t.Errorf("want URL from command, got %q", got)
	}
}

func TestExtractManifestURL_Empty(t *testing.T) {
	tok := registrationTokenResponse{}
	if got := extractManifestURL(tok); got != "" {
		t.Errorf("want empty string, got %q", got)
	}
}

func TestExtractManifestURL_CommandWithoutFlag(t *testing.T) {
	tok := registrationTokenResponse{Command: "some random command without -f"}
	if got := extractManifestURL(tok); got != "" {
		t.Errorf("want empty when no -f flag, got %q", got)
	}
}

// --- FindClusterByName ---

func TestFindClusterByName_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing or wrong Authorization header")
		}
		json.NewEncoder(w).Encode(clusterListResponse{
			Data: []clusterResponse{
				{ID: "c-abc123", Name: "vcluster-myapp", State: "active"},
			},
		})
	}))
	defer srv.Close()

	info, found, err := newTestClient(srv).FindClusterByName("myapp")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("want found=true")
	}
	if info.ID != "c-abc123" || info.State != "active" {
		t.Errorf("unexpected cluster info: %+v", info)
	}
}

func TestFindClusterByName_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(clusterListResponse{Data: []clusterResponse{}})
	}))
	defer srv.Close()

	_, found, err := newTestClient(srv).FindClusterByName("doesnotexist")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("want found=false for empty list")
	}
}

func TestFindClusterByName_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, _, err := newTestClient(srv).FindClusterByName("myapp")
	if err == nil {
		t.Fatal("want error for HTTP 401")
	}
}

// --- DeleteCluster ---

func TestDeleteCluster_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("want DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestClient(srv).DeleteCluster("c-abc123"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteCluster_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	err := newTestClient(srv).DeleteCluster("c-abc123")
	if err == nil {
		t.Fatal("want error for HTTP 403")
	}
}

// --- DownloadManifest ---

func TestDownloadManifest_OK(t *testing.T) {
	want := []byte("apiVersion: v1\nkind: Namespace")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(want)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).DownloadManifest(srv.URL + "/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestDownloadManifest_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := newTestClient(srv).DownloadManifest(srv.URL + "/missing.yaml")
	if err == nil {
		t.Fatal("want error for HTTP 404")
	}
}

// --- NewClient ---

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	c := NewClient("https://rancher.example.com/", "tok")
	if c.URL() != "https://rancher.example.com" {
		t.Errorf("want trailing slash trimmed, got %q", c.URL())
	}
}
