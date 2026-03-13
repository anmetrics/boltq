.PHONY: build server cli test bench clean docker

build: server cli

server:
	go build -ldflags="-s -w" -o bin/boltq-server ./cmd/server

cli:
	go build -ldflags="-s -w" -o bin/boltq ./cmd/cli

test:
	go test -v -race ./...

bench:
	go test -bench=. -benchmem -run=^$$ ./internal/queue/
	go test -bench=. -benchmem -run=^$$ ./internal/wal/
	go test -bench=. -benchmem -run=^$$ ./internal/broker/
	go test -bench=. -benchmem -run=^$$ ./internal/api/

bench-queue:
	go test -bench=. -benchmem -benchtime=5s -run=^$$ ./internal/queue/

bench-wal:
	go test -bench=. -benchmem -benchtime=5s -run=^$$ ./internal/wal/

bench-broker:
	go test -bench=. -benchmem -benchtime=5s -run=^$$ ./internal/broker/

bench-api:
	go test -bench=. -benchmem -benchtime=5s -run=^$$ ./internal/api/

clean:
	rm -rf bin/ data/

docker:
	docker build -t boltq:latest .

run: server
	./bin/boltq-server
