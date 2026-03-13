package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	boltq "github.com/boltq/boltq/client/golang"
	"github.com/gorilla/mux"
)

var client *boltq.Client

func main() {
	boltqAddr := envOr("BOLTQ_ADDR", "localhost:9091")
	apiKey := os.Getenv("BOLTQ_API_KEY")
	listenAddr := envOr("LISTEN_ADDR", ":3000")

	opts := []boltq.Option{}
	if apiKey != "" {
		opts = append(opts, boltq.WithAPIKey(apiKey))
	}
	client = boltq.New(boltqAddr, opts...)

	if err := client.Connect(); err != nil {
		log.Fatalf("failed to connect to BoltQ: %v", err)
	}
	defer client.Close()

	// Start background order consumer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go consumeOrders(ctx)

	r := mux.NewRouter()
	r.HandleFunc("/orders", createOrder).Methods("POST")
	r.HandleFunc("/orders/notify", notifyAll).Methods("POST")
	r.HandleFunc("/stats", getStats).Methods("GET")
	r.HandleFunc("/health", checkHealth).Methods("GET")

	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("Server listening on %s", listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)
}

// --- Handlers ---

type orderRequest struct {
	Product  string `json:"product"`
	Quantity int    `json:"quantity"`
}

func createOrder(w http.ResponseWriter, r *http.Request) {
	var req orderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}

	payload := map[string]interface{}{
		"product":    req.Product,
		"quantity":   req.Quantity,
		"created_at": time.Now().Format(time.RFC3339),
	}

	id, err := client.Publish("orders", payload, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message":    "Order queued",
		"message_id": id,
	})
}

type notifyRequest struct {
	Event string      `json:"event"`
	Data  interface{} `json:"data"`
}

func notifyAll(w http.ResponseWriter, r *http.Request) {
	var req notifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}

	payload := map[string]interface{}{
		"event":     req.Event,
		"data":      req.Data,
		"timestamp": time.Now().Format(time.RFC3339),
	}

	id, err := client.PublishTopic("order-events", payload, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message":    "Event published",
		"message_id": id,
	})
}

func getStats(w http.ResponseWriter, r *http.Request) {
	stats, err := client.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func checkHealth(w http.ResponseWriter, r *http.Request) {
	status := "up"
	if err := client.Health(); err != nil {
		status = "down"
	}
	writeJSON(w, http.StatusOK, map[string]string{"boltq": status})
}

// --- Background consumer ---

func consumeOrders(ctx context.Context) {
	log.Println("Order consumer started")
	for {
		select {
		case <-ctx.Done():
			log.Println("Order consumer stopped")
			return
		default:
		}

		msg, err := client.Consume("orders")
		if err != nil {
			log.Printf("consume error: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		if msg == nil {
			time.Sleep(1 * time.Second)
			continue
		}

		log.Printf("Processing order %s: %s", msg.ID, string(msg.Payload))

		// Simulate processing
		time.Sleep(100 * time.Millisecond)

		if err := client.Ack(msg.ID); err != nil {
			log.Printf("ack error: %v", err)
		} else {
			log.Printf("Order %s processed and acked", msg.ID)
		}
	}
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
