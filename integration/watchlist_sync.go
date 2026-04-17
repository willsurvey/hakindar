package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"stockbit-haka-haki/cache"
)

// WatchlistEntry represents a screener-generated entry plan for a stock
type WatchlistEntry struct {
	Ticker     string  `json:"ticker"`
	Entry1     float64 `json:"entry_1"`
	Entry2     float64 `json:"entry_2,omitempty"`
	StopLoss   float64 `json:"stop_loss,omitempty"`
	Target1    float64 `json:"target_1,omitempty"`
	Target2    float64 `json:"target_2,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Reason     string  `json:"reason,omitempty"`
	Strategy   string  `json:"strategy,omitempty"`
}

// WatchlistSync loads and manages screener watchlist data from Redis
// The Python screener publishes results to Redis keys like:
//   - watchlist:<date>:<ticker> = JSON entry plan
//   - watchlist:date = current date string
//   - watchlist:count = number of stocks
//   - ihsg:safe = "1" or "0"
//   - ihsg:trend = "BULLISH", "BEARISH", "NEUTRAL"
type WatchlistSync struct {
	redis     *cache.RedisClient
	mu        sync.RWMutex
	watchlist map[string]*WatchlistEntry // key: ticker
	ihsgSafe  bool
	ihsgTrend string
	lastSync  time.Time
}

// NewWatchlistSync creates a new watchlist synchronizer
func NewWatchlistSync(redis *cache.RedisClient) *WatchlistSync {
	return &WatchlistSync{
		redis:     redis,
		watchlist: make(map[string]*WatchlistEntry),
		ihsgSafe:  true, // Default: safe until proven otherwise
		ihsgTrend: "UNKNOWN",
	}
}

// Start begins periodic watchlist synchronization from Redis
func (ws *WatchlistSync) Start(ctx context.Context) {
	log.Println("📋 Watchlist Sync started")

	// Initial sync
	ws.sync()

	// Sync every 5 minutes
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ws.sync()
		case <-ctx.Done():
			log.Println("📋 Watchlist Sync stopped")
			return
		}
	}
}

// sync loads the latest watchlist from Redis
func (ws *WatchlistSync) sync() {
	if ws.redis == nil {
		return
	}

	ctx := context.Background()

	// Load IHSG safety flag
	var ihsgSafe string
	if err := ws.redis.Get(ctx, "ihsg:safe", &ihsgSafe); err == nil {
		ws.mu.Lock()
		ws.ihsgSafe = ihsgSafe == "1"
		ws.mu.Unlock()
	}

	// Load IHSG trend
	var ihsgTrend string
	if err := ws.redis.Get(ctx, "ihsg:trend", &ihsgTrend); err == nil {
		ws.mu.Lock()
		ws.ihsgTrend = ihsgTrend
		ws.mu.Unlock()
	}

	// Load watchlist date
	var watchlistDate string
	if err := ws.redis.Get(ctx, "watchlist:date", &watchlistDate); err != nil {
		return // No watchlist available yet
	}

	// Load watchlist count
	var countStr string
	if err := ws.redis.Get(ctx, "watchlist:count", &countStr); err != nil {
		return
	}

	// Load individual watchlist entries
	// We scan for keys matching watchlist:<date>:*
	newWatchlist := make(map[string]*WatchlistEntry)

	// Try common tickers from screener output (max 10)
	// The screener publishes keys like watchlist:2026-04-18:BBRI
	// We need to scan or the screener should also publish a ticker list
	var tickerList string
	if err := ws.redis.Get(ctx, fmt.Sprintf("watchlist:%s:tickers", watchlistDate), &tickerList); err == nil {
		tickers := strings.Split(tickerList, ",")
		for _, ticker := range tickers {
			ticker = strings.TrimSpace(ticker)
			if ticker == "" {
				continue
			}

			key := fmt.Sprintf("watchlist:%s:%s", watchlistDate, ticker)
			var entryJSON string
			if err := ws.redis.Get(ctx, key, &entryJSON); err == nil {
				var entry WatchlistEntry
				if err := json.Unmarshal([]byte(entryJSON), &entry); err == nil {
					entry.Ticker = ticker
					newWatchlist[ticker] = &entry
				}
			}
		}
	}

	if len(newWatchlist) > 0 {
		ws.mu.Lock()
		ws.watchlist = newWatchlist
		ws.lastSync = time.Now()
		ws.mu.Unlock()
		log.Printf("📋 Watchlist synced: %d stocks, IHSG safe=%v trend=%s",
			len(newWatchlist), ws.ihsgSafe, ws.ihsgTrend)
	}
}

// IsIHSGSafe returns whether the IHSG is in a safe state for buying
func (ws *WatchlistSync) IsIHSGSafe() bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.ihsgSafe
}

// GetIHSGTrend returns the current IHSG trend classification
func (ws *WatchlistSync) GetIHSGTrend() string {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.ihsgTrend
}

// GetWatchlistEntry returns the entry plan for a specific ticker
// Returns nil if the ticker is not in today's watchlist
func (ws *WatchlistSync) GetWatchlistEntry(ticker string) *WatchlistEntry {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.watchlist[ticker]
}

// IsInWatchlist checks if a ticker is in today's screener watchlist
func (ws *WatchlistSync) IsInWatchlist(ticker string) bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	_, exists := ws.watchlist[ticker]
	return exists
}

// GetAllWatchlistTickers returns all tickers in the current watchlist
func (ws *WatchlistSync) GetAllWatchlistTickers() []string {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	tickers := make([]string, 0, len(ws.watchlist))
	for ticker := range ws.watchlist {
		tickers = append(tickers, ticker)
	}
	return tickers
}

// GetWatchlistCount returns the number of stocks in the current watchlist
func (ws *WatchlistSync) GetWatchlistCount() int {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return len(ws.watchlist)
}

// GetConfidenceBoost returns a confidence multiplier for a signal based on watchlist
// - Signal for stock IN watchlist: boost confidence (1.1x - 1.3x)
// - Signal for stock NOT in watchlist: no change (1.0x)
// - IHSG not safe: heavy penalty (0.5x)
func (ws *WatchlistSync) GetConfidenceBoost(ticker string, signalPrice float64) float64 {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	// IHSG safety gate — strongest signal
	if !ws.ihsgSafe {
		return 0.5 // 50% penalty when market is unsafe
	}

	entry, exists := ws.watchlist[ticker]
	if !exists {
		return 1.0 // No boost, no penalty
	}

	// Stock is in watchlist — base boost
	boost := 1.1

	// Extra boost if signal price is near entry zone
	if entry.Entry1 > 0 && signalPrice > 0 {
		priceDiffPct := ((signalPrice - entry.Entry1) / entry.Entry1) * 100

		if priceDiffPct >= -1.0 && priceDiffPct <= 1.0 {
			// Price is within ±1% of entry zone — strong boost
			boost = 1.3
		} else if priceDiffPct >= -3.0 && priceDiffPct <= 3.0 {
			// Price is within ±3% of entry zone — moderate boost
			boost = 1.2
		} else if priceDiffPct > 5.0 {
			// Price already far above entry — reduce boost (chasing)
			boost = 0.9
		}
	}

	return boost
}
