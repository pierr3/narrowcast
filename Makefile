.PHONY: build build-pi run clean certs

# Build for local machine (macOS w/ Homebrew)
build:
	CGO_CFLAGS="-I/opt/homebrew/include" CGO_LDFLAGS="-L/opt/homebrew/lib" \
		go build -o bin/narrowcast ./cmd/narrowcast

# Cross-compile for Raspberry Pi (64-bit)
build-pi:
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
		go build -o bin/narrowcast-arm64 ./cmd/narrowcast

# Run locally
run: build
	./bin/narrowcast --host 0.0.0.0 --port 4444

# Generate self-signed TLS certs
certs:
	@mkdir -p certs
	openssl ecparam -genkey -name prime256v1 -out certs/server.key
	openssl req -new -x509 -key certs/server.key -out certs/server.crt \
		-days 3650 -subj "/CN=narrowcast"
	@echo "Certs written to certs/"

clean:
	rm -rf bin/
