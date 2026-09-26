package main

import (
	"context"
	"net/http"
	"time"
)

var defaultConnectivityURLs = []string{
	"https://www.cloudflare.com/cdn-cgi/trace",
	"https://www.google.com/generate_204",
}

const defaultConnectivityIPURL = "https://1.1.1.1/help"

func (a *app) classifyConnectivity(result checkResult) checkResult {
	if result.State != stateConnection || result.HTTPStatus != 0 || (result.ErrorCode != "timeout" && result.ErrorCode != "connection_error") {
		return result
	}
	if a.connectivityAvailable() {
		return result
	}
	result.State = "unknown"
	endpoint := a.connectivityIPURL
	if endpoint == "" {
		endpoint = defaultConnectivityIPURL
	}
	if probeConnectivity(endpoint) {
		result.ErrorCode = "dns_issue_possible"
		result.ErrorDetail = "IP control responded but hostname controls failed"
	} else {
		result.ErrorCode = "local_connectivity_unknown"
		result.ErrorDetail = "provider failure coincided with failed internet controls"
	}
	return result
}

// connectivityAvailable is called only after a provider transport failure.
// Either independent control responding is enough to establish that this host
// has a working route to the public internet. The requests carry no secrets.
func (a *app) connectivityAvailable() bool {
	urls := a.connectivityURLs
	if len(urls) == 0 {
		urls = defaultConnectivityURLs
	}
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	results := make(chan bool, len(urls))
	for _, endpoint := range urls {
		go func(endpoint string) {
			results <- probeConnectivityWithClient(client, endpoint)
		}(endpoint)
	}
	for range urls {
		if <-results {
			return true
		}
	}
	return false
}

func probeConnectivity(endpoint string) bool {
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return probeConnectivityWithClient(client, endpoint)
}

func probeConnectivityWithClient(client *http.Client, endpoint string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}
