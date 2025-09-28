package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
)

type ProxyApp struct {
	app          fyne.App
	pool         *Pool
	server       *Server
	enabled      bool
	mu           sync.RWMutex
	statusLabel  *widget.Label
	proxyLabel   *widget.Label
	scoreLabel   *widget.Label
	toggleButton *widget.Button
}

func NewProxyApp(pool *Pool, server *Server) *ProxyApp {
	myApp := app.New()
	
	// Try to load icon from file, fallback to nil if not found
	if iconResource := loadIconFromFile("icon.png"); iconResource != nil {
		myApp.SetIcon(iconResource)
	}
	
	pa := &ProxyApp{
		app:     myApp,
		pool:    pool,
		server:  server,
		enabled: false, // start disconnected
	}
	
	pa.setupUI()
	return pa
}

func loadIconFromFile(path string) fyne.Resource {
	// Try to read icon file
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("Could not load icon from %s: %v", path, err)
		return nil
	}
	return fyne.NewStaticResource("icon.png", data)
}

func (pa *ProxyApp) setupUI() {
	// Create status labels
	pa.statusLabel = widget.NewLabel("🔴 Disconnected")
	pa.statusLabel.TextStyle.Bold = true

	pa.proxyLabel = widget.NewLabel("Proxy: None")
	pa.scoreLabel = widget.NewLabel("Score: -")

	// Create main toggle button
	pa.toggleButton = widget.NewButton("Connect to Proxy", pa.toggleProxy)
	pa.toggleButton.Importance = widget.HighImportance

	// Create main window (hidden by default for system tray app)
	w := pa.app.NewWindow("Go Proxy")
	w.SetFixedSize(true)
	w.Resize(fyne.NewSize(400, 300))

	content := container.NewVBox(
		widget.NewCard("Proxy Status", "", container.NewVBox(
			pa.statusLabel,
			pa.proxyLabel,
			pa.scoreLabel,
		)),
		widget.NewSeparator(),
		pa.toggleButton,
		widget.NewButton("Quit", func() {
			pa.cleanup()
			pa.app.Quit()
		}),
	)

	w.SetContent(container.NewPadded(content))

	// Setup system tray
	if desk, ok := pa.app.(desktop.App); ok {
		menu := fyne.NewMenu("Go Proxy",
			fyne.NewMenuItem("Show", func() {
				w.Show()
			}),
			fyne.NewMenuItem("Toggle Proxy", pa.toggleProxy),
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("Quit", func() {
				pa.cleanup()
				pa.app.Quit()
			}),
		)
		desk.SetSystemTrayMenu(menu)
		
		// Set system tray icon if available
		if iconResource := loadIconFromFile("icon.png"); iconResource != nil {
			desk.SetSystemTrayIcon(iconResource)
		}
	}

	// Hide main window initially (run as system tray app)
	w.Hide()

	// Start status update loop
	go pa.updateStatusLoop()
}

func (pa *ProxyApp) toggleProxy() {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	pa.enabled = !pa.enabled
	pa.server.SetBypass(!pa.enabled) // bypass when disabled

	if pa.enabled {
		// Enable proxy
		pa.toggleButton.SetText("Disconnect Proxy")
		pa.statusLabel.SetText("🟡 Connecting...")

		// Configure macOS system proxy
		go pa.enableSystemProxy()
	} else {
		// Disable proxy
		pa.toggleButton.SetText("Connect to Proxy")
		pa.statusLabel.SetText("🔴 Disconnected")
		pa.proxyLabel.SetText("Proxy: None")
		pa.scoreLabel.SetText("Score: -")

		// Disable macOS system proxy
		go pa.disableSystemProxy()
	}
}

func (pa *ProxyApp) updateStatusLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		pa.updateStatus()
	}
}

func (pa *ProxyApp) updateStatus() {
	pa.mu.RLock()
	enabled := pa.enabled
	pa.mu.RUnlock()

	if !enabled {
		return
	}

	best := pa.pool.Best()
	if best == nil {
		pa.statusLabel.SetText("🔴 No proxies available")
		pa.proxyLabel.SetText("Proxy: None")
		pa.scoreLabel.SetText("Score: -")
		return
	}

	best.mu.RLock()
	lastOK := best.lastOK
	score := best.score
	addr := best.Addr
	rtt := best.lastRTT
	best.mu.RUnlock()

	if lastOK {
		pa.statusLabel.SetText("🟢 Connected")
		pa.proxyLabel.SetText(fmt.Sprintf("Proxy: %s", addr))
		pa.scoreLabel.SetText(fmt.Sprintf("Score: %.1f (RTT: %v)", score, rtt))
	} else {
		pa.statusLabel.SetText("🟡 Proxy issues detected")
		pa.proxyLabel.SetText(fmt.Sprintf("Proxy: %s (failing)", addr))
		pa.scoreLabel.SetText("Score: 0.0")
	}
}

func (pa *ProxyApp) enableSystemProxy() {
	// Get network service (usually Wi-Fi or Ethernet)
	cmd := exec.Command("networksetup", "-setwebproxy", "Wi-Fi", "127.0.0.1", "8080")
	if err := cmd.Run(); err != nil {
		log.Printf("Failed to set HTTP proxy: %v", err)
	}

	cmd = exec.Command("networksetup", "-setsecurewebproxy", "Wi-Fi", "127.0.0.1", "8080")
	if err := cmd.Run(); err != nil {
		log.Printf("Failed to set HTTPS proxy: %v", err)
	}

	// Try Ethernet as fallback
	exec.Command("networksetup", "-setwebproxy", "Ethernet", "127.0.0.1", "8080").Run()
	exec.Command("networksetup", "-setsecurewebproxy", "Ethernet", "127.0.0.1", "8080").Run()
}

func (pa *ProxyApp) disableSystemProxy() {
	// Disable proxy for Wi-Fi
	cmd := exec.Command("networksetup", "-setwebproxystate", "Wi-Fi", "off")
	if err := cmd.Run(); err != nil {
		log.Printf("Failed to disable HTTP proxy: %v", err)
	}

	cmd = exec.Command("networksetup", "-setsecurewebproxystate", "Wi-Fi", "off")
	if err := cmd.Run(); err != nil {
		log.Printf("Failed to disable HTTPS proxy: %v", err)
	}

	// Try Ethernet as fallback
	exec.Command("networksetup", "-setwebproxystate", "Ethernet", "off").Run()
	exec.Command("networksetup", "-setsecurewebproxystate", "Ethernet", "off").Run()
}

func (pa *ProxyApp) cleanup() {
	// Always disable system proxy on exit
	pa.disableSystemProxy()
}

func (pa *ProxyApp) Run() {
	pa.app.Run()
}
