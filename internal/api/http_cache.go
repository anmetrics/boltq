package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/boltq/boltq/internal/cache"
)

// registerCacheRoutes registers all cache/KV store HTTP endpoints.
func (s *HTTPServer) registerCacheRoutes() {
	s.mux.HandleFunc("/cache/get", s.cors(s.auth(s.handleCacheGet)))
	s.mux.HandleFunc("/cache/set", s.cors(s.auth(s.handleCacheSet)))
	s.mux.HandleFunc("/cache/del", s.cors(s.auth(s.handleCacheDel)))
	s.mux.HandleFunc("/cache/keys", s.cors(s.auth(s.handleCacheKeys)))
	s.mux.HandleFunc("/cache/exists", s.cors(s.auth(s.handleCacheExists)))
	s.mux.HandleFunc("/cache/expire", s.cors(s.auth(s.handleCacheExpire)))
	s.mux.HandleFunc("/cache/incr", s.cors(s.auth(s.handleCacheIncr)))
	s.mux.HandleFunc("/cache/flush", s.cors(s.auth(s.handleCacheFlush)))
	s.mux.HandleFunc("/cache/stats", s.cors(s.auth(s.handleCacheStats)))
	s.mux.HandleFunc("/cache/entries", s.cors(s.auth(s.handleCacheEntries)))
}

// --- GET ---

func (s *HTTPServer) handleCacheGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cache == nil {
		writeError(w, http.StatusServiceUnavailable, "cache not enabled")
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	entry := s.cache.Get(key)
	if entry == nil {
		s.metrics.IncCacheMiss()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found"})
		return
	}

	s.metrics.IncCacheHit()

	// Try to return value as JSON if possible, otherwise as string
	var value interface{}
	if json.Valid(entry.Value) {
		value = json.RawMessage(entry.Value)
	} else {
		value = string(entry.Value)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key":           entry.Key,
		"value":         value,
		"ttl":           entry.RemainingTTL(),
		"created_at":    entry.CreatedAt,
	})
}

// --- SET ---

func (s *HTTPServer) handleCacheSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cache == nil {
		writeError(w, http.StatusServiceUnavailable, "cache not enabled")
		return
	}

	var req struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
		TTL   int64           `json:"ttl"` // milliseconds
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()

	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	// Store value as-is (raw JSON bytes)
	var valueBytes []byte
	if req.Value != nil {
		// If it's a JSON string, unwrap quotes for plain string storage
		var str string
		if json.Unmarshal(req.Value, &str) == nil {
			valueBytes = []byte(str)
		} else {
			valueBytes = []byte(req.Value)
		}
	}

	ttl := req.TTL
	if ttl == 0 && s.defaultTTL > 0 {
		ttl = s.defaultTTL
	}

	s.cache.Set(req.Key, valueBytes, ttl)
	s.metrics.IncCacheSet()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"key":    req.Key,
	})
}

// --- DEL ---

func (s *HTTPServer) handleCacheDel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cache == nil {
		writeError(w, http.StatusServiceUnavailable, "cache not enabled")
		return
	}

	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()

	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	deleted := s.cache.Del(req.Key)
	if deleted {
		s.metrics.IncCacheDelete()
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"deleted": deleted,
	})
}

// --- KEYS ---

func (s *HTTPServer) handleCacheKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cache == nil {
		writeError(w, http.StatusServiceUnavailable, "cache not enabled")
		return
	}

	pattern := r.URL.Query().Get("pattern")
	if pattern == "" {
		pattern = "*"
	}

	keys := s.cache.Keys(pattern)
	if keys == nil {
		keys = []string{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"keys":  keys,
		"count": len(keys),
	})
}

// --- EXISTS ---

func (s *HTTPServer) handleCacheExists(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cache == nil {
		writeError(w, http.StatusServiceUnavailable, "cache not enabled")
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	exists := s.cache.Exists(key)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key":    key,
		"exists": exists,
	})
}

// --- EXPIRE ---

func (s *HTTPServer) handleCacheExpire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cache == nil {
		writeError(w, http.StatusServiceUnavailable, "cache not enabled")
		return
	}

	var req struct {
		Key string `json:"key"`
		TTL int64  `json:"ttl"` // milliseconds
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()

	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	ok := s.cache.Expire(req.Key, req.TTL)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"key":    req.Key,
		"found":  ok,
	})
}

// --- INCR ---

func (s *HTTPServer) handleCacheIncr(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cache == nil {
		writeError(w, http.StatusServiceUnavailable, "cache not enabled")
		return
	}

	var req struct {
		Key   string `json:"key"`
		Delta int64  `json:"delta"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	defer r.Body.Close()

	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}
	if req.Delta == 0 {
		req.Delta = 1
	}

	val, err := s.cache.Incr(req.Key, req.Delta)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.metrics.IncCacheSet()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key":   req.Key,
		"value": val,
	})
}

// --- FLUSH ---

func (s *HTTPServer) handleCacheFlush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cache == nil {
		writeError(w, http.StatusServiceUnavailable, "cache not enabled")
		return
	}

	count := s.cache.FlushAll()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "flushed",
		"removed": count,
	})
}

// --- STATS ---

func (s *HTTPServer) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cache == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled": false,
		})
		return
	}

	stats := s.cache.GetStats()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": true,
		"stats":   stats,
	})
}

// --- ENTRIES (paginated browse) ---

func (s *HTTPServer) handleCacheEntries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cache == nil {
		writeError(w, http.StatusServiceUnavailable, "cache not enabled")
		return
	}

	pattern := r.URL.Query().Get("pattern")
	if pattern == "" {
		pattern = "*"
	}

	search := r.URL.Query().Get("search")

	keys := s.cache.Keys(pattern)

	// Apply search filter
	if search != "" {
		search = strings.ToLower(search)
		filtered := make([]string, 0)
		for _, k := range keys {
			if strings.Contains(strings.ToLower(k), search) {
				filtered = append(filtered, k)
			}
		}
		keys = filtered
	}

	// Limit to 100 entries for UI
	total := len(keys)
	if len(keys) > 100 {
		keys = keys[:100]
	}

	entries := make([]map[string]interface{}, 0, len(keys))
	for _, key := range keys {
		entry := s.cache.Get(key)
		if entry == nil {
			continue
		}

		var value interface{}
		if json.Valid(entry.Value) {
			value = json.RawMessage(entry.Value)
		} else {
			value = string(entry.Value)
		}

		entries = append(entries, map[string]interface{}{
			"key":        entry.Key,
			"value":      value,
			"ttl":        entry.RemainingTTL(),
			"created_at": entry.CreatedAt,
			"size":       len(entry.Value),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries,
		"total":   total,
	})
}

// SetCache sets the cache store for cache endpoints.
func (s *HTTPServer) SetCache(c *cache.Store, defaultTTL int64) {
	s.cache = c
	s.defaultTTL = defaultTTL
}
