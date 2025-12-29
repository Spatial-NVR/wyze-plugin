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

const (
	// DefaultRTSPPort is the default RTSP port for wyze-bridge
	// Uses 8564 to avoid conflict with go2rtc (8554/8555)
	DefaultRTSPPort = 8564

	// DefaultWebPort is the default web UI port for wyze-bridge
	// Using 5002 to avoid conflicts with macOS AirPlay (5000)
	DefaultWebPort = 5002

	// WyzeBridgeVersion is the version of wyze-bridge to download
	WyzeBridgeVersion = "2.10.2"

	// WyzeBridgeRepo is the GitHub repo for wyze-bridge releases
	WyzeBridgeRepo = "mrlt8/docker-wyze-bridge"

	// TUTKLibraryURL is the URL to download the TUTK library for P2P camera connections
	// This is required for the wyzecam library to work
	TUTKLibraryURL = "https://github.com/mrlt8/docker-wyze-bridge/raw/main/docker/tutk/lib/x86_64/libIOTCAPIs_ALL.so"
)

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
	RTSPPort int
	WebPort  int

	// Data path for persistent storage
	DataPath string
}

// BridgeCamera represents a camera from the bridge API
type BridgeCamera struct {
	Name      string `json:"name_uri"`
	Model     string `json:"product_model"`
	Connected bool   `json:"connected"`
	Enabled   bool   `json:"enabled"`
	OnDemand  bool   `json:"on_demand"`
	Audio     bool   `json:"audio"`
	Recording bool   `json:"recording"`
	URI       string `json:"uri"`
	RTSPURI   string `json:"rtsp_uri"`
	HLSURI    string `json:"hls_uri"`
	WebRTCURI string `json:"webrtc_uri"`
}

// BridgeManager manages wyze-bridge as a subprocess
// Downloads and runs wyze-bridge Python application (self-contained, like Scrypted)
type BridgeManager struct {
	pluginPath string // Plugin's installation directory
	bridgePath string // Path to downloaded wyze-bridge
	dataPath   string
	rtspPort   int
	webPort    int

	cmd       *exec.Cmd
	running   bool
	runningMu sync.RWMutex

	stopCh chan struct{}

	// Startup tracking
	startTime        time.Time
	apiReadyLogged   bool
	apiFailureLogged bool
}

// NewBridgeManager creates a new subprocess-based bridge manager
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

	// wyze-bridge is bundled with the plugin
	bridgePath := filepath.Join(pluginPath, "wyze-bridge")

	return &BridgeManager{
		pluginPath: pluginPath,
		bridgePath: bridgePath,
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

	// Ensure wyze-bridge is downloaded and available
	if err := m.ensureWyzeBridge(ctx); err != nil {
		return fmt.Errorf("failed to setup wyze-bridge: %w", err)
	}

	// Check if wyze-bridge is available
	wyzeBridgeScript := filepath.Join(m.bridgePath, "app", "wyze-bridge")
	if _, err := os.Stat(wyzeBridgeScript); os.IsNotExist(err) {
		// Try alternative location
		wyzeBridgeScript = filepath.Join(m.bridgePath, "app", "run.py")
		if _, err := os.Stat(wyzeBridgeScript); os.IsNotExist(err) {
			return fmt.Errorf("wyze-bridge not found at %s after download", m.bridgePath)
		}
	}

	// Ensure data directories exist
	tokenPath := filepath.Join(m.dataPath, "tokens")
	imgPath := filepath.Join(m.dataPath, "img")
	if err := os.MkdirAll(tokenPath, 0755); err != nil {
		return fmt.Errorf("failed to create token directory: %w", err)
	}
	if err := os.MkdirAll(imgPath, 0755); err != nil {
		return fmt.Errorf("failed to create img directory: %w", err)
	}

	// Build environment variables for wyze-bridge
	env := os.Environ()

	// Add TUTK library path to LD_LIBRARY_PATH for P2P connections
	libDir := filepath.Join(m.bridgePath, "lib")
	ldPath := os.Getenv("LD_LIBRARY_PATH")
	if ldPath != "" {
		ldPath = libDir + ":" + ldPath
	} else {
		ldPath = libDir
	}

	env = append(env,
		fmt.Sprintf("LD_LIBRARY_PATH=%s", ldPath),
		fmt.Sprintf("WYZE_EMAIL=%s", config.Email),
		fmt.Sprintf("WYZE_PASSWORD=%s", config.Password),
		"ENABLE_AUDIO=True",
		"ON_DEMAND=False",
		"SNAPSHOT=API",
		"QUALITY=HD",
		"WB_AUTH=False",
		fmt.Sprintf("RTSP_PORT=%d", m.rtspPort),
		fmt.Sprintf("WEB_PORT=%d", m.webPort),
		fmt.Sprintf("TOKEN_PATH=%s", tokenPath),
		fmt.Sprintf("IMG_PATH=%s", imgPath),
	)

	// Optional credentials
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
		env = append(env, fmt.Sprintf("FILTER_NAMES=%s", strings.Join(config.Cameras, ",")))
	}

	// Determine how to run wyze-bridge
	var cmd *exec.Cmd
	if strings.HasSuffix(wyzeBridgeScript, ".py") {
		cmd = exec.CommandContext(ctx, "python3", wyzeBridgeScript)
	} else {
		cmd = exec.CommandContext(ctx, wyzeBridgeScript)
	}
	cmd.Dir = filepath.Join(m.bridgePath, "app")
	cmd.Env = env

	// Capture output
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	log.Printf("Starting wyze-bridge subprocess from %s", wyzeBridgeScript)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start wyze-bridge: %w", err)
	}

	m.cmd = cmd
	m.runningMu.Lock()
	m.running = true
	m.startTime = time.Now()
	m.apiReadyLogged = false
	m.apiFailureLogged = false
	m.stopCh = make(chan struct{})
	m.runningMu.Unlock()

	// Stream logs in background
	go m.readOutput(stdout, "stdout")
	go m.readOutput(stderr, "stderr")

	// Monitor process in background
	go func() {
		err := cmd.Wait()
		m.runningMu.Lock()
		m.running = false
		m.runningMu.Unlock()
		if err != nil {
			log.Printf("wyze-bridge process exited with error: %v", err)
		} else {
			log.Println("wyze-bridge process exited normally")
		}
	}()

	// Wait for bridge to be ready
	if err := m.waitForReady(ctx); err != nil {
		_ = m.Stop()
		return fmt.Errorf("bridge failed to start: %w", err)
	}

	log.Printf("Wyze-bridge started successfully on RTSP port %d", m.rtspPort)
	return nil
}

// readOutput reads and logs output from the subprocess
func (m *BridgeManager) readOutput(r io.Reader, name string) {
	buf := make([]byte, 4096)
	for {
		select {
		case <-m.stopCh:
			return
		default:
		}

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
	}
}

// waitForReady waits for the bridge API to be ready
func (m *BridgeManager) waitForReady(ctx context.Context) error {
	timeout := time.After(90 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	url := fmt.Sprintf("http://127.0.0.1:%d/api/cameras", m.webPort)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for wyze-bridge to start")
		case <-ticker.C:
			// Check if process is still running
			if !m.IsRunning() {
				return fmt.Errorf("wyze-bridge process exited unexpectedly")
			}

			// Check API
			resp, err := http.Get(url)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == 200 || resp.StatusCode == 401 {
					return nil
				}
			}
		}
	}
}

// ensureWyzeBridge downloads wyze-bridge if not already present
// This makes the plugin self-contained (like Scrypted plugins)
func (m *BridgeManager) ensureWyzeBridge(ctx context.Context) error {
	// Check if wyze-bridge is already installed
	appDir := filepath.Join(m.bridgePath, "app")
	versionFile := filepath.Join(m.bridgePath, ".version")

	// Check version file to see if we have the right version
	if data, err := os.ReadFile(versionFile); err == nil {
		if strings.TrimSpace(string(data)) == WyzeBridgeVersion {
			// Already have the right version
			if _, err := os.Stat(appDir); err == nil {
				log.Printf("wyze-bridge %s already installed", WyzeBridgeVersion)
				return nil
			}
		}
	}

	log.Printf("Downloading wyze-bridge %s...", WyzeBridgeVersion)

	// Create the bridge directory
	if err := os.MkdirAll(m.bridgePath, 0755); err != nil {
		return fmt.Errorf("failed to create bridge directory: %w", err)
	}

	// Download wyze-bridge source from GitHub
	// We download the source tarball and extract the app directory
	url := fmt.Sprintf("https://github.com/%s/archive/refs/tags/v%s.tar.gz", WyzeBridgeRepo, WyzeBridgeVersion)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download wyze-bridge: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to download wyze-bridge: HTTP %d", resp.StatusCode)
	}

	// Extract tarball
	if err := m.extractTarGz(resp.Body, m.bridgePath); err != nil {
		return fmt.Errorf("failed to extract wyze-bridge: %w", err)
	}

	// The archive extracts to docker-wyze-bridge-X.X.X/app, move it to bridgePath/app
	extractedDir := filepath.Join(m.bridgePath, fmt.Sprintf("docker-wyze-bridge-%s", WyzeBridgeVersion))
	extractedApp := filepath.Join(extractedDir, "app")

	// Move app directory to final location
	if _, err := os.Stat(appDir); err == nil {
		if err := os.RemoveAll(appDir); err != nil {
			return fmt.Errorf("failed to remove old app directory: %w", err)
		}
	}
	if err := os.Rename(extractedApp, appDir); err != nil {
		return fmt.Errorf("failed to move app directory: %w", err)
	}

	// Clean up extracted directory
	_ = os.RemoveAll(extractedDir)

	// Install Python dependencies
	if err := m.installDependencies(ctx); err != nil {
		log.Printf("Warning: failed to install some dependencies: %v", err)
		// Continue anyway - some may already be installed
	}

	// Download TUTK library for P2P connections (x86_64 only for now)
	if runtime.GOARCH == "amd64" {
		if err := m.downloadTUTKLibrary(ctx); err != nil {
			log.Printf("Warning: failed to download TUTK library: %v", err)
			log.Printf("P2P camera connections may not work")
		}
	} else {
		log.Printf("TUTK library not available for %s architecture - P2P connections may not work", runtime.GOARCH)
	}

	// Write version file
	if err := os.WriteFile(versionFile, []byte(WyzeBridgeVersion), 0644); err != nil {
		log.Printf("Warning: failed to write version file: %v", err)
	}

	// Make scripts executable
	_ = os.Chmod(filepath.Join(appDir, "wyze-bridge"), 0755)
	_ = os.Chmod(filepath.Join(appDir, "run.py"), 0755)

	log.Printf("wyze-bridge %s installed successfully", WyzeBridgeVersion)
	return nil
}

// downloadTUTKLibrary downloads the TUTK library required for P2P camera connections
func (m *BridgeManager) downloadTUTKLibrary(ctx context.Context) error {
	libDir := filepath.Join(m.bridgePath, "lib")
	libPath := filepath.Join(libDir, "libIOTCAPIs_ALL.so")

	// Check if already downloaded
	if _, err := os.Stat(libPath); err == nil {
		log.Printf("TUTK library already present")
		return nil
	}

	log.Printf("Downloading TUTK library for P2P connections...")

	if err := os.MkdirAll(libDir, 0755); err != nil {
		return fmt.Errorf("failed to create lib directory: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", TUTKLibraryURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download TUTK library: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to download TUTK library: HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(libPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("failed to create library file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("failed to write library file: %w", err)
	}

	log.Printf("TUTK library downloaded successfully")
	return nil
}

// extractTarGz extracts a tar.gz archive
func (m *BridgeManager) extractTarGz(r io.Reader, dest string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(dest, header.Name)

		// Security check: prevent path traversal
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid file path: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(header.Mode))
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

	return nil
}

// installDependencies installs Python dependencies for wyze-bridge
func (m *BridgeManager) installDependencies(ctx context.Context) error {
	appDir := filepath.Join(m.bridgePath, "app")
	requirementsFile := filepath.Join(appDir, "requirements.txt")

	// Check if requirements.txt exists
	if _, err := os.Stat(requirementsFile); os.IsNotExist(err) {
		log.Printf("No requirements.txt found, skipping dependency installation")
		return nil
	}

	log.Printf("Installing Python dependencies...")

	// Determine pip command
	pip := "pip3"
	if runtime.GOOS == "darwin" {
		// On macOS, might need to use python3 -m pip
		pip = "python3"
	}

	var cmd *exec.Cmd
	if pip == "python3" {
		cmd = exec.CommandContext(ctx, pip, "-m", "pip", "install", "--user", "-r", requirementsFile)
	} else {
		cmd = exec.CommandContext(ctx, pip, "install", "--user", "-r", requirementsFile)
	}

	cmd.Dir = appDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("pip install output: %s", string(output))
		return fmt.Errorf("pip install failed: %w", err)
	}

	log.Printf("Python dependencies installed successfully")
	return nil
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
		log.Println("Stopping wyze-bridge subprocess...")
		// Send SIGTERM first
		if err := m.cmd.Process.Signal(os.Interrupt); err != nil {
			log.Printf("Failed to send interrupt signal: %v", err)
		}

		// Wait briefly for graceful shutdown
		done := make(chan struct{})
		go func() {
			_ = m.cmd.Wait()
			close(done)
		}()

		select {
		case <-done:
			log.Println("wyze-bridge stopped gracefully")
		case <-time.After(10 * time.Second):
			log.Println("wyze-bridge did not stop gracefully, killing...")
			_ = m.cmd.Process.Kill()
		}
	}

	log.Println("Wyze-bridge subprocess stopped")
	return nil
}

// IsRunning returns whether the subprocess is running
func (m *BridgeManager) IsRunning() bool {
	m.runningMu.RLock()
	defer m.runningMu.RUnlock()
	return m.running
}

// GetRTSPURL returns the RTSP URL for a camera
func (m *BridgeManager) GetRTSPURL(cameraName string, substream bool) string {
	name := bridgeSanitizeName(cameraName)
	if substream {
		return fmt.Sprintf("rtsp://localhost:%d/%s_sub", m.rtspPort, name)
	}
	return fmt.Sprintf("rtsp://localhost:%d/%s", m.rtspPort, name)
}

// GetSnapshotURL returns the snapshot URL for a camera
func (m *BridgeManager) GetSnapshotURL(cameraName string) string {
	name := bridgeSanitizeName(cameraName)
	return fmt.Sprintf("http://127.0.0.1:%d/img/%s.jpg", m.webPort, name)
}

// GetCameras returns the list of cameras from the bridge API
func (m *BridgeManager) GetCameras(ctx context.Context) (map[string]BridgeCamera, error) {
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

	var result map[string]BridgeCamera
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse bridge camera response: %w", err)
	}

	return result, nil
}

// IsCameraConnected checks if a specific camera is connected
func (m *BridgeManager) IsCameraConnected(cameraName string) bool {
	if !m.IsRunning() {
		return false
	}

	// Suppress during startup
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

	normalizedName := normalizeCameraName(cameraName)

	for key, cam := range cameras {
		normalizedKey := normalizeCameraName(key)
		if normalizedKey == normalizedName {
			return cam.Connected
		}
	}

	return false
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

// normalizeCameraName normalizes a camera name for comparison
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

// getEnvInt returns an environment variable as int, or the default if not set/invalid
func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}
