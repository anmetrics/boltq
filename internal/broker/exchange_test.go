package broker

import "testing"

func TestTopicMatch(t *testing.T) {
	tests := []struct {
		routingKey string
		pattern    string
		expected   bool
	}{
		// Exact match
		{"order.created", "order.created", true},
		{"order.created", "order.updated", false},

		// Star wildcard - matches exactly one word
		{"order.created", "order.*", true},
		{"order.created.vn", "order.*", false},
		{"order.created", "*.created", true},
		{"user.created", "*.created", true},

		// Hash wildcard - matches zero or more words
		{"order.created", "order.#", true},
		{"order.created.vn", "order.#", true},
		{"order", "order.#", true},
		{"user.created", "order.#", false},

		// Hash at beginning
		{"a.b.c.created", "#.created", true},
		{"created", "#.created", true},

		// Mixed
		{"order.created.vn", "order.*.vn", true},
		{"order.updated.vn", "order.*.vn", true},
		{"order.created.us", "order.*.vn", false},

		// Hash in middle
		{"a.b.c.d", "a.#.d", true},
		{"a.d", "a.#.d", true},

		// Just hash
		{"anything.at.all", "#", true},
		{"single", "#", true},
	}

	for _, tt := range tests {
		result := topicMatch(tt.routingKey, tt.pattern)
		if result != tt.expected {
			t.Errorf("topicMatch(%q, %q) = %v, want %v", tt.routingKey, tt.pattern, result, tt.expected)
		}
	}
}

func TestHeadersMatch(t *testing.T) {
	msgHeaders := map[string]string{
		"format": "pdf",
		"type":   "report",
		"source": "web",
	}

	// Match all
	if !headersMatch(msgHeaders, map[string]string{"format": "pdf", "type": "report"}, true) {
		t.Error("expected all-match to succeed")
	}
	if headersMatch(msgHeaders, map[string]string{"format": "pdf", "type": "invoice"}, true) {
		t.Error("expected all-match to fail when one header differs")
	}

	// Match any
	if !headersMatch(msgHeaders, map[string]string{"format": "pdf", "type": "invoice"}, false) {
		t.Error("expected any-match to succeed when one header matches")
	}
	if headersMatch(msgHeaders, map[string]string{"format": "csv", "type": "invoice"}, false) {
		t.Error("expected any-match to fail when no headers match")
	}

	// Empty binding headers
	if !headersMatch(msgHeaders, map[string]string{}, true) {
		t.Error("expected match with empty binding headers")
	}
}

func TestExchangeRoute(t *testing.T) {
	// Direct exchange
	direct := &Exchange{Name: "direct-ex", Type: ExchangeDirect}
	direct.Bindings = []*Binding{
		{Queue: "q1", BindingKey: "order.created"},
		{Queue: "q2", BindingKey: "order.updated"},
	}
	queues := direct.route("order.created", nil)
	if len(queues) != 1 || queues[0] != "q1" {
		t.Errorf("direct route: expected [q1], got %v", queues)
	}

	// Fanout exchange
	fanout := &Exchange{Name: "fanout-ex", Type: ExchangeFanout}
	fanout.Bindings = []*Binding{
		{Queue: "q1"},
		{Queue: "q2"},
		{Queue: "q3"},
	}
	queues = fanout.route("anything", nil)
	if len(queues) != 3 {
		t.Errorf("fanout route: expected 3 queues, got %d", len(queues))
	}

	// Topic exchange
	topic := &Exchange{Name: "topic-ex", Type: ExchangeTopic}
	topic.Bindings = []*Binding{
		{Queue: "q1", BindingKey: "order.*"},
		{Queue: "q2", BindingKey: "user.#"},
		{Queue: "q3", BindingKey: "order.created"},
	}
	queues = topic.route("order.created", nil)
	if len(queues) != 2 { // q1 (order.*) and q3 (order.created)
		t.Errorf("topic route: expected 2 queues, got %v", queues)
	}

	// Headers exchange
	headers := &Exchange{Name: "headers-ex", Type: ExchangeHeaders}
	headers.Bindings = []*Binding{
		{Queue: "q1", Headers: map[string]string{"format": "pdf"}, MatchAll: true},
		{Queue: "q2", Headers: map[string]string{"format": "csv"}, MatchAll: true},
	}
	queues = headers.route("", map[string]string{"format": "pdf", "type": "report"})
	if len(queues) != 1 || queues[0] != "q1" {
		t.Errorf("headers route: expected [q1], got %v", queues)
	}
}

func TestExchangeDuplicateQueues(t *testing.T) {
	// Same queue bound with different keys should only appear once
	ex := &Exchange{Name: "test", Type: ExchangeTopic}
	ex.Bindings = []*Binding{
		{Queue: "q1", BindingKey: "order.*"},
		{Queue: "q1", BindingKey: "order.#"},
	}
	queues := ex.route("order.created", nil)
	if len(queues) != 1 {
		t.Errorf("expected 1 unique queue, got %d", len(queues))
	}
}
