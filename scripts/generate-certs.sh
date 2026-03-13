#!/bin/bash
set -e

# Setup directory
DIR="./certs"
mkdir -p $DIR

echo "Generating CA key and certificate..."
openssl genrsa -out $DIR/ca.key 2048
openssl req -x509 -new -nodes -key $DIR/ca.key -sha256 -days 365 -out $DIR/ca.crt -subj "/CN=BoltQ-CA"

echo "Generating server key and certificate request..."
openssl genrsa -out $DIR/server.key 2048
openssl req -new -key $DIR/server.key -out $DIR/server.csr -subj "/CN=localhost"

echo "Signing server certificate with CA..."
openssl x509 -req -in $DIR/server.csr -CA $DIR/ca.crt -CAkey $DIR/ca.key -CAcreateserial -out $DIR/server.crt -days 365 -sha256

echo "Certificate generation complete in $DIR"
ls -l $DIR
