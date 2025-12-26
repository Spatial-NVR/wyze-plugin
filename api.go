package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	wyzeAuthURL     = "https://auth-prod.api.wyze.com"
	wyzeAPIURL      = "https://api.wyzecam.com"
	wyzeAppInfo     = "wyze_ios_2.50.0"
	wyzeSCInfo      = "9f275790cab94a72bd206c8876429f3c"
	wyzeAppVer      = "com.hualai.WyzeCam___2.50.0"
	wyzePhoneID     = "wyze_developer_api"
)

// WyzeAPI handles authentication and API calls to Wyze
type WyzeAPI struct {
	email    string
	password string
	keyID    string
	apiKey   string
	totpKey  string

	accessToken  string
	refreshToken string
	tokenExpiry  time.Time

	http *http.Client
	mu   sync.RWMutex
}

// WyzeDevice represents a Wyze device
type WyzeDevice struct {
	MAC             string `json:"mac"`
	Nickname        string `json:"nickname"`
	ProductModel    string `json:"product_model"`
	ProductType     string `json:"product_type"`
	FirmwareVersion string `json:"firmware_ver"`
	IsOnline        bool   `json:"device_online"`
	P2PID           string `json:"p2p_id"`
	P2PType         int    `json:"p2p_type"`
	EnrToken        string `json:"enr"`
	ParentDTLS      int    `json:"parent_dtls"`
}

func (d *WyzeDevice) IsCamera() bool {
	cameraModels := []string{
		"WYZECP1",     // Cam Pan
		"WYZEC1",      // Cam v1
		"WYZEC1-JZ",   // Cam v2
		"WYZE_CAKP2",  // Cam v3
		"HL_CAM3P",    // Cam v3 Pro
		"HL_PAN2",     // Cam Pan v2
		"HL_PAN3",     // Cam Pan v3
		"HL_PANP",     // Cam Pan Pro
		"WYZEDB3",     // Video Doorbell v1
		"GW_BE1",      // Video Doorbell v2
		"GW_GC1",      // Video Doorbell Pro
		"AN_RSCW",     // Cam OG
		"AN_RLT",      // Cam OG Telephoto
		"HL_WCO2",     // Cam Outdoor v2
		"WVOD1",       // Cam Outdoor v1
		"HL_CFL1",     // Cam Floodlight
		"HL_CFL2",     // Cam Floodlight v2
	}

	for _, m := range cameraModels {
		if d.ProductModel == m {
			return true
		}
	}
	return false
}

func (d *WyzeDevice) GetCapabilities() []string {
	caps := []string{"video", "snapshot"}

	model := d.ProductModel

	// Pan cameras have PTZ
	if strings.Contains(model, "PAN") || strings.Contains(model, "CP1") {
		caps = append(caps, "ptz")
	}

	// Most cameras have audio
	if d.IsCamera() {
		caps = append(caps, "audio", "two_way_audio", "motion")
	}

	// Doorbells
	if strings.Contains(model, "DB") || strings.Contains(model, "GW_") {
		caps = append(caps, "doorbell")
	}

	// Outdoor/battery cameras
	if strings.Contains(model, "WCO") || strings.Contains(model, "WVOD") {
		caps = append(caps, "battery")
	}

	// Floodlights
	if strings.Contains(model, "CFL") {
		caps = append(caps, "floodlight")
	}

	return caps
}

func (d *WyzeDevice) SupportsPanTilt() bool {
	return strings.Contains(d.ProductModel, "PAN") || strings.Contains(d.ProductModel, "CP1")
}

func NewWyzeAPI(email, password, keyID, apiKey, totpKey string) *WyzeAPI {
	return &WyzeAPI{
		email:    email,
		password: password,
		keyID:    keyID,
		apiKey:   apiKey,
		totpKey:  totpKey,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (a *WyzeAPI) IsAuthenticated() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.accessToken != "" && time.Now().Before(a.tokenExpiry)
}

func (a *WyzeAPI) Login(ctx context.Context) error {
	// Hash password
	passwordHash := md5Hash(md5Hash(md5Hash(a.password)))

	payload := map[string]interface{}{
		"email":    a.email,
		"password": passwordHash,
	}

	// Use API key auth if provided
	if a.keyID != "" && a.apiKey != "" {
		payload["keyid"] = a.keyID
		payload["apikey"] = a.apiKey
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", wyzeAuthURL+"/api/user/login", bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Keyid", a.keyID)
	req.Header.Set("Apikey", a.apiKey)
	req.Header.Set("User-Agent", "wyze-sdk")

	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		MFAOptions   []string `json:"mfa_options"`
		MFADetails   interface{} `json:"mfa_details"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("failed to parse login response: %w", err)
	}

	// Check if MFA is required
	if len(result.MFAOptions) > 0 {
		if a.totpKey == "" {
			return fmt.Errorf("MFA required but no TOTP key provided")
		}
		// Handle TOTP verification
		if err := a.verifyTOTP(ctx); err != nil {
			return fmt.Errorf("MFA verification failed: %w", err)
		}
	} else if result.AccessToken != "" {
		a.mu.Lock()
		a.accessToken = result.AccessToken
		a.refreshToken = result.RefreshToken
		if result.ExpiresIn > 0 {
			a.tokenExpiry = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
		} else {
			a.tokenExpiry = time.Now().Add(24 * time.Hour)
		}
		a.mu.Unlock()
	} else {
		return fmt.Errorf("login failed: no access token received")
	}

	return nil
}

func (a *WyzeAPI) verifyTOTP(ctx context.Context) error {
	// Generate TOTP code
	code := generateTOTP(a.totpKey)

	payload := map[string]interface{}{
		"mfa_type":      "TotpVerificationCode",
		"verification_id": "",
		"verification_code": code,
	}

	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", wyzeAuthURL+"/user/login", bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return err
	}

	if result.AccessToken == "" {
		return fmt.Errorf("TOTP verification failed")
	}

	a.mu.Lock()
	a.accessToken = result.AccessToken
	a.refreshToken = result.RefreshToken
	a.tokenExpiry = time.Now().Add(24 * time.Hour)
	a.mu.Unlock()

	return nil
}

func (a *WyzeAPI) ensureAuthenticated(ctx context.Context) error {
	if a.IsAuthenticated() {
		return nil
	}
	return a.Login(ctx)
}

func (a *WyzeAPI) GetDevices(ctx context.Context) ([]WyzeDevice, error) {
	if err := a.ensureAuthenticated(ctx); err != nil {
		return nil, err
	}

	a.mu.RLock()
	token := a.accessToken
	a.mu.RUnlock()

	payload := map[string]interface{}{
		"access_token":  token,
		"app_name":      wyzeAppInfo,
		"app_ver":       wyzeAppVer,
		"app_version":   wyzeAppVer,
		"phone_id":      wyzePhoneID,
		"phone_system_type": "1",
		"sc":            wyzeSCInfo,
		"sv":            "9d74946e652647e9b6c9d59326aef104",
		"ts":            time.Now().UnixMilli(),
	}

	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", wyzeAPIURL+"/app/v2/home_page/get_object_list", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wyze-sdk")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			DeviceList []WyzeDevice `json:"device_list"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse device list: %w", err)
	}

	if result.Code != "1" {
		return nil, fmt.Errorf("API error: %s", result.Msg)
	}

	return result.Data.DeviceList, nil
}

func (a *WyzeAPI) GetP2PToken(ctx context.Context, mac string) (string, error) {
	if err := a.ensureAuthenticated(ctx); err != nil {
		return "", err
	}

	a.mu.RLock()
	token := a.accessToken
	a.mu.RUnlock()

	payload := map[string]interface{}{
		"access_token": token,
		"device_mac":   mac,
		"app_name":     wyzeAppInfo,
		"app_ver":      wyzeAppVer,
		"sc":           wyzeSCInfo,
		"sv":           "9d74946e652647e9b6c9d59326aef104",
		"ts":           time.Now().UnixMilli(),
	}

	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", wyzeAPIURL+"/app/v2/device/get_property_list", bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		Code string `json:"code"`
		Data struct {
			PropertyList []struct {
				PID   string `json:"pid"`
				Value string `json:"value"`
			} `json:"property_list"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", err
	}

	for _, prop := range result.Data.PropertyList {
		if prop.PID == "P3" {
			return prop.Value, nil
		}
	}

	return "", fmt.Errorf("P2P token not found")
}

// Helper functions
func md5Hash(s string) string {
	hash := md5.Sum([]byte(s))
	return hex.EncodeToString(hash[:])
}

func generateTOTP(secret string) string {
	// Simplified TOTP - in production use a proper TOTP library
	// This is a placeholder
	return "000000"
}
