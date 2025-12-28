package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// WyzeCamera represents a connected Wyze camera
type WyzeCamera struct {
	device WyzeDevice
	api    *WyzeAPI
	bridge *BridgeManager

	// TUTK connection state
	connected bool
	p2pToken  string

	online   bool
	lastSeen time.Time

	mu sync.RWMutex
}

func NewWyzeCamera(device WyzeDevice, api *WyzeAPI, bridge *BridgeManager) *WyzeCamera {
	return &WyzeCamera{
		device:   device,
		api:      api,
		bridge:   bridge,
		online:   device.IsOnline,
		lastSeen: time.Now(),
	}
}

func (c *WyzeCamera) ID() string      { return c.device.MAC }
func (c *WyzeCamera) Name() string    { return c.device.Nickname }
func (c *WyzeCamera) Model() string   { return c.device.ProductModel }

func (c *WyzeCamera) IsOnline() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Check if bridge reports camera as connected
	if c.bridge != nil && c.bridge.IsCameraConnected(c.device.Nickname) {
		return true
	}

	// Fall back to stored online status
	return c.online
}

func (c *WyzeCamera) LastSeen() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSeen
}

func (c *WyzeCamera) ToPluginCamera() PluginCamera {
	// Generate stream URLs from the bridge
	var mainStream, subStream, snapshotURL string
	if c.bridge != nil {
		mainStream = c.bridge.GetRTSPURL(c.device.Nickname, false)
		subStream = c.bridge.GetRTSPURL(c.device.Nickname, true)
		snapshotURL = c.bridge.GetSnapshotURL(c.device.Nickname)
	} else {
		// Fallback to default URLs
		mainStream = c.getStreamURL("main")
		subStream = c.getStreamURL("sub")
	}

	// Check online status (this queries the bridge if available)
	online := c.IsOnline()

	c.mu.RLock()
	lastSeen := c.lastSeen
	c.mu.RUnlock()

	// Update lastSeen if online
	if online {
		lastSeen = time.Now()
		c.mu.Lock()
		c.lastSeen = lastSeen
		c.mu.Unlock()
	}

	return PluginCamera{
		ID:           c.device.MAC,
		PluginID:     "wyze",
		Name:         c.device.Nickname,
		Model:        c.device.ProductModel,
		MainStream:   mainStream,
		SubStream:    subStream,
		SnapshotURL:  snapshotURL,
		Capabilities: c.device.GetCapabilities(),
		Online:       online,
		LastSeen:     lastSeen.Format(time.RFC3339),
	}
}

func (c *WyzeCamera) getStreamURL(quality string) string {
	// TUTK-based streaming requires a local bridge
	// The stream URL format depends on how docker-wyze-bridge exposes it
	// Port from WYZE_RTSP_PORT env var, defaults to 8556 (avoids go2rtc on 8554)

	port := getEnvInt("WYZE_RTSP_PORT", DefaultRTSPPort)
	name := sanitizeName(c.device.Nickname)
	if quality == "sub" {
		return fmt.Sprintf("rtsp://localhost:%d/%s_sub", port, name)
	}
	return fmt.Sprintf("rtsp://localhost:%d/%s", port, name)
}

func (c *WyzeCamera) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}

	// Get P2P token for TUTK connection
	token, err := c.api.GetP2PToken(ctx, c.device.MAC)
	if err != nil {
		return fmt.Errorf("failed to get P2P token: %w", err)
	}

	c.p2pToken = token
	c.connected = true
	c.online = true
	c.lastSeen = time.Now()

	log.Printf("Connected to camera: %s", c.device.Nickname)
	return nil
}

func (c *WyzeCamera) Disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return
	}

	c.connected = false
	log.Printf("Disconnected from camera: %s", c.device.Nickname)
}

func (c *WyzeCamera) PTZControl(ctx context.Context, cmd PTZCommand) error {
	if !c.device.SupportsPanTilt() {
		return fmt.Errorf("camera does not support PTZ")
	}

	// PTZ control via Wyze API
	// This would send commands through the Wyze cloud API
	action := ""
	switch cmd.Action {
	case "pan":
		if cmd.Direction < 0 {
			action = "left"
		} else {
			action = "right"
		}
	case "tilt":
		if cmd.Direction < 0 {
			action = "down"
		} else {
			action = "up"
		}
	case "stop":
		action = "stop"
	case "preset":
		// Wyze doesn't have traditional presets
		return fmt.Errorf("presets not supported on Wyze cameras")
	default:
		return fmt.Errorf("unknown action: %s", cmd.Action)
	}

	log.Printf("PTZ command for %s: %s", c.device.Nickname, action)

	// TODO: Implement actual PTZ control via Wyze API or TUTK
	// This requires sending commands through the P2P connection

	return nil
}

func (c *WyzeCamera) GetSnapshot(ctx context.Context) (string, error) {
	// Get snapshot from wyze-bridge if available
	if c.bridge != nil {
		snapshotURL := c.bridge.GetSnapshotURL(c.device.Nickname)
		return snapshotURL, nil
	}

	// Fallback: Wyze cameras don't have a direct snapshot API
	return "", fmt.Errorf("snapshot not available - wyze-bridge not running")
}

// GetTUTKConfig returns the configuration needed for TUTK connection
func (c *WyzeCamera) GetTUTKConfig() map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return map[string]interface{}{
		"p2p_id":    c.device.P2PID,
		"p2p_type":  c.device.P2PType,
		"enr":       c.device.EnrToken,
		"mac":       c.device.MAC,
		"model":     c.device.ProductModel,
		"dtls":      c.device.ParentDTLS,
	}
}

// Helper to sanitize camera name for use in URLs
func sanitizeName(name string) string {
	result := ""
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			result += string(c)
		} else if c == ' ' || c == '-' || c == '_' {
			result += "_"
		}
	}
	return result
}
