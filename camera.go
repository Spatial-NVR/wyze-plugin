package main

import (
	"context"
	"encoding/base64"
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

	// Stream ports (when connected via TUTK)
	rtspPort int

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
	return c.online
}

func (c *WyzeCamera) LastSeen() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSeen
}

func (c *WyzeCamera) ToPluginCamera() PluginCamera {
	c.mu.RLock()
	defer c.mu.RUnlock()

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

	return PluginCamera{
		ID:           c.device.MAC,
		PluginID:     "wyze",
		Name:         c.device.Nickname,
		Model:        c.device.ProductModel,
		MainStream:   mainStream,
		SubStream:    subStream,
		SnapshotURL:  snapshotURL,
		Capabilities: c.device.GetCapabilities(),
		Online:       c.online,
		LastSeen:     c.lastSeen.Format(time.RFC3339),
	}
}

func (c *WyzeCamera) getStreamURL(quality string) string {
	// TUTK-based streaming requires a local bridge
	// The stream URL format depends on how docker-wyze-bridge exposes it
	// Typically: rtsp://localhost:8554/{camera_name}

	// Fallback URLs when bridge is not available
	name := sanitizeName(c.device.Nickname)
	if quality == "sub" {
		return fmt.Sprintf("rtsp://localhost:8554/%s_sub", name)
	}
	return fmt.Sprintf("rtsp://localhost:8554/%s", name)
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

// encodeBase64 encodes bytes to base64 string
func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
