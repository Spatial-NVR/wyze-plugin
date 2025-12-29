package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NewJSONDecoder creates a new JSON decoder
func NewJSONDecoder(r io.Reader) *json.Decoder {
	return json.NewDecoder(r)
}

const (
	// DefaultRTSPPort is the default RTSP port for wyze-bridge
	// Uses 8564 to avoid conflict with go2rtc (8554/8555) and previous instances
	// Can be overridden via WYZE_RTSP_PORT environment variable
	DefaultRTSPPort = 8564

	// DefaultWebPort is the default web UI port for wyze-bridge
	// Using 5002 to avoid conflicts with macOS AirPlay (5000) and other services
	// Can be overridden via WYZE_WEB_PORT environment variable
	DefaultWebPort = 5002
)

// BridgeManager manages the wyze-bridge subprocess
type BridgeManager struct {
	bridgePath string
	dataPath   string

	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser

	rtspPort int
	webPort  int

	running   bool
	runningMu sync.RWMutex

	stopCh chan struct{}

	// Startup tracking to suppress log spam during bridge initialization
	startTime        time.Time
	apiReadyLogged   bool
	apiFailureLogged bool
}

// BridgeConfig holds configuration for the wyze-bridge
type BridgeConfig struct {
	Email    string
	Password string
	KeyID    string
	APIKey   string
	TOTPKey  string

	// Optional: filter specific cameras
	Cameras []string

	// Ports
	RTSPPort int // Default: 8556 (avoids conflict with go2rtc on 8554)
	WebPort  int // Default: 5000

	// Data path for persistent storage
	DataPath string
}

// NewBridgeManager creates a new bridge manager
func NewBridgeManager(pluginPath string, config BridgeConfig) *BridgeManager {
	// Priority: config > env var > default
	rtspPort := config.RTSPPort
	if rtspPort == 0 {
		rtspPort = getEnvInt("WYZE_RTSP_PORT", DefaultRTSPPort)
	}

	webPort := config.WebPort
	if webPort == 0 {
		webPort = getEnvInt("WYZE_WEB_PORT", DefaultWebPort)
	}

	dataPath := config.DataPath
	if dataPath == "" {
		dataPath = filepath.Join(pluginPath, "data")
	}

	return &BridgeManager{
		bridgePath: filepath.Join(pluginPath, "wyze-bridge", "app"),
		dataPath:   dataPath,
		rtspPort:   rtspPort,
		webPort:    webPort,
		stopCh:     make(chan struct{}),
	}
}

// Start launches the wyze-bridge subprocess
func (m *BridgeManager) Start(ctx context.Context, config BridgeConfig) error {
	m.runningMu.Lock()
	if m.running {
		m.runningMu.Unlock()
		return nil
	}
	m.runningMu.Unlock()

	// Ensure wyze-bridge Python app is available
	if err := m.ensureWyzeBridge(ctx); err != nil {
		return fmt.Errorf("failed to setup wyze-bridge: %w", err)
	}

	// Ensure data directory exists
	if err := os.MkdirAll(m.dataPath, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	// Ensure MediaMTX is available
	if err := m.ensureMediaMTX(ctx); err != nil {
		return fmt.Errorf("failed to setup MediaMTX: %w", err)
	}

	// Setup TUTK library
	if err := m.setupTUTKLibrary(); err != nil {
		log.Printf("Warning: failed to setup TUTK library: %v", err)
	}

	// Check if Python is available
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		pythonPath, err = exec.LookPath("python")
		if err != nil {
			return fmt.Errorf("python not found: please install Python 3")
		}
	}

	// Install requirements if needed
	if err := m.ensureRequirements(ctx, pythonPath); err != nil {
		log.Printf("Warning: failed to install requirements: %v", err)
	}

	// Build environment variables for wyze-bridge
	// Set paths first - wyze-bridge expects these at import time
	tokenPath := filepath.Join(m.dataPath, "tokens")
	imgPath := filepath.Join(m.dataPath, "img")
	if err := os.MkdirAll(tokenPath, 0755); err != nil {
		return fmt.Errorf("failed to create token directory: %w", err)
	}
	if err := os.MkdirAll(imgPath, 0755); err != nil {
		return fmt.Errorf("failed to create img directory: %w", err)
	}

	// MediaMTX config path
	mtxConfigPath := filepath.Join(m.bridgePath, "mediamtx.yml")

	// Filter out deprecated WEB_PATH from inherited environment
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "WEB_PATH=") {
			env = append(env, e)
		}
	}
	env = append(env,
		fmt.Sprintf("WYZE_EMAIL=%s", config.Email),
		fmt.Sprintf("WYZE_PASSWORD=%s", config.Password),
		// Use WB_ prefix (not deprecated WEB_) for wyze-bridge v2.10+
		fmt.Sprintf("WB_RTSP_PORT=%d", m.rtspPort),
		fmt.Sprintf("WB_PORT=%d", m.webPort),
		// Set MediaMTX ports directly to avoid conflicts with go2rtc (8554/8555)
		fmt.Sprintf("MTX_RTSPADDRESS=:%d", m.rtspPort),
		"MTX_WEBRTCADDRESS=:8561",           // go2rtc uses 8555
		"MTX_WEBRTCICEUDPMUXADDRESS=:8563",  // WebRTC ICE UDP (avoid 8189 conflict)
		"MTX_HLSADDRESS=:8562",              // Avoid any HLS conflicts
		"MTX_RTMPADDRESS=",                  // Disable RTMP (not needed)
		fmt.Sprintf("TOKEN_PATH=%s/", tokenPath),
		fmt.Sprintf("IMG_PATH=%s/", imgPath),
		fmt.Sprintf("MTX_CONFIG=%s", mtxConfigPath),
		"ENABLE_AUDIO=True",
		"ON_DEMAND=False", // Keep streams active
		"SNAPSHOT=API",
		"QUALITY=HD",
		"WB_AUTH=False", // Disable web API auth for internal plugin communication
	)

	if config.KeyID != "" {
		env = append(env, fmt.Sprintf("API_ID=%s", config.KeyID))
	}
	if config.APIKey != "" {
		env = append(env, fmt.Sprintf("API_KEY=%s", config.APIKey))
	}
	if config.TOTPKey != "" {
		env = append(env, fmt.Sprintf("TOTP_KEY=%s", config.TOTPKey))
	}

	// Camera filter
	if len(config.Cameras) > 0 {
		for _, cam := range config.Cameras {
			env = append(env, fmt.Sprintf("FILTER_NAMES=%s", cam))
		}
	}

	// Start the bridge with web UI (frontend.py includes both Flask API and WyzeBridge)
	m.cmd = exec.CommandContext(ctx, pythonPath, "frontend.py")
	m.cmd.Dir = m.bridgePath
	m.cmd.Env = env

	m.stdout, err = m.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	m.stderr, err = m.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start wyze-bridge: %w", err)
	}

	m.runningMu.Lock()
	m.running = true
	m.startTime = time.Now()
	m.apiReadyLogged = false
	m.apiFailureLogged = false
	m.runningMu.Unlock()

	// Start log readers
	go m.readOutput(m.stdout, "stdout")
	go m.readOutput(m.stderr, "stderr")

	// Wait for bridge to be ready
	if err := m.waitForReady(ctx); err != nil {
		_ = m.Stop()
		return fmt.Errorf("bridge failed to start: %w", err)
	}

	log.Printf("Wyze-bridge started successfully on RTSP port %d", m.rtspPort)
	return nil
}

// ensureRequirements installs Python dependencies if needed
func (m *BridgeManager) ensureRequirements(ctx context.Context, pythonPath string) error {
	reqPath := filepath.Join(m.bridgePath, "requirements.txt")
	if _, err := os.Stat(reqPath); os.IsNotExist(err) {
		return nil // No requirements file
	}

	// Check if flask is importable (quick check if deps are installed)
	checkCmd := exec.CommandContext(ctx, pythonPath, "-c", "import flask")
	if checkCmd.Run() == nil {
		return nil // Already installed
	}

	log.Println("Installing wyze-bridge dependencies...")

	cmd := exec.CommandContext(ctx, pythonPath, "-m", "pip", "install", "-r", reqPath, "--quiet")
	cmd.Dir = m.bridgePath

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pip install failed: %w\n%s", err, string(output))
	}

	return nil
}

// waitForReady waits for the bridge to be ready to accept connections
func (m *BridgeManager) waitForReady(ctx context.Context) error {
	timeout := time.After(60 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	// Use 127.0.0.1 explicitly to avoid IPv6 issues
	// Some systems try [::1] first which may not work with wyze-bridge
	url := fmt.Sprintf("http://127.0.0.1:%d/api/cameras", m.webPort)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for bridge to start")
		case <-ticker.C:
			resp, err := http.Get(url)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == 200 || resp.StatusCode == 401 {
					// Bridge is ready (401 means auth required but server is up)
					return nil
				}
			}

			// Check if process is still running
			m.runningMu.RLock()
			running := m.running
			m.runningMu.RUnlock()

			if !running {
				return fmt.Errorf("bridge process exited unexpectedly")
			}
		}
	}
}

// Stop shuts down the wyze-bridge subprocess
func (m *BridgeManager) Stop() error {
	m.runningMu.Lock()
	if !m.running {
		m.runningMu.Unlock()
		return nil
	}
	m.running = false
	m.runningMu.Unlock()

	close(m.stopCh)

	if m.cmd != nil && m.cmd.Process != nil {
		// Send SIGTERM first
		_ = m.cmd.Process.Signal(os.Interrupt)

		// Wait with timeout
		done := make(chan error, 1)
		go func() {
			done <- m.cmd.Wait()
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = m.cmd.Process.Kill()
		}
	}

	log.Println("Wyze-bridge stopped")
	return nil
}

// IsRunning returns whether the bridge is running
func (m *BridgeManager) IsRunning() bool {
	m.runningMu.RLock()
	defer m.runningMu.RUnlock()
	return m.running
}

// GetRTSPURL returns the RTSP URL for a camera
func (m *BridgeManager) GetRTSPURL(cameraName string, substream bool) string {
	// wyze-bridge uses lowercase names with hyphens
	name := bridgeSanitizeName(cameraName)
	if substream {
		return fmt.Sprintf("rtsp://localhost:%d/%s_sub", m.rtspPort, name)
	}
	return fmt.Sprintf("rtsp://localhost:%d/%s", m.rtspPort, name)
}

// GetSnapshotURL returns the snapshot URL for a camera
func (m *BridgeManager) GetSnapshotURL(cameraName string) string {
	name := bridgeSanitizeName(cameraName)
	// Use 127.0.0.1 explicitly to avoid IPv6 issues
	return fmt.Sprintf("http://127.0.0.1:%d/img/%s.jpg", m.webPort, name)
}

// bridgeSanitizeName converts a camera name to the format wyze-bridge uses
// e.g., "Pet Cam" -> "pet-cam"
func bridgeSanitizeName(name string) string {
	name = strings.ToLower(name)
	result := ""
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result += string(c)
		} else if c == ' ' || c == '_' {
			result += "-"
		} else if c == '-' {
			result += "-"
		}
	}
	// Remove consecutive hyphens
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	return strings.Trim(result, "-")
}

// readOutput reads and logs output from the bridge process
func (m *BridgeManager) readOutput(r io.Reader, name string) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			log.Printf("[wyze-bridge %s] %s", name, string(buf[:n]))
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[wyze-bridge] %s read error: %v", name, err)
			}
			return
		}

		// Check if we should stop
		select {
		case <-m.stopCh:
			return
		default:
		}
	}
}

// ensureMediaMTX downloads and sets up MediaMTX if not present
func (m *BridgeManager) ensureMediaMTX(ctx context.Context) error {
	mtxPath := filepath.Join(m.bridgePath, "mediamtx")
	mtxConfigPath := filepath.Join(m.bridgePath, "mediamtx.yml")

	// Check if MediaMTX binary exists
	if _, err := os.Stat(mtxPath); err == nil {
		// Check if config exists
		if _, err := os.Stat(mtxConfigPath); err == nil {
			return nil // Already set up
		}
	}

	log.Println("Setting up MediaMTX...")

	// Determine architecture
	arch := runtime.GOARCH
	goos := runtime.GOOS

	var mtxArch string
	switch arch {
	case "amd64":
		mtxArch = "amd64"
	case "arm64":
		mtxArch = "arm64"
	case "arm":
		mtxArch = "armv7"
	default:
		return fmt.Errorf("unsupported architecture: %s", arch)
	}

	var mtxOS string
	switch goos {
	case "linux":
		mtxOS = "linux"
	case "darwin":
		mtxOS = "darwin"
	case "windows":
		mtxOS = "windows"
	default:
		return fmt.Errorf("unsupported OS: %s", goos)
	}

	// Download MediaMTX
	mtxVersion := "1.9.1" // From wyze-bridge .env
	url := fmt.Sprintf("https://github.com/bluenviron/mediamtx/releases/download/v%s/mediamtx_v%s_%s_%s.tar.gz",
		mtxVersion, mtxVersion, mtxOS, mtxArch)

	log.Printf("Downloading MediaMTX from %s", url)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download MediaMTX: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to download MediaMTX: HTTP %d", resp.StatusCode)
	}

	// Extract tarball
	gzr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read gzip: %w", err)
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar: %w", err)
		}

		target := filepath.Join(m.bridgePath, header.Name)

		switch header.Typeflag {
		case tar.TypeReg:
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			_ = f.Close()
		}
	}

	log.Println("MediaMTX setup complete")
	return nil
}

// setupTUTKLibrary sets up the TUTK native library
func (m *BridgeManager) setupTUTKLibrary() error {
	// Determine the correct library file
	arch := runtime.GOARCH
	var libName string
	switch arch {
	case "amd64":
		libName = "lib.amd64"
	case "arm64":
		libName = "lib.arm64"
	case "arm":
		libName = "lib.arm"
	default:
		return fmt.Errorf("unsupported architecture for TUTK: %s", arch)
	}

	srcPath := filepath.Join(m.bridgePath, "lib", libName)
	dstPath := filepath.Join(m.bridgePath, "libIOTCAPIs_ALL.so")

	// Check if source exists
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return fmt.Errorf("TUTK library not found: %s", srcPath)
	}

	// Check if destination already exists
	if _, err := os.Stat(dstPath); err == nil {
		return nil // Already set up
	}

	// Create symlink or copy
	if err := os.Symlink(srcPath, dstPath); err != nil {
		// Fall back to copy if symlink fails
		src, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()

		dst, err := os.Create(dstPath)
		if err != nil {
			return err
		}
		defer func() { _ = dst.Close() }()

		if _, err := io.Copy(dst, src); err != nil {
			return err
		}

		// Make executable
		if err := os.Chmod(dstPath, 0755); err != nil {
			return err
		}
	}

	log.Println("TUTK library setup complete")
	return nil
}

// GetCameras returns the list of cameras from the bridge API
func (m *BridgeManager) GetCameras(ctx context.Context) (map[string]BridgeCamera, error) {
	// Use 127.0.0.1 explicitly to avoid IPv6 issues
	url := fmt.Sprintf("http://127.0.0.1:%d/api/cameras", m.webPort)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get cameras from bridge: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("bridge API returned status %d", resp.StatusCode)
	}

	// Parse response - bridge returns camera list as JSON object keyed by stream name
	var result map[string]BridgeCamera
	decoder := NewJSONDecoder(resp.Body)
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse bridge camera response: %w", err)
	}

	return result, nil
}

// IsCameraConnected checks if a specific camera is connected via the bridge
func (m *BridgeManager) IsCameraConnected(cameraName string) bool {
	if !m.IsRunning() {
		return false
	}

	// Suppress connection checks during startup grace period (30 seconds)
	if time.Since(m.startTime) < 30*time.Second {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cameras, err := m.GetCameras(ctx)
	if err != nil {
		log.Printf("Failed to check camera connection: %v", err)
		return false
	}

	// The bridge uses lowercase names with hyphens or underscores
	// e.g., "Pet Cam" becomes "pet-cam" or "pet_cam"
	normalizedName := normalizeCameraName(cameraName)

	// Check all cameras for a match
	for key, cam := range cameras {
		normalizedKey := normalizeCameraName(key)
		if normalizedKey == normalizedName {
			return cam.Connected
		}
	}

	return false
}

// normalizeCameraName normalizes a camera name for comparison
// Converts to lowercase and replaces spaces/special chars with underscores
func normalizeCameraName(name string) string {
	name = strings.ToLower(name)
	result := ""
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result += string(c)
		} else if c == ' ' || c == '-' || c == '_' {
			result += "_"
		}
	}
	// Remove consecutive underscores
	for strings.Contains(result, "__") {
		result = strings.ReplaceAll(result, "__", "_")
	}
	return strings.Trim(result, "_")
}

// BridgeCamera represents a camera from the bridge API
type BridgeCamera struct {
	Name       string `json:"name_uri"`
	Model      string `json:"product_model"`
	Connected  bool   `json:"connected"`
	Enabled    bool   `json:"enabled"`
	OnDemand   bool   `json:"on_demand"`
	Audio      bool   `json:"audio"`
	Recording  bool   `json:"recording"`
	URI        string `json:"uri"`
	RTSPURI    string `json:"rtsp_uri"`
	HLSURI     string `json:"hls_uri"`
	WebRTCURI  string `json:"webrtc_uri"`
}

// ensureWyzeBridge downloads the wyze-bridge Python app if not present
func (m *BridgeManager) ensureWyzeBridge(ctx context.Context) error {
	bridgeScript := filepath.Join(m.bridgePath, "wyze_bridge.py")

	// Check if wyze-bridge is already installed
	if _, err := os.Stat(bridgeScript); err == nil {
		return nil // Already installed
	}

	log.Println("wyze-bridge not found, downloading...")

	// Get the parent directory of bridgePath (which is pluginPath/wyze-bridge/app)
	// We want to clone to pluginPath/wyze-bridge
	wyzeBridgeDir := filepath.Dir(m.bridgePath) // pluginPath/wyze-bridge

	// Check if git is available
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git not found: please install git or manually clone wyze-bridge to %s", wyzeBridgeDir)
	}

	// Create parent directory if needed
	if err := os.MkdirAll(filepath.Dir(wyzeBridgeDir), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Remove existing incomplete directory if any
	if _, err := os.Stat(wyzeBridgeDir); err == nil {
		log.Println("Removing incomplete wyze-bridge directory...")
		if err := os.RemoveAll(wyzeBridgeDir); err != nil {
			return fmt.Errorf("failed to remove existing directory: %w", err)
		}
	}

	log.Println("Cloning wyze-bridge repository (this may take a moment)...")

	// Clone the wyze-bridge repository
	// Using shallow clone with depth=1 for faster download
	cmd := exec.CommandContext(ctx, gitPath, "clone",
		"--depth", "1",
		"--single-branch",
		"https://github.com/mrlt8/docker-wyze-bridge.git",
		wyzeBridgeDir)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %w\n%s", err, string(output))
	}

	log.Println("wyze-bridge downloaded successfully")

	// Verify the main script exists
	if _, err := os.Stat(bridgeScript); os.IsNotExist(err) {
		return fmt.Errorf("wyze-bridge downloaded but wyze_bridge.py not found at %s", bridgeScript)
	}

	return nil
}

// getEnvInt returns an environment variable as int, or the default if not set/invalid
func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}
