package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	boltq "github.com/boltq/boltq/client/golang"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	serverAddr := os.Getenv("BOLTQ_ADDR")
	if serverAddr == "" {
		serverAddr = "localhost:9091"
	}
	apiKey := os.Getenv("BOLTQ_API_KEY")

	var opts []boltq.Option
	if apiKey != "" {
		opts = append(opts, boltq.WithAPIKey(apiKey))
	}

	if os.Getenv("BOLTQ_TLS_ENABLED") == "true" {
		tlsCfg := &tls.Config{}
		caFile := os.Getenv("BOLTQ_CA_FILE")
		if caFile != "" {
			caCert, err := os.ReadFile(caFile)
			if err != nil {
				log.Fatalf("failed to read CA file: %v", err)
			}
			caPool := x509.NewCertPool()
			caPool.AppendCertsFromPEM(caCert)
			tlsCfg.RootCAs = caPool
		} else {
			// If no CA provided, but TLS enabled, we might want to skip verification for self-signed
			// or use system certs. For now, let's assume system certs if no CA provided.
			// Or we can add a BOLTQ_TLS_INSECURE flag.
			if os.Getenv("BOLTQ_TLS_INSECURE") == "true" {
				tlsCfg.InsecureSkipVerify = true
			}
		}
		opts = append(opts, boltq.WithTLS(tlsCfg))
	}

	client := boltq.New(serverAddr, opts...)
	if err := client.Connect(); err != nil {
		log.Fatalf("failed to connect to %s: %v", serverAddr, err)
	}
	defer client.Close()

	switch os.Args[1] {
	case "server":
		fmt.Println("Use 'boltq-server' or 'go run ./cmd/server' to start the server")
	case "publish":
		cmdPublish(client, os.Args[2:])
	case "consume":
		cmdConsume(client, os.Args[2:])
	case "ack":
		cmdAck(client, os.Args[2:])
	case "nack":
		cmdNack(client, os.Args[2:])
	case "stats":
		cmdStats(client)
	case "health":
		cmdHealth(client)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`BoltQ CLI

Usage:
  boltq <command> [flags]

Commands:
  publish   Publish a message to a queue
  consume   Consume a message from a queue
  ack       Acknowledge a message
  nack      Negatively acknowledge a message
  stats     Show broker statistics
  health    Check server health

Environment:
  BOLTQ_ADDR          TCP server address (default: localhost:9091)
  BOLTQ_API_KEY       API key for authentication
  BOLTQ_TLS_ENABLED   Enable TLS (true/false)
  BOLTQ_CA_FILE       Path to CA certificate file
  BOLTQ_TLS_INSECURE  Skip TLS verification (true/false)`)
}

func cmdPublish(client *boltq.Client, args []string) {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	topic := fs.String("topic", "", "topic/queue name")
	payload := fs.String("payload", "", "message payload (JSON string)")
	fs.Parse(args)

	if *topic == "" || *payload == "" {
		fmt.Fprintln(os.Stderr, "usage: boltq publish -topic <name> -payload <json>")
		os.Exit(1)
	}

	id, err := client.Publish(*topic, json.RawMessage(*payload), nil, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "publish error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("published: id=%s topic=%s\n", id, *topic)
}

func cmdConsume(client *boltq.Client, args []string) {
	fs := flag.NewFlagSet("consume", flag.ExitOnError)
	topic := fs.String("topic", "", "topic/queue name")
	fs.Parse(args)

	if *topic == "" {
		fmt.Fprintln(os.Stderr, "usage: boltq consume -topic <name>")
		os.Exit(1)
	}

	msg, err := client.Consume(*topic)
	if err != nil {
		fmt.Fprintf(os.Stderr, "consume error: %v\n", err)
		os.Exit(1)
	}
	if msg == nil {
		fmt.Println("no messages available")
		return
	}

	data, _ := json.MarshalIndent(msg, "", "  ")
	fmt.Println(string(data))
}

func cmdAck(client *boltq.Client, args []string) {
	fs := flag.NewFlagSet("ack", flag.ExitOnError)
	id := fs.String("id", "", "message ID")
	fs.Parse(args)

	if *id == "" {
		fmt.Fprintln(os.Stderr, "usage: boltq ack -id <message_id>")
		os.Exit(1)
	}

	if err := client.Ack(*id); err != nil {
		fmt.Fprintf(os.Stderr, "ack error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("acked:", *id)
}

func cmdNack(client *boltq.Client, args []string) {
	fs := flag.NewFlagSet("nack", flag.ExitOnError)
	id := fs.String("id", "", "message ID")
	fs.Parse(args)

	if *id == "" {
		fmt.Fprintln(os.Stderr, "usage: boltq nack -id <message_id>")
		os.Exit(1)
	}

	if err := client.Nack(*id); err != nil {
		fmt.Fprintf(os.Stderr, "nack error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("nacked:", *id)
}

func cmdStats(client *boltq.Client) {
	stats, err := client.Stats()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stats error: %v\n", err)
		os.Exit(1)
	}
	data, _ := json.MarshalIndent(stats, "", "  ")
	fmt.Println(string(data))
}

func cmdHealth(client *boltq.Client) {
	if err := client.Health(); err != nil {
		fmt.Fprintf(os.Stderr, "unhealthy: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("healthy")
}
