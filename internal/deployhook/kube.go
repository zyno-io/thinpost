// Package deployhook implements the Helm hook steps that migrate the database
// schema before a control rollout and reopen admission after it. It reads only
// fixed, non-sensitive state, and every failure is reported as a fixed reason.
package deployhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// maxKubeResponse caps the pod list this client will read.
const maxKubeResponse = 4 << 20

// Pod is the subset of a Kubernetes pod the rollout check needs.
type Pod struct {
	Name     string
	Deleting bool
	// Phase is the pod phase, for example Running or Unknown.
	Phase string
	Ready bool
	// Images maps each container name to its spec image.
	Images map[string]string
}

// Kube lists pods. The only Kubernetes operation a hook needs is a read-only
// pod list; there is deliberately no way to change cluster state.
type Kube interface {
	ListPods(ctx context.Context, namespace, selector string) ([]Pod, error)
}

// Client is a minimal in-cluster REST client built on the standard library.
type Client struct {
	// BaseURL is https://host:port.
	BaseURL string
	// TokenPath is read on every request, because projected tokens rotate.
	TokenPath string
	HTTP      *http.Client
}

// NewInCluster builds a client from the service account directory (token and
// ca.crt) and the standard service environment variables.
func NewInCluster(dir string, getenv func(string) string) (*Client, error) {
	host, port := getenv("KUBERNETES_SERVICE_HOST"), getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("kubernetes service address is not configured")
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, errors.New("kubernetes CA is unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("kubernetes CA is invalid")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, MaxIdleConns: 1, IdleConnTimeout: 30 * time.Second}
	return &Client{BaseURL: "https://" + net.JoinHostPort(host, port), TokenPath: filepath.Join(dir, "token"), HTTP: &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

var (
	namespacePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	selectorPattern  = regexp.MustCompile(`^[A-Za-z0-9._/=,-]{1,253}$`)
)

type podList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []struct {
		Metadata struct {
			Name              string  `json:"name"`
			DeletionTimestamp *string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// ListPods lists at most 500 pods. Errors never include the response body.
func (c *Client) ListPods(ctx context.Context, namespace, selector string) ([]Pod, error) {
	if !namespacePattern.MatchString(namespace) || !selectorPattern.MatchString(selector) || c.HTTP == nil {
		return nil, errors.New("invalid pod query")
	}
	token, err := os.ReadFile(c.TokenPath)
	if err != nil || len(strings.TrimSpace(string(token))) == 0 {
		return nil, errors.New("kubernetes token is unavailable")
	}
	query := url.Values{"labelSelector": {selector}, "limit": {"500"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/namespaces/"+namespace+"/pods?"+query.Encode(), nil)
	if err != nil {
		return nil, errors.New("invalid pod query")
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Accept", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, errors.New("kubernetes API request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("kubernetes API returned an error status")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxKubeResponse+1))
	if err != nil || len(body) > maxKubeResponse {
		return nil, errors.New("kubernetes API response is unusable")
	}
	var list podList
	if err = json.Unmarshal(body, &list); err != nil || list.Metadata.Continue != "" {
		return nil, errors.New("kubernetes API response is unusable")
	}
	pods := make([]Pod, 0, len(list.Items))
	for _, item := range list.Items {
		pod := Pod{Name: item.Metadata.Name, Deleting: item.Metadata.DeletionTimestamp != nil, Phase: item.Status.Phase, Images: map[string]string{}}
		for _, container := range item.Spec.Containers {
			pod.Images[container.Name] = container.Image
		}
		for _, condition := range item.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				pod.Ready = true
			}
		}
		pods = append(pods, pod)
	}
	return pods, nil
}
