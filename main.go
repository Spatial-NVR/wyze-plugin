// Wyze Plugin for NVR System
// This is a standalone plugin that communicates via JSON-RPC over stdin/stdout
// It uses the TUTK library for direct camera connections (like docker-wyze-bridge)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func main() {
	log.SetOutput(os.Stderr)
	log.Println("Wyze plugin starting...")

	plugin := NewPlugin()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("Failed to parse request: %v", err)
			continue
		}

		resp := plugin.HandleRequest(req)
		respBytes, _ := json.Marshal(resp)
		fmt.Println(string(respBytes))
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Scanner error: %v", err)
	}

	log.Println("Wyze plugin shutting down...")
}

// JSON-RPC types
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      interface{}   `json:"id,omitempty"`
	Result  interface{}   `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Plugin types
type Plugin struct {
	cameras    map[string]*WyzeCamera
	config     PluginConfig
	api        *WyzeAPI
	bridge     *BridgeManager
	pluginPath string
	mu         sync.RWMutex
	ctx        context.Context
	cancel     context.CancelFunc
}

type PluginConfig struct {
	Email    string         `json:"email"`
	Password string         `json:"password"`
	KeyID    string         `json:"key_id,omitempty"`
	APIKey   string         `json:"api_key,omitempty"`
	TOTPKey  string         `json:"totp_key,omitempty"`
	Cameras  []CameraFilter `json:"cameras,omitempty"`

	// Bridge configuration
	RTSPPort int    `json:"rtsp_port,omitempty"` // Default: 8554
	WebPort  int    `json:"web_port,omitempty"`  // Default: 5000
	DataPath string `json:"data_path,omitempty"` // Persistent storage path
}

type CameraFilter struct {
	MAC  string `json:"mac"`
	Name string `json:"name,omitempty"`
}

type CameraConfig struct {
	MAC      string                 `json:"mac"`
	Name     string                 `json:"name,omitempty"`
	Extra    map[string]interface{} `json:"extra,omitempty"`
}

type PluginCamera struct {
	ID           string   `json:"id"`
	PluginID     string   `json:"plugin_id"`
	Name         string   `json:"name"`
	Model        string   `json:"model"`
	Host         string   `json:"host"`
	MainStream   string   `json:"main_stream"`
	SubStream    string   `json:"sub_stream"`
	SnapshotURL  string   `json:"snapshot_url"`
	Capabilities []string `json:"capabilities"`
	Online       bool     `json:"online"`
	LastSeen     string   `json:"last_seen"`
}

type DiscoveredCamera struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Model           string   `json:"model"`
	Manufacturer    string   `json:"manufacturer"`
	Host            string   `json:"host"`
	Port            int      `json:"port"`
	Channels        int      `json:"channels"`
	Capabilities    []string `json:"capabilities"`
	FirmwareVersion string   `json:"firmware_version,omitempty"`
	Serial          string   `json:"serial,omitempty"`
}

type HealthStatus struct {
	State     string                 `json:"state"`
	Message   string                 `json:"message,omitempty"`
	LastCheck string                 `json:"last_check"`
	Details   map[string]interface{} `json:"details,omitempty"`
}

type PTZCommand struct {
	Action    string  `json:"action"`
	Direction float64 `json:"direction,omitempty"`
	Speed     float64 `json:"speed,omitempty"`
	Preset    string  `json:"preset,omitempty"`
}

func NewPlugin() *Plugin {
	// Get the plugin path from executable location
	exePath, _ := os.Executable()
	pluginPath := filepath.Dir(exePath)

	return &Plugin{
		cameras:    make(map[string]*WyzeCamera),
		pluginPath: pluginPath,
	}
}

func (p *Plugin) HandleRequest(req JSONRPCRequest) JSONRPCResponse {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
	}

	ctx := context.Background()
	if p.ctx != nil {
		ctx = p.ctx
	}

	switch req.Method {
	case "initialize":
		var config map[string]interface{}
		if req.Params != nil {
			_ = json.Unmarshal(req.Params, &config)
		}
		if err := p.Initialize(ctx, config); err != nil {
			resp.Error = &JSONRPCError{Code: -32603, Message: err.Error()}
		} else {
			resp.Result = map[string]interface{}{"status": "ok"}
		}

	case "shutdown":
		if err := p.Shutdown(ctx); err != nil {
			resp.Error = &JSONRPCError{Code: -32603, Message: err.Error()}
		} else {
			resp.Result = map[string]interface{}{"status": "ok"}
		}

	case "health":
		resp.Result = p.Health()

	case "discover_cameras":
		cameras, err := p.DiscoverCameras(ctx)
		if err != nil {
			resp.Error = &JSONRPCError{Code: -32603, Message: err.Error()}
		} else {
			resp.Result = cameras
		}

	case "add_camera":
		var config CameraConfig
		if err := json.Unmarshal(req.Params, &config); err != nil {
			resp.Error = &JSONRPCError{Code: -32602, Message: "Invalid params: " + err.Error()}
		} else {
			cam, err := p.AddCamera(ctx, config)
			if err != nil {
				resp.Error = &JSONRPCError{Code: -32603, Message: err.Error()}
			} else {
				resp.Result = cam
			}
		}

	case "remove_camera":
		var params struct {
			CameraID string `json:"camera_id"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &JSONRPCError{Code: -32602, Message: "Invalid params"}
		} else if err := p.RemoveCamera(ctx, params.CameraID); err != nil {
			resp.Error = &JSONRPCError{Code: -32603, Message: err.Error()}
		} else {
			resp.Result = map[string]interface{}{"status": "ok"}
		}

	case "list_cameras":
		resp.Result = p.ListCameras()

	case "get_camera":
		var params struct {
			CameraID string `json:"camera_id"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &JSONRPCError{Code: -32602, Message: "Invalid params"}
		} else if cam := p.GetCamera(params.CameraID); cam != nil {
			resp.Result = cam
		} else {
			resp.Error = &JSONRPCError{Code: -32603, Message: "Camera not found"}
		}

	case "ptz_control":
		var params struct {
			CameraID string     `json:"camera_id"`
			Command  PTZCommand `json:"command"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &JSONRPCError{Code: -32602, Message: "Invalid params"}
		} else if err := p.PTZControl(ctx, params.CameraID, params.Command); err != nil {
			resp.Error = &JSONRPCError{Code: -32603, Message: err.Error()}
		} else {
			resp.Result = map[string]interface{}{"status": "ok"}
		}

	case "get_snapshot":
		var params struct {
			CameraID string `json:"camera_id"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &JSONRPCError{Code: -32602, Message: "Invalid params"}
		} else if data, err := p.GetSnapshot(ctx, params.CameraID); err != nil {
			resp.Error = &JSONRPCError{Code: -32603, Message: err.Error()}
		} else {
			resp.Result = data
		}

	default:
		resp.Error = &JSONRPCError{Code: -32601, Message: "Method not found: " + req.Method}
	}

	return resp
}

func (p *Plugin) Initialize(ctx context.Context, config map[string]interface{}) error {
	p.ctx, p.cancel = context.WithCancel(ctx)

	if err := p.parseConfig(config); err != nil {
		return err
	}

	// Initialize Wyze API
	p.api = NewWyzeAPI(p.config.Email, p.config.Password, p.config.KeyID, p.config.APIKey, p.config.TOTPKey)

	// Login to Wyze
	if err := p.api.Login(ctx); err != nil {
		return fmt.Errorf("failed to login to Wyze: %w", err)
	}

	// Start the wyze-bridge subprocess asynchronously
	// This prevents blocking the main request loop during initialization
	bridgeConfig := BridgeConfig{
		Email:    p.config.Email,
		Password: p.config.Password,
		KeyID:    p.config.KeyID,
		APIKey:   p.config.APIKey,
		TOTPKey:  p.config.TOTPKey,
		RTSPPort: p.config.RTSPPort,
		WebPort:  p.config.WebPort,
		DataPath: p.config.DataPath,
	}

	p.bridge = NewBridgeManager(p.pluginPath, bridgeConfig)
	go func() {
		if err := p.bridge.Start(p.ctx, bridgeConfig); err != nil {
			log.Printf("Warning: failed to start wyze-bridge: %v", err)
			log.Printf("Cameras will not have RTSP streams available")
			// Don't fail - we can still list cameras via API
		} else {
			log.Printf("Wyze-bridge started successfully")
		}
	}()

	// Discover cameras
	devices, err := p.api.GetDevices(ctx)
	if err != nil {
		return fmt.Errorf("failed to get devices: %w", err)
	}

	// Filter and add cameras
	for _, device := range devices {
		if !device.IsCamera() {
			continue
		}

		// Check if camera should be included
		if len(p.config.Cameras) > 0 {
			found := false
			for _, filter := range p.config.Cameras {
				if filter.MAC == device.MAC {
					found = true
					if filter.Name != "" {
						device.Nickname = filter.Name
					}
					break
				}
			}
			if !found {
				continue
			}
		}

		cam := NewWyzeCamera(device, p.api, p.bridge)
		p.mu.Lock()
		p.cameras[device.MAC] = cam
		p.mu.Unlock()

		log.Printf("Added camera: %s (%s)", device.Nickname, device.MAC)
	}

	log.Printf("Plugin initialized with %d cameras", len(p.cameras))
	return nil
}

func (p *Plugin) parseConfig(config map[string]interface{}) error {
	if config == nil {
		return fmt.Errorf("configuration required")
	}

	if email, ok := config["email"].(string); ok {
		p.config.Email = email
	}
	if password, ok := config["password"].(string); ok {
		p.config.Password = password
	}
	if keyID, ok := config["key_id"].(string); ok {
		p.config.KeyID = keyID
	}
	if apiKey, ok := config["api_key"].(string); ok {
		p.config.APIKey = apiKey
	}
	if totpKey, ok := config["totp_key"].(string); ok {
		p.config.TOTPKey = totpKey
	}

	if p.config.Email == "" || p.config.Password == "" {
		return fmt.Errorf("email and password are required")
	}

	if cameras, ok := config["cameras"].([]interface{}); ok {
		for _, c := range cameras {
			if camMap, ok := c.(map[string]interface{}); ok {
				filter := CameraFilter{}
				if mac, ok := camMap["mac"].(string); ok {
					filter.MAC = mac
				}
				if name, ok := camMap["name"].(string); ok {
					filter.Name = name
				}
				if filter.MAC != "" {
					p.config.Cameras = append(p.config.Cameras, filter)
				}
			}
		}
	}

	return nil
}

func (p *Plugin) Shutdown(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}

	// Disconnect all cameras
	p.mu.Lock()
	for _, cam := range p.cameras {
		cam.Disconnect()
	}
	p.mu.Unlock()

	// Stop the wyze-bridge subprocess
	if p.bridge != nil {
		if err := p.bridge.Stop(); err != nil {
			log.Printf("Warning: failed to stop wyze-bridge: %v", err)
		}
	}

	log.Println("Plugin shutdown complete")
	return nil
}

func (p *Plugin) Health() HealthStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	online := 0
	total := len(p.cameras)

	for _, cam := range p.cameras {
		if cam.IsOnline() {
			online++
		}
	}

	bridgeRunning := p.bridge != nil && p.bridge.IsRunning()

	state := "healthy"
	msg := fmt.Sprintf("%d/%d cameras online", online, total)

	if total == 0 {
		state = "unknown"
		msg = "No cameras configured"
	} else if !bridgeRunning {
		state = "unhealthy"
		msg = "Wyze-bridge not running"
	} else if online == 0 {
		state = "unhealthy"
	} else if online < total {
		state = "degraded"
	}

	return HealthStatus{
		State:     state,
		Message:   msg,
		LastCheck: time.Now().Format(time.RFC3339),
		Details: map[string]interface{}{
			"cameras_online":  online,
			"cameras_total":   total,
			"authenticated":   p.api != nil && p.api.IsAuthenticated(),
			"bridge_running":  bridgeRunning,
		},
	}
}

func (p *Plugin) DiscoverCameras(ctx context.Context) ([]DiscoveredCamera, error) {
	if p.api == nil {
		return nil, fmt.Errorf("not initialized")
	}

	devices, err := p.api.GetDevices(ctx)
	if err != nil {
		return nil, err
	}

	var discovered []DiscoveredCamera
	for _, device := range devices {
		if !device.IsCamera() {
			continue
		}

		discovered = append(discovered, DiscoveredCamera{
			ID:              device.MAC,
			Name:            device.Nickname,
			Model:           device.ProductModel,
			Manufacturer:    "Wyze",
			Capabilities:    device.GetCapabilities(),
			FirmwareVersion: device.FirmwareVersion,
			Serial:          device.MAC,
		})
	}

	return discovered, nil
}

func (p *Plugin) AddCamera(ctx context.Context, cfg CameraConfig) (*PluginCamera, error) {
	if p.api == nil {
		return nil, fmt.Errorf("not initialized")
	}

	// Get device info
	devices, err := p.api.GetDevices(ctx)
	if err != nil {
		return nil, err
	}

	var device *WyzeDevice
	for _, d := range devices {
		if d.MAC == cfg.MAC {
			device = &d
			break
		}
	}

	if device == nil {
		return nil, fmt.Errorf("camera not found: %s", cfg.MAC)
	}

	if cfg.Name != "" {
		device.Nickname = cfg.Name
	}

	cam := NewWyzeCamera(*device, p.api, p.bridge)

	p.mu.Lock()
	p.cameras[device.MAC] = cam
	p.mu.Unlock()

	return p.GetCamera(device.MAC), nil
}

func (p *Plugin) RemoveCamera(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	cam, ok := p.cameras[id]
	if !ok {
		return fmt.Errorf("camera not found: %s", id)
	}

	cam.Disconnect()
	delete(p.cameras, id)
	log.Printf("Removed camera: %s", id)
	return nil
}

func (p *Plugin) ListCameras() []PluginCamera {
	p.mu.RLock()
	defer p.mu.RUnlock()

	cameras := make([]PluginCamera, 0, len(p.cameras))
	for _, cam := range p.cameras {
		cameras = append(cameras, cam.ToPluginCamera())
	}
	return cameras
}

func (p *Plugin) GetCamera(id string) *PluginCamera {
	p.mu.RLock()
	defer p.mu.RUnlock()

	cam, ok := p.cameras[id]
	if !ok {
		return nil
	}

	pc := cam.ToPluginCamera()
	return &pc
}

func (p *Plugin) PTZControl(ctx context.Context, cameraID string, cmd PTZCommand) error {
	p.mu.RLock()
	cam, ok := p.cameras[cameraID]
	p.mu.RUnlock()

	if !ok {
		return fmt.Errorf("camera not found: %s", cameraID)
	}

	return cam.PTZControl(ctx, cmd)
}

func (p *Plugin) GetSnapshot(ctx context.Context, cameraID string) (string, error) {
	p.mu.RLock()
	cam, ok := p.cameras[cameraID]
	p.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("camera not found: %s", cameraID)
	}

	return cam.GetSnapshot(ctx)
}
