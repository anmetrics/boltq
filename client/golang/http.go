package boltq

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"
)

// newHTTPRequest creates a new HTTP request.
func newHTTPRequest(method, url string, body []byte) (*http.Request, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	return http.NewRequest(method, url, bodyReader)
}

// doHTTPRequest executes an HTTP request and returns the response body.
func doHTTPRequest(req *http.Request, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(data))
	}

	return data, nil
}
