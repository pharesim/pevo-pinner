package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

const pinataBaseURL = "https://api.pinata.cloud"

// PinataBackend implements IPFSBackend using the Pinata REST API.
type PinataBackend struct {
	apiKey    string
	secretKey string
	client    *http.Client
}

// NewPinataBackend creates a Pinata-backed pinner.
func NewPinataBackend(apiKey, secretKey string) *PinataBackend {
	return &PinataBackend{
		apiKey:    apiKey,
		secretKey: secretKey,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (p *PinataBackend) doRequest(ctx context.Context, method, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("pinata_api_key", p.apiKey)
	req.Header.Set("pinata_secret_api_key", p.secretKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return p.client.Do(req)
}

func (p *PinataBackend) Pin(ctx context.Context, cid string) error {
	if err := ValidateCID(cid); err != nil {
		return err
	}
	payload := map[string]string{"hashToPin": cid}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling pin request: %w", err)
	}

	resp, err := p.doRequest(ctx, http.MethodPost, pinataBaseURL+"/pinning/pinByHash", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("pinata pin request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pinata pin failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	log.Printf("[pinata] pinned %s", cid)
	return nil
}

func (p *PinataBackend) Unpin(ctx context.Context, cid string) error {
	if err := ValidateCID(cid); err != nil {
		return err
	}
	// Defense-in-depth: escape the CID as a URL path segment. ValidateCID's regex
	// already excludes path-delimiting characters, so this is additive — it stops
	// the validator from being load-bearing for URL safety if the regex is ever
	// relaxed.
	resp, err := p.doRequest(ctx, http.MethodDelete, pinataBaseURL+"/pinning/unpin/"+url.PathEscape(cid), nil)
	if err != nil {
		return fmt.Errorf("pinata unpin request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pinata unpin failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	log.Printf("[pinata] unpinned %s", cid)
	return nil
}

func (p *PinataBackend) IsPinned(ctx context.Context, cid string) (bool, error) {
	if err := ValidateCID(cid); err != nil {
		return false, err
	}
	// Defense-in-depth: escape the CID as a URL query value. See the rationale on
	// Unpin above; the validator already narrows the character class so this is
	// purely additive at the URL-construction layer.
	reqURL := fmt.Sprintf("%s/data/pinList?status=pinned&hashContains=%s", pinataBaseURL, url.QueryEscape(cid))
	resp, err := p.doRequest(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, fmt.Errorf("pinata pin check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("pinata pin check failed (status %d)", resp.StatusCode)
	}

	var result struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("decoding pinata response: %w", err)
	}

	return result.Count > 0, nil
}

func (p *PinataBackend) PinnedCIDs(ctx context.Context) ([]string, error) {
	var allCIDs []string
	offset := 0
	pageSize := 1000

	for {
		url := fmt.Sprintf("%s/data/pinList?status=pinned&pageLimit=%d&pageOffset=%d", pinataBaseURL, pageSize, offset)
		resp, err := p.doRequest(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("pinata list request: %w", err)
		}

		var result struct {
			Count int `json:"count"`
			Rows  []struct {
				IPFSPinHash string `json:"ipfs_pin_hash"`
			} `json:"rows"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decoding pinata list: %w", err)
		}
		resp.Body.Close()

		for _, row := range result.Rows {
			allCIDs = append(allCIDs, row.IPFSPinHash)
		}

		if len(result.Rows) < pageSize {
			break
		}
		offset += pageSize
	}

	return allCIDs, nil
}

// Drain is a no-op for the Pinata backend: every Pin/Unpin/IsPinned call is a
// standalone HTTP round-trip that already honors its own context. There is no
// long-lived blockstore write to drain.
func (p *PinataBackend) Drain(_ context.Context) error {
	return nil
}

func (p *PinataBackend) Close() error {
	return nil
}
