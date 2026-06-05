// Package workspaceclient provides S2S HTTP clients for the workspace service.
package workspaceclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/CoverOnes/payment/internal/service"
	"github.com/google/uuid"
)

// rosterResponseBody is the JSON shape of GET /internal/v1/contracts/:id/parties.
// The workspace service wraps data in {"data": [...]} per its httpx.OK envelope.
type rosterResponseBody struct {
	Data []rosterEntryJSON `json:"data"`
}

// rosterEntryJSON is a single party entry in the workspace roster response.
type rosterEntryJSON struct {
	VendorUserID uuid.UUID `json:"vendorUserId"`
	ShareBps     int       `json:"shareBps"`
}

const (
	// maxRosterBodyBytes caps the response body to prevent DoS (backend-security-design).
	maxRosterBodyBytes = 64 * 1024 // 64 KiB — plenty for any realistic roster

	// rosterRequestTimeout is the per-call deadline for the workspace S2S request.
	rosterRequestTimeout = 10 * time.Second
)

// HTTPRosterClient fetches the frozen ACTIVE-party roster from the workspace service
// via the S2S internal endpoint: GET /internal/v1/contracts/:id/parties.
// Credentials are sent in X-Service-Token and X-Service-Id headers (never in the URL).
type HTTPRosterClient struct {
	httpClient   *http.Client
	baseURL      string
	serviceID    string
	serviceToken string
}

// NewHTTPRosterClient returns an HTTPRosterClient.
// baseURL is the workspace service base URL (e.g. "http://workspace:8081").
// serviceToken is sent in X-Service-Token — NEVER interpolated into the URL.
func NewHTTPRosterClient(baseURL, serviceID, serviceToken string) *HTTPRosterClient {
	return &HTTPRosterClient{
		httpClient:   &http.Client{Timeout: rosterRequestTimeout},
		baseURL:      baseURL,
		serviceID:    serviceID,
		serviceToken: serviceToken,
	}
}

// GetPartyRoster calls GET <workspace>/internal/v1/contracts/:id/parties
// and returns the frozen ACTIVE-party roster as []service.RosterEntry.
// Returns an error if the workspace endpoint is unreachable or returns non-200.
func (c *HTTPRosterClient) GetPartyRoster(ctx context.Context, contractID uuid.UUID) ([]service.RosterEntry, error) {
	url := fmt.Sprintf("%s/internal/v1/contracts/%s/parties", c.baseURL, contractID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build workspace roster request: %w", err)
	}

	// Credentials in headers — NEVER in the URL (backend-security-design §4.2 / go-security).
	req.Header.Set("X-Service-Id", "payment")
	req.Header.Set("X-Service-Token", c.serviceToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("workspace roster GET: %w", err)
	}

	defer resp.Body.Close() //nolint:errcheck // best-effort close on HTTP response body

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("workspace roster returned status %d for contract %s", resp.StatusCode, contractID)
	}

	// Body limit — DoS prevention (backend-security-design).
	limited := io.LimitReader(resp.Body, maxRosterBodyBytes)

	var body rosterResponseBody
	if decErr := json.NewDecoder(limited).Decode(&body); decErr != nil {
		return nil, fmt.Errorf("decode workspace roster response: %w", decErr)
	}

	entries := make([]service.RosterEntry, len(body.Data))
	for i, d := range body.Data {
		entries[i] = service.RosterEntry{
			VendorUserID: d.VendorUserID,
			ShareBps:     d.ShareBps,
		}
	}

	return entries, nil
}
