package broker

import (
	"strings"
	"sync"
)

// ExchangeType identifies the routing strategy for an exchange.
type ExchangeType string

const (
	ExchangeDirect  ExchangeType = "direct"
	ExchangeFanout  ExchangeType = "fanout"
	ExchangeTopic   ExchangeType = "topic"
	ExchangeHeaders ExchangeType = "headers"
)

// Exchange routes messages to bound queues based on routing rules.
type Exchange struct {
	Name     string       `json:"name"`
	Type     ExchangeType `json:"type"`
	Durable  bool         `json:"durable"`
	Bindings []*Binding   `json:"bindings,omitempty"`
	mu       sync.RWMutex
}

// Binding connects an exchange to a queue with matching rules.
type Binding struct {
	Exchange   string            `json:"exchange"`
	Queue      string            `json:"queue"`
	BindingKey string            `json:"binding_key"`
	Headers    map[string]string `json:"headers,omitempty"`
	MatchAll   bool              `json:"match_all,omitempty"` // for headers exchange: true=all, false=any
}

// ExchangeState is the serializable form of an Exchange (for snapshots).
type ExchangeState struct {
	Name     string       `json:"name"`
	Type     ExchangeType `json:"type"`
	Durable  bool         `json:"durable"`
	Bindings []*Binding   `json:"bindings,omitempty"`
}

// route returns the list of queue names that match the given routing key and headers.
func (e *Exchange) route(routingKey string, headers map[string]string) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	seen := make(map[string]bool)
	var queues []string

	for _, b := range e.Bindings {
		if seen[b.Queue] {
			continue
		}
		matched := false
		switch e.Type {
		case ExchangeDirect:
			matched = b.BindingKey == routingKey
		case ExchangeFanout:
			matched = true
		case ExchangeTopic:
			matched = topicMatch(routingKey, b.BindingKey)
		case ExchangeHeaders:
			matched = headersMatch(headers, b.Headers, b.MatchAll)
		}
		if matched {
			seen[b.Queue] = true
			queues = append(queues, b.Queue)
		}
	}
	return queues
}

// topicMatch matches a routing key against a binding pattern with wildcards.
// Dot-separated words. '*' matches exactly one word. '#' matches zero or more words.
func topicMatch(routingKey, pattern string) bool {
	rParts := strings.Split(routingKey, ".")
	pParts := strings.Split(pattern, ".")
	return matchParts(rParts, 0, pParts, 0)
}

func matchParts(rParts []string, ri int, pParts []string, pi int) bool {
	for pi < len(pParts) {
		if pParts[pi] == "#" {
			// '#' at end matches everything remaining
			if pi == len(pParts)-1 {
				return true
			}
			// Try matching '#' to 0..N routing words
			for ri <= len(rParts) {
				if matchParts(rParts, ri, pParts, pi+1) {
					return true
				}
				ri++
			}
			return false
		}
		if ri >= len(rParts) {
			return false
		}
		if pParts[pi] != "*" && pParts[pi] != rParts[ri] {
			return false
		}
		ri++
		pi++
	}
	return ri == len(rParts)
}

// headersMatch checks if message headers match binding headers.
// If matchAll is true, all binding headers must be present and equal.
// If matchAll is false, any one match is sufficient.
func headersMatch(msgHeaders, bindingHeaders map[string]string, matchAll bool) bool {
	if len(bindingHeaders) == 0 {
		return true
	}
	if matchAll {
		for k, v := range bindingHeaders {
			if msgHeaders[k] != v {
				return false
			}
		}
		return true
	}
	// any match
	for k, v := range bindingHeaders {
		if msgHeaders[k] == v {
			return true
		}
	}
	return false
}
