package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

// BridgeManager manages wyze-bridge as a Docker container
// This provides better isolation and simpler dependency management
type BridgeManager struct {
	containerName string
	imageName     string
	dataPath      string

	rtspPort int
	webPort  int

	running   bool
	runningMu sync.RWMutex

	stopCh chan struct{}

	// Startup tracking
	startTime        time.Time
	apiReadyLogged   bool
	apiFailureLogged bool
}

// NewBridgeManager creates a new Docker-based bridge manager
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
		containerName: "spatialnvr-wyze-bridge",
		imageName:     "mrlt8/wyze-bridge:latest",
		dataPath:      dataPath,
		rtspPort:      rtspPort,
		webPort:       webPort,
		stopCh:        make(chan struct{}),
	}
}

// Start launches the wyze-bridge container
func (m *BridgeManager) Start(ctx context.Context, config BridgeConfig) error {
	m.runningMu.Lock()
	if m.running {
		m.runningMu.Unlock()
		return nil
	}
	m.runningMu.Unlock()

	// Check if Docker is available
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not found in PATH - please install Docker")
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

	// Stop any existing container with same name
	log.Println("Stopping any existing wyze-bridge container...")
	stopCmd := exec.CommandContext(ctx, "docker", "rm", "-f", m.containerName)
	_ = stopCmd.Run() // Ignore error if container doesn't exist

	// Pull the latest image
	log.Printf("Pulling wyze-bridge image: %s", m.imageName)
	pullCmd := exec.CommandContext(ctx, "docker", "pull", m.imageName)
	pullCmd.Stdout = os.Stdout
	pullCmd.Stderr = os.Stderr
	if err := pullCmd.Run(); err != nil {
		log.Printf("Warning: failed to pull image (will use cached): %v", err)
	}

	// Build docker run command
	args := []string{
		"run",
		"-d",                          // Detached mode
		"--name", m.containerName,     // Container name
		"--restart", "unless-stopped", // Restart policy

		// Port mappings
		"-p", fmt.Sprintf("%d:8554", m.rtspPort), // RTSP
		"-p", fmt.Sprintf("%d:5000", m.webPort),  // Web UI
		"-p", "8561:8561/tcp",                    // WebRTC HTTP
		"-p", "8563:8563/udp",                    // WebRTC ICE UDP
		"-p", "8562:8888",                        // HLS

		// Volume mounts for persistence
		"-v", fmt.Sprintf("%s:/tokens", tokenPath),
		"-v", fmt.Sprintf("%s:/img", imgPath),

		// Environment variables
		"-e", fmt.Sprintf("WYZE_EMAIL=%s", config.Email),
		"-e", fmt.Sprintf("WYZE_PASSWORD=%s", config.Password),
		"-e", "ENABLE_AUDIO=True",
		"-e", "ON_DEMAND=False",
		"-e", "SNAPSHOT=API",
		"-e", "QUALITY=HD",
		"-e", "WB_AUTH=False",
	}

	// Optional credentials
	if config.KeyID != "" {
		args = append(args, "-e", fmt.Sprintf("API_ID=%s", config.KeyID))
	}
	if config.APIKey != "" {
		args = append(args, "-e", fmt.Sprintf("API_KEY=%s", config.APIKey))
	}
	if config.TOTPKey != "" {
		args = append(args, "-e", fmt.Sprintf("TOTP_KEY=%s", config.TOTPKey))
	}

	// Camera filter
	if len(config.Cameras) > 0 {
		for _, cam := range config.Cameras {
			args = append(args, "-e", fmt.Sprintf("FILTER_NAMES=%s", cam))
		}
	}

	// Add image name at the end
	args = append(args, m.imageName)

	log.Printf("Starting wyze-bridge container: docker %s", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "docker", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to start container: %w\n%s", err, string(output))
	}

	containerID := strings.TrimSpace(string(output))
	if len(containerID) >= 12 {
		log.Printf("Container started with ID: %s", containerID[:12])
	} else {
		log.Printf("Container started with ID: %s", containerID)
	}

	m.runningMu.Lock()
	m.running = true
	m.startTime = time.Now()
	m.apiReadyLogged = false
	m.apiFailureLogged = false
	m.runningMu.Unlock()

	// Start log streamer in background
	go m.streamLogs(ctx)

	// Wait for bridge to be ready
	if err := m.waitForReady(ctx); err != nil {
		_ = m.Stop()
		return fmt.Errorf("bridge failed to start: %w", err)
	}

	log.Printf("Wyze-bridge container started successfully on RTSP port %d", m.rtspPort)
	return nil
}

// streamLogs streams container logs to our log output
func (m *BridgeManager) streamLogs(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "docker", "logs", "-f", m.containerName)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to get container logs: %v", err)
		return
	}
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start log streaming: %v", err)
		return
	}

	go m.readOutput(stdout, "stdout")
	go m.readOutput(stderr, "stderr")

	_ = cmd.Wait()
}

// readOutput reads and logs output from the container
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
	timeout := time.After(90 * time.Second) // Longer timeout for container startup
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	url := fmt.Sprintf("http://127.0.0.1:%d/api/cameras", m.webPort)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for container to start")
		case <-ticker.C:
			// Check if container is still running
			checkCmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", m.containerName)
			output, err := checkCmd.Output()
			if err != nil {
				return fmt.Errorf("container failed to start")
			}
			if strings.TrimSpace(string(output)) != "true" {
				// Get logs for debugging
				logsCmd := exec.CommandContext(ctx, "docker", "logs", "--tail", "50", m.containerName)
				logs, _ := logsCmd.CombinedOutput()
				return fmt.Errorf("container is not running. Last logs:\n%s", string(logs))
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

// Stop shuts down the wyze-bridge container
func (m *BridgeManager) Stop() error {
	m.runningMu.Lock()
	if !m.running {
		m.runningMu.Unlock()
		return nil
	}
	m.running = false
	m.runningMu.Unlock()

	close(m.stopCh)

	log.Println("Stopping wyze-bridge container...")
	cmd := exec.Command("docker", "stop", "-t", "10", m.containerName)
	if err := cmd.Run(); err != nil {
		log.Printf("Warning: failed to stop container gracefully: %v", err)
		// Force remove
		_ = exec.Command("docker", "rm", "-f", m.containerName).Run()
	}

	// Remove the stopped container
	_ = exec.Command("docker", "rm", m.containerName).Run()

	log.Println("Wyze-bridge container stopped")
	return nil
}

// IsRunning returns whether the container is running
func (m *BridgeManager) IsRunning() bool {
	m.runningMu.RLock()
	defer m.runningMu.RUnlock()

	if !m.running {
		return false
	}

	// Also check with Docker
	cmd := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", m.containerName)
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) == "true"
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
