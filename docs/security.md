# Security & TLS

BoltQ supports full end-to-end encryption using TLS (Transport Layer Security) for both its TCP messaging protocol and HTTP admin API.

## Enabling TLS on Server

To enable TLS, you must provide a certificate and a private key in the configuration.

### Configuration

```json
{
  "server": {
    "tls": {
      "enabled": true,
      "cert_file": "./certs/server.crt",
      "key_file": "./certs/server.key"
    }
  }
}
```

### Generating Self-Signed Certificates

For development and testing, you can use the provided script to generate a self-signed CA and server certificate:

```bash
./scripts/generate-certs.sh
```

This will produce:
- `certs/ca.crt`: The Root CA certificate (give this to clients).
- `certs/server.crt`: The server certificate.
- `certs/server.key`: The server private key.

## Using TLS with Go Client

The Go SDK supports TLS via the `WithTLS` option.

### With Verified CA (Recommended)

```go
import (
    "crypto/tls"
    "crypto/x509"
    boltq "github.com/boltq/boltq/client/golang"
)

// Load CA
caCert, _ := os.ReadFile("certs/ca.crt")
caPool := x509.NewCertPool()
caPool.AppendCertsFromPEM(caCert)

tlsCfg := &tls.Config{
    RootCAs: caPool,
}

client := boltq.New("localhost:9091", boltq.WithTLS(tlsCfg))
err := client.Connect()
```

### Insecure (Skip Verification)

```go
client := boltq.New("localhost:9091", boltq.WithTLS(&tls.Config{
    InsecureSkipVerify: true,
}))
```

## Using TLS with CLI

The CLI tool supports TLS via environment variables.

```bash
# Enable TLS and point to CA
export BOLTQ_TLS_ENABLED=true
export BOLTQ_CA_FILE=./certs/ca.crt
boltq stats

# Or skip verification
export BOLTQ_TLS_ENABLED=true
export BOLTQ_TLS_INSECURE=true
boltq stats
```

## Production Recommendations

- **Don't use self-signed certificates in production.** Use a trusted CA like Let's Encrypt or your organization's internal CA.
- **TLS Termination**: In many cloud environments (like AWS), you can terminate TLS at the Load Balancer (ALB/NLB). In this case, BoltQ can run without TLS internally, but the connection from the public internet remains encrypted.
- **Secrets Management**: In Kubernetes, use `Secrets` to store and mount your certificates.
