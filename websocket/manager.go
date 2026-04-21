package websocket

import (
	"context"
	"fmt"
	"log"
	"stockbit-haka-haki/auth"
	pb "stockbit-haka-haki/proto"
	"strings"
	"sync"
	"time"
)

// ConnectionManager handles WebSocket connection lifecycle, health monitoring, and reconnection.
type ConnectionManager struct {
	client      *Client
	authManager *auth.AuthManager
	wsURL       string
	lastMsgTime time.Time

	// reconnectMu prevents concurrent reconnections (health monitor + token refresh racing)
	reconnectMu   sync.Mutex
	isReconnecting bool
}

// NewConnectionManager creates a new ConnectionManager.
func NewConnectionManager(wsURL string, authManager *auth.AuthManager) *ConnectionManager {
	return &ConnectionManager{
		wsURL:       wsURL,
		authManager: authManager,
		lastMsgTime: time.Now(),
	}
}

// Connect establishes the initial WebSocket connection using the AuthManager.
func (cm *ConnectionManager) Connect() error {
	accessToken := cm.authManager.GetClient().GetAccessToken()
	fmt.Println("🔌 Connecting to trading WebSocket...")
	cm.client = NewClient(cm.wsURL, accessToken)

	if err := cm.client.Connect(); err != nil {
		return fmt.Errorf("trading WebSocket connection failed: %w", err)
	}
	fmt.Println("✅ Trading WebSocket connected!")

	// Authenticate WebSocket session
	return cm.AuthenticateAndSubscribe()
}

// AuthenticateAndSubscribe handles the WS-specific handshake (getting key and subscribing).
func (cm *ConnectionManager) AuthenticateAndSubscribe() error {
	// Get WebSocket key for subscription (with retry on token expiry)
	fmt.Println("🔑 Fetching WebSocket key...")
	authClient := cm.authManager.GetClient()
	wsKey, err := authClient.GetWebSocketKey()

	if err != nil {
		// If token expired, try to refresh and retry once
		if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "kedaluwarsa") {
			log.Println("⚠️  WebSocket key fetch failed (token expired), refreshing token...")

			// Force refresh via AuthManager's client
			if refreshErr := authClient.RefreshToken(); refreshErr != nil {
				log.Println("⚠️  Token refresh failed, re-authenticating...")
				if loginErr := authClient.Login(); loginErr != nil {
					return fmt.Errorf("failed to re-authenticate: %w", loginErr)
				}
			}

			// Update WebSocket client with new token
			accessToken := authClient.GetAccessToken()
			cm.client = NewClient(cm.wsURL, accessToken) // Re-create client with new token
			if err := cm.client.Connect(); err != nil {
				return fmt.Errorf("websocket reconnection failed: %w", err)
			}

			// Retry getting WebSocket key
			wsKey, err = authClient.GetWebSocketKey()
			if err != nil {
				return fmt.Errorf("failed to get websocket key after token refresh: %w", err)
			}
		} else {
			return fmt.Errorf("failed to get websocket key: %w", err)
		}
	}
	fmt.Println("✅ WebSocket key obtained!")

	// Subscribe to all stocks (wildcard)
	userID := fmt.Sprintf("%d", authClient.GetUserID())
	if err := cm.client.SubscribeToStocks(nil, userID, wsKey); err != nil {
		log.Printf("Warning: Subscription failed: %v", err)
		return err
	}

	return nil
}

// StartPing starts the keep-alive pinger.
func (cm *ConnectionManager) StartPing(interval time.Duration) {
	if cm.client != nil {
		cm.client.StartPing(interval)
	}
}

// ReadMessage reads a message from the WebSocket.
func (cm *ConnectionManager) ReadMessage() (*pb.WebsocketWrapMessageChannel, error) {
	if cm.client == nil {
		return nil, fmt.Errorf("client not connected")
	}
	msg, err := cm.client.ReadMessage()
	if err == nil {
		cm.lastMsgTime = time.Now()
	}
	return msg, err
}

// Close closes the WebSocket connection gracefully.
func (cm *ConnectionManager) Close() error {
	if cm.client != nil {
		return cm.client.Close()
	}
	return nil
}

// safeReconnect prevents concurrent reconnects via mutex.
// Returns false if a reconnect is already in progress (caller should skip).
func (cm *ConnectionManager) safeReconnect(caller string) error {
	cm.reconnectMu.Lock()
	if cm.isReconnecting {
		cm.reconnectMu.Unlock()
		log.Printf("🔄 [%s] Reconnect skipped — another reconnect already in progress", caller)
		return nil
	}
	cm.isReconnecting = true
	cm.reconnectMu.Unlock()

	defer func() {
		cm.reconnectMu.Lock()
		cm.isReconnecting = false
		cm.reconnectMu.Unlock()
	}()

	return cm.Reconnect()
}

// RunHealthMonitor starts a background loop to check connection health.
// Reconnects if no message received for 90 seconds (reduced from 5 minutes).
func (cm *ConnectionManager) RunHealthMonitor(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second) // Check every 30 seconds
	defer ticker.Stop()

	log.Println("💓 WebSocket health monitoring started")

	for {
		select {
		case <-ctx.Done():
			log.Println("🛑 WebSocket health monitoring stopped")
			return
		case <-ticker.C:
			timeSinceLastMessage := time.Since(cm.lastMsgTime)

			// Reconnect if silent for 90 seconds (market sends hundreds of msgs/min)
			if timeSinceLastMessage > 90*time.Second {
				log.Printf("⚠️  No WebSocket message received for %v, reconnecting...", timeSinceLastMessage.Round(time.Second))

				if err := cm.safeReconnect("HealthMonitor"); err != nil {
					log.Printf("❌ WebSocket reconnection failed: %v", err)
				} else {
					log.Println("✅ WebSocket reconnected successfully")
					cm.lastMsgTime = time.Now()
				}
			} else {
				log.Printf("💓 WebSocket healthy, last message %v ago", timeSinceLastMessage.Round(time.Second))
			}
		}
	}
}

// Reconnect attempts to reconnect the WebSocket.
func (cm *ConnectionManager) Reconnect() error {
	log.Println("🔄 Attempting to reconnect in 5s...")
	time.Sleep(5 * time.Second)

	if cm.client != nil {
		_ = cm.client.Close()
	}

	authClient := cm.authManager.GetClient()

	// Try to refresh token before reconnecting
	if err := authClient.RefreshToken(); err != nil {
		log.Printf("⚠️  Token refresh during reconnect failed: %v", err)
	}

	accessToken := authClient.GetAccessToken()
	cm.client = NewClient(cm.wsURL, accessToken)

	if err := cm.client.Connect(); err != nil {
		return fmt.Errorf("websocket connection failed: %w", err)
	}

	// Re-authenticate and re-subscribe
	if err := cm.AuthenticateAndSubscribe(); err != nil {
		return fmt.Errorf("websocket re-subscribe failed: %w", err)
	}

	// Restart ping on new connection
	cm.client.StartPing(25 * time.Second)
	log.Println("✅ Reconnection successful with refreshed token")
	return nil
}

// UpdateToken updates the client connection when token is refreshed externally.
// Uses safeReconnect to avoid racing with health monitor.
func (cm *ConnectionManager) UpdateToken(newToken string) {
	log.Println("🔄 Updating WebSocket connection with refreshed token...")

	if err := cm.safeReconnect("TokenUpdate"); err != nil {
		log.Printf("⚠️  Failed to reconnect WebSocket after token update: %v", err)
	} else {
		log.Println("✅ WebSocket reconnected with new token")
	}
}

