# 🚀 Go Proxy with Upstream Pooling

A simple forward proxy written in Go that lets you route all your HTTP/HTTPS traffic through an array of upstream HTTP and SOCKS5 proxies.

## ✨ Features

- Acts as a local forward proxy (listens on localhost:8080 by default)
- Supports upstream HTTP and SOCKS5 proxies
- Periodically probes proxies and promotes fastest ones first
- Automatic failover if a proxy is slow or unreachable
- Docker-ready with Go 1.25

## 📦 Setup

### 1. Clone and build

```bash
git clone https://github.com/yourusername/go-proxy.git
cd go-proxy
make build
```

### 2. Configure proxies

Create a `proxies.json` in the project root:

```json
[
  { "raw": "http://user:pass@1.2.3.4:3128" },
  { "raw": "socks5://5.6.7.8:1080" },
  { "raw": "http://9.10.11.12:8080" }
]
```

### 3. Run locally

```bash
make run
```

This will start the proxy at localhost:8080.

## 🐳 Run with Docker

### Build the image

```bash
make docker-build
```

### Run container

```bash
make docker-run
```

This maps port 8080 from the container to your host and mounts proxies.json.

## 🌍 Use the Proxy

### Browser only

Set browser HTTP/HTTPS proxy to `localhost:8080`.

### System-wide

- **macOS**: System Preferences → Network → Advanced → Proxies
- **Windows**: Settings → Network & Internet → Proxy → Manual setup
- **Linux**: Set `http_proxy` / `https_proxy` environment variables or system proxy settings

For apps without proxy support, use tools like `proxychains` or `redsocks` to redirect TCP through the proxy.

## ⚙️ Makefile Commands

| Command             | Description                           |
| ------------------- | ------------------------------------- |
| `make build`        | Build binary into bin/proxy           |
| `make run`          | Run locally with proxies.json         |
| `make clean`        | Remove build artifacts                |
| `make docker-build` | Build Docker image                    |
| `make docker-run`   | Run container with port 8080 + config |

## 📝 TODO / Improvements

- Replace placeholder base64 with proper encoding/base64 (for Proxy-Authorization)
- Add authentication for local proxy
- Add metrics or dashboard for proxy health
- Persist proxy performance history across restarts

## 📜 License

MIT License. Use at your own risk.
