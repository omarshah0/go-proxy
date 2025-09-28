// main.go
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

type ProxySpec struct {
	Raw string `json:"raw"` // e.g. "http://user:pass@1.2.3.4:3128" or "socks5://1.2.3.4:1080"
}

type Upstream struct {
	Raw       string
	URL       *url.URL
	Kind      string // "http" or "socks5"
	Addr      string // host:port for direct TCP connect to proxy
	User      string
	Password  string
	lastRTT   time.Duration
	lastOK    bool
	score     float64
	mu        sync.RWMutex
	lastCheck time.Time
}

type Pool struct {
	upstreams []*Upstream
	mu        sync.RWMutex
}

func (p *Pool) Best() *Upstream {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.upstreams) == 0 {
		return nil
	}

	var best *Upstream
	var bestScore float64 = -1

	// Find best proxy without complex locking
	for _, u := range p.upstreams {
		u.mu.RLock()
		score := u.score
		lastOK := u.lastOK
		u.mu.RUnlock()

		if lastOK && score > bestScore {
			best = u
			bestScore = score
		}
	}

	return best
}

func (p *Pool) List() []*Upstream {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Upstream, len(p.upstreams))
	copy(out, p.upstreams)
	return out
}

func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.upstreams)
}

func loadConfig(path string) ([]*Upstream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var specs []ProxySpec
	if err := json.NewDecoder(f).Decode(&specs); err != nil {
		return nil, err
	}
	var ups []*Upstream
	for _, s := range specs {
		u, err := url.Parse(s.Raw)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy url %q: %w", s.Raw, err)
		}
		kind := "http"
		if u.Scheme == "socks5" || u.Scheme == "socks5h" {
			kind = "socks5"
		}
		up := &Upstream{
			Raw:  s.Raw,
			URL:  u,
			Kind: kind,
		}
		if u.User != nil {
			up.User = u.User.Username()
			if p, ok := u.User.Password(); ok {
				up.Password = p
			}
		}
		host := u.Host
		// If scheme includes path (rare) strip
		if strings.Contains(host, "/") {
			host = strings.Split(host, "/")[0]
		}
		up.Addr = host
		up.score = 0.0
		ups = append(ups, up)
	}
	return ups, nil
}

// dialViaUpstream creates a net.Conn to targetAddr ("host:port") using the upstream proxy.
func dialViaUpstream(ctx context.Context, up *Upstream, targetAddr string) (net.Conn, error) {
	if up.Kind == "socks5" {
		var auth *proxy.Auth
		if up.User != "" {
			auth = &proxy.Auth{
				User:     up.User,
				Password: up.Password,
			}
		}
		// Create a SOCKS5 dialer to proxy addr
		dialer, err := proxy.SOCKS5("tcp", up.Addr, auth, proxy.Direct)
		if err != nil {
			return nil, err
		}
		// dialer.Dial doesn't accept context; use a small wrapper
		type dialerIface interface {
			Dial(network, addr string) (net.Conn, error)
		}
		d := dialer.(dialerIface)
		conn, err := d.Dial("tcp", targetAddr)
		if err != nil {
			return nil, err
		}
		return conn, nil
	}

	// HTTP proxy upstream: open TCP to proxy and send CONNECT for target.
	conn, err := net.DialTimeout("tcp", up.Addr, 10*time.Second)
	if err != nil {
		return nil, err
	}
	// build CONNECT request
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", targetAddr, targetAddr)
	if up.User != "" {
		auth := up.User + ":" + up.Password
		enc := basicAuth(auth)
		req += "Proxy-Authorization: Basic " + enc + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	// read response line
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != 200 {
		// read body for debugging then close
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed: %s - %s", resp.Status, string(body))
	}
	// At this point br may have buffered bytes (unlikely). Wrap conn with br to preserve buffered bytes.
	// But net.Conn cannot be replaced easily; simplest: ignore buffered because usually no leftover.
	return conn, nil
}

func basicAuth(creds string) string {
	return base64.StdEncoding.EncodeToString([]byte(creds))
}

// -------------------- PROBE --------------------

func (p *Pool) probeLoop(ctx context.Context, interval time.Duration, probeTarget string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.probeAllProxies()
		}
	}
}

// ProbeAllProxies tests all proxies simultaneously and updates their scores
func (p *Pool) probeAllProxies() {
	ups := p.List()
	var wg sync.WaitGroup

	log.Printf("Probing %d proxies...", len(ups))

	for _, u := range ups {
		wg.Add(1)
		go func(up *Upstream) {
			defer wg.Done()
			rtt, ok := probeProxyHTTP(up)
			up.mu.Lock()
			up.lastCheck = time.Now()
			up.lastRTT = rtt
			up.lastOK = ok
			if ok {
				// Better scoring: combination of speed and reliability
				// Base score from latency, bonus for reliability
				latencyScore := 1000.0 / float64(rtt.Milliseconds()+10) // +10 to avoid division issues
				up.score = latencyScore
				log.Printf("Proxy %s: OK (RTT: %v, Score: %.2f)", up.Addr, rtt, up.score)
			} else {
				up.score = 0.0
				log.Printf("Proxy %s: FAILED", up.Addr)
			}
			up.mu.Unlock()
		}(u)
	}
	wg.Wait()

	// Log best proxy
	if best := p.Best(); best != nil {
		best.mu.RLock()
		addr := best.Addr
		score := best.score
		rtt := best.lastRTT
		best.mu.RUnlock()
		log.Printf("Best proxy: %s (Score: %.2f, RTT: %v)", addr, score, rtt)
	} else {
		log.Println("No working proxies found")
	}
}

// FreshProbe forces an immediate probe of all proxies (used when reconnecting)
func (p *Pool) FreshProbe() {
	log.Println("Starting fresh proxy discovery...")
	p.probeAllProxies()
}

// probeProxyHTTP tests proxy with actual HTTP request to multiple endpoints
func probeProxyHTTP(up *Upstream) (time.Duration, bool) {
	// Test endpoints - use fast, reliable services
	testURLs := []string{
		"http://httpbin.org/ip",
		"http://icanhazip.com",
		"http://checkip.amazonaws.com",
	}

	start := time.Now()

	// Try each test URL, return on first success
	for _, testURL := range testURLs {
		if rtt, ok := testProxyWithURL(up, testURL); ok {
			return rtt, true
		}
	}

	return time.Since(start), false
}

func testProxyWithURL(up *Upstream, testURL string) (time.Duration, bool) {
	start := time.Now()

	// Create HTTP client with proxy
	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			return up.URL, nil
		},
		ResponseHeaderTimeout: 5 * time.Second,
		DisableKeepAlives:     true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	resp, err := client.Get(testURL)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()

	// Check for successful response
	if resp.StatusCode != 200 {
		return 0, false
	}

	// Read a bit of the response to ensure it's working
	buffer := make([]byte, 64)
	_, err = resp.Body.Read(buffer)
	if err != nil && err != io.EOF {
		return 0, false
	}

	return time.Since(start), true
}

// -------------------- HANDLER --------------------

type Server struct {
	pool   *Pool
	bypass bool // when true, acts as direct connection without upstream proxies
	mu     sync.RWMutex
}

func (s *Server) SetBypass(bypass bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bypass = bypass
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	bypassMode := s.bypass
	s.mu.RUnlock()

	// If bypass mode is enabled, act as direct connection
	if bypassMode {
		s.handleDirect(w, r)
		return
	}

	// pick best upstream
	up := s.pool.Best()
	if up == nil {
		http.Error(w, "no upstream proxies configured", http.StatusBadGateway)
		return
	}
	// If it's CONNECT -> tunnel
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r, up)
		return
	}
	// Non-CONNECT: use http.Transport with Proxy pointing to upstream
	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			return up.URL, nil
		},
		ForceAttemptHTTP2:     false,
		DisableKeepAlives:     false,
		MaxIdleConnsPerHost:   10,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	outReq := r.Clone(context.Background())
	// ensure URL is absolute for proxy.
	if outReq.URL.Scheme == "" {
		if r.TLS != nil {
			outReq.URL.Scheme = "https"
		} else {
			outReq.URL.Scheme = "http"
		}
	}
	outReq.RequestURI = ""
	// remove hop-by-hop headers
	removeHopByHop(outReq.Header)
	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (s *Server) handleDirect(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleDirectConnect(w, r)
		return
	}

	// For non-CONNECT requests in direct mode, make direct HTTP request
	transport := &http.Transport{
		ForceAttemptHTTP2:     false,
		DisableKeepAlives:     false,
		MaxIdleConnsPerHost:   10,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	outReq := r.Clone(context.Background())
	// ensure URL is absolute
	if outReq.URL.Scheme == "" {
		if r.TLS != nil {
			outReq.URL.Scheme = "https"
		} else {
			outReq.URL.Scheme = "http"
		}
	}
	outReq.RequestURI = ""
	// remove hop-by-hop headers
	removeHopByHop(outReq.Header)

	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "direct connection error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (s *Server) handleDirectConnect(w http.ResponseWriter, r *http.Request) {
	hij, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "proxy does not support hijacking", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hij.Hijack()
	if err != nil {
		http.Error(w, "hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Direct connection to target
	target := r.Host
	serverConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		clientConn.Close()
		return
	}

	// send success to client
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// pipe both directions
	go func() {
		io.Copy(serverConn, clientConn)
		serverConn.Close()
		clientConn.Close()
	}()
	go func() {
		io.Copy(clientConn, serverConn)
		serverConn.Close()
		clientConn.Close()
	}()
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request, up *Upstream) {
	hij, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "proxy does not support hijacking", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hij.Hijack()
	if err != nil {
		http.Error(w, "hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Dial target via upstream (upstream should perform CONNECT if needed)
	target := r.Host
	serverConn, err := dialViaUpstream(r.Context(), up, target)
	if err != nil {
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		clientConn.Close()
		return
	}
	// send success to client
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	// pipe both directions
	go func() {
		io.Copy(serverConn, clientConn)
		serverConn.Close()
		clientConn.Close()
	}()
	go func() {
		io.Copy(clientConn, serverConn)
		serverConn.Close()
		clientConn.Close()
	}()
}

// -------------------- UTIL --------------------

func removeHopByHop(h http.Header) {
	h.Del("Proxy-Connection")
	h.Del("Connection")
	h.Del("Keep-Alive")
	h.Del("Proxy-Authenticate")
	h.Del("Proxy-Authorization")
	h.Del("TE")
	h.Del("Trailers")
	h.Del("Transfer-Encoding")
	h.Del("Upgrade")
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// -------------------- MAIN --------------------

func runCLI() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: ./proxy <proxies.json> [listen_addr]")
		fmt.Println("       ./proxy --gui  (for native macOS app)")
		fmt.Println("Example proxies.json content: [{\"raw\":\"http://user:pass@1.2.3.4:3128\"},{\"raw\":\"socks5://1.2.3.4:1080\"}]")
		return
	}
	cfg := os.Args[1]
	listen := ":8080"
	if len(os.Args) >= 3 {
		listen = os.Args[2]
	}

	ups, err := loadConfig(cfg)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	pool := &Pool{upstreams: ups}

	// Start probe loop with faster interval for better responsiveness
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.probeLoop(ctx, 30*time.Second, "") // Empty target since we use HTTP endpoints now

	// start HTTP proxy server
	srv := &http.Server{
		Addr:    listen,
		Handler: &Server{pool: pool},
	}
	log.Printf("listening local proxy on %s, upstream count=%d", listen, len(ups))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
	}
}

func runGUI() {
	// Load config
	cfg := "proxies.json" // default config file
	ups, err := loadConfig(cfg)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	pool := &Pool{upstreams: ups}

	// Start probe loop with faster interval
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.probeLoop(ctx, 30*time.Second, "")

	// Create server that starts in bypass mode
	server := &Server{pool: pool, bypass: true}

	// Start HTTP proxy server in background
	srv := &http.Server{
		Addr:    ":8080",
		Handler: server,
	}
	go func() {
		log.Printf("proxy server ready on :8080, upstream count=%d", len(ups))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server failed: %v", err)
		}
	}()

	// Create and run GUI app
	app := NewProxyApp(pool, server)
	app.Run()
}

func main() {
	// Check if GUI mode is requested
	if len(os.Args) >= 2 && os.Args[1] == "--gui" {
		runGUI()
	} else {
		runCLI()
	}
}
