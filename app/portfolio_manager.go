package app

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"stockbit-haka-haki/config"
)

// PortfolioManager tracks virtual trading balance and position sizing
// This is a SIMULATION portfolio — no real orders are placed.
// It helps evaluate signal quality by tracking P/L with realistic constraints.
type PortfolioManager struct {
	mu sync.RWMutex

	// Balance
	InitialBalance float64            `json:"initial_balance"`
	CurrentBalance float64            `json:"current_balance"`
	ReservedAmount float64            `json:"reserved_amount"` // Amount locked in open positions
	OpenPositions  map[string]*VirtualPosition `json:"open_positions"`

	// Cumulative stats
	TotalRealizedPL float64 `json:"total_realized_pl"`
	TotalTrades     int     `json:"total_trades"`
	WinCount        int     `json:"win_count"`
	LossCount       int     `json:"loss_count"`

	// Config
	MaxPositionPct     float64 // Max % of balance per position (default 10%)
	MaxTotalExposurePct float64 // Max % of balance exposed total (default 70%)

	cfg *config.Config
}

// VirtualPosition represents a simulated open position
type VirtualPosition struct {
	StockSymbol string    `json:"stock_symbol"`
	EntryPrice  float64   `json:"entry_price"`
	LotSize     int       `json:"lot_size"`      // Number of lots (1 lot = 100 shares)
	Shares      int       `json:"shares"`         // lot_size * 100
	TotalValue  float64   `json:"total_value"`    // entry_price * shares
	EntryTime   time.Time `json:"entry_time"`
	SignalID    int64     `json:"signal_id"`
}

// PortfolioSummary provides a snapshot of portfolio state for API
type PortfolioSummary struct {
	InitialBalance  float64 `json:"initial_balance"`
	CurrentBalance  float64 `json:"current_balance"`
	ReservedAmount  float64 `json:"reserved_amount"`
	AvailableBalance float64 `json:"available_balance"`
	TotalEquity     float64 `json:"total_equity"` // balance + unrealized P/L
	TotalRealizedPL float64 `json:"total_realized_pl"`
	OpenPositionCount int    `json:"open_position_count"`
	TotalTrades     int     `json:"total_trades"`
	WinRate         float64 `json:"win_rate_pct"`
	ExposurePct     float64 `json:"exposure_pct"`
}

// NewPortfolioManager creates a new virtual portfolio tracker
func NewPortfolioManager(cfg *config.Config) *PortfolioManager {
	balance := cfg.Trading.TradingBalance
	if balance <= 0 {
		balance = 200000 // Default Rp 200K
	}

	maxPosPct := cfg.Trading.MaxPositionPct
	if maxPosPct <= 0 {
		maxPosPct = 10.0 // Default 10% per position
	}

	maxExpPct := cfg.Trading.MaxTotalExposurePct
	if maxExpPct <= 0 {
		maxExpPct = 70.0 // Default 70% total exposure
	}

	pm := &PortfolioManager{
		InitialBalance:     balance,
		CurrentBalance:     balance,
		OpenPositions:      make(map[string]*VirtualPosition),
		MaxPositionPct:     maxPosPct,
		MaxTotalExposurePct: maxExpPct,
		cfg:                cfg,
	}

	log.Printf("💰 Virtual Portfolio initialized: Rp %.0f (max %.0f%% per position, %.0f%% total exposure)",
		balance, maxPosPct, maxExpPct)

	return pm
}

// CalculateLotSize determines how many lots to buy based on available balance and risk
// Returns 0 if insufficient balance or exposure limit reached
func (pm *PortfolioManager) CalculateLotSize(symbol string, price float64) int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	if price <= 0 {
		return 0
	}

	// Check if already have position in this symbol
	if _, exists := pm.OpenPositions[symbol]; exists {
		log.Printf("⚠️ Portfolio: Already have open position in %s", symbol)
		return 0
	}

	// Available balance = current - reserved
	available := pm.CurrentBalance - pm.ReservedAmount

	// Max value per position
	maxPositionValue := pm.CurrentBalance * (pm.MaxPositionPct / 100)

	// Check total exposure limit
	maxTotalExposure := pm.CurrentBalance * (pm.MaxTotalExposurePct / 100)
	remainingExposure := maxTotalExposure - pm.ReservedAmount
	if remainingExposure <= 0 {
		log.Printf("⚠️ Portfolio: Total exposure limit reached (%.0f%% of Rp %.0f)",
			pm.MaxTotalExposurePct, pm.CurrentBalance)
		return 0
	}

	// Use the minimum of: available balance, max position size, remaining exposure
	maxBuyValue := math.Min(available, math.Min(maxPositionValue, remainingExposure))

	if maxBuyValue < price*100 { // Can't even buy 1 lot
		log.Printf("⚠️ Portfolio: Insufficient balance for 1 lot of %s @ Rp %.0f (available: Rp %.0f)",
			symbol, price, maxBuyValue)
		return 0
	}

	// Calculate lots (1 lot = 100 shares)
	shares := int(maxBuyValue / price)
	lots := shares / 100

	// Ensure at least 1 lot
	if lots < 1 {
		lots = 1
	}

	return lots
}

// OpenPosition records a new virtual position
func (pm *PortfolioManager) OpenPosition(symbol string, price float64, lots int, signalID int64) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if lots <= 0 {
		return fmt.Errorf("invalid lot size: %d", lots)
	}

	shares := lots * 100
	totalValue := price * float64(shares)

	// Buy fee: 0.15%
	buyFee := totalValue * 0.0015
	totalCost := totalValue + buyFee

	available := pm.CurrentBalance - pm.ReservedAmount
	if totalCost > available {
		return fmt.Errorf("insufficient balance: need Rp %.0f, available Rp %.0f", totalCost, available)
	}

	pm.OpenPositions[symbol] = &VirtualPosition{
		StockSymbol: symbol,
		EntryPrice:  price,
		LotSize:     lots,
		Shares:      shares,
		TotalValue:  totalCost,
		EntryTime:   time.Now(),
		SignalID:    signalID,
	}
	pm.ReservedAmount += totalCost

	log.Printf("📈 Portfolio: Opened %s — %d lots @ Rp %.0f = Rp %.0f (fee: Rp %.0f)",
		symbol, lots, price, totalValue, buyFee)
	log.Printf("   💰 Balance: Rp %.0f | Reserved: Rp %.0f | Available: Rp %.0f",
		pm.CurrentBalance, pm.ReservedAmount, pm.CurrentBalance-pm.ReservedAmount)

	return nil
}

// ClosePosition closes a virtual position and updates balance
func (pm *PortfolioManager) ClosePosition(symbol string, exitPrice float64) (profitLoss float64, err error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pos, exists := pm.OpenPositions[symbol]
	if !exists {
		return 0, fmt.Errorf("no open position for %s", symbol)
	}

	exitValue := exitPrice * float64(pos.Shares)
	// Sell fee: 0.25% (includes 0.1% tax)
	sellFee := exitValue * 0.0025

	netExitValue := exitValue - sellFee
	profitLoss = netExitValue - pos.TotalValue

	// Update balance
	pm.CurrentBalance += profitLoss
	pm.ReservedAmount -= pos.TotalValue
	if pm.ReservedAmount < 0 {
		pm.ReservedAmount = 0
	}

	// Update stats
	pm.TotalRealizedPL += profitLoss
	pm.TotalTrades++
	if profitLoss > 0 {
		pm.WinCount++
	} else {
		pm.LossCount++
	}

	plPct := (profitLoss / pos.TotalValue) * 100

	log.Printf("📉 Portfolio: Closed %s — %d lots @ Rp %.0f → Rp %.0f | P/L: Rp %.0f (%.2f%%)",
		symbol, pos.LotSize, pos.EntryPrice, exitPrice, profitLoss, plPct)
	log.Printf("   💰 Balance: Rp %.0f | Realized P/L: Rp %.0f | Win Rate: %.1f%%",
		pm.CurrentBalance, pm.TotalRealizedPL, pm.GetWinRate())

	delete(pm.OpenPositions, symbol)
	return profitLoss, nil
}

// GetWinRate returns the win rate percentage
func (pm *PortfolioManager) GetWinRate() float64 {
	if pm.TotalTrades == 0 {
		return 0
	}
	return float64(pm.WinCount) / float64(pm.TotalTrades) * 100
}

// GetSummary returns a snapshot of portfolio state
func (pm *PortfolioManager) GetSummary() interface{} {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	available := pm.CurrentBalance - pm.ReservedAmount
	exposurePct := 0.0
	if pm.CurrentBalance > 0 {
		exposurePct = (pm.ReservedAmount / pm.CurrentBalance) * 100
	}

	return PortfolioSummary{
		InitialBalance:    pm.InitialBalance,
		CurrentBalance:    pm.CurrentBalance,
		ReservedAmount:    pm.ReservedAmount,
		AvailableBalance:  available,
		TotalEquity:       pm.CurrentBalance, // Without unrealized P/L for simplicity
		TotalRealizedPL:   pm.TotalRealizedPL,
		OpenPositionCount: len(pm.OpenPositions),
		TotalTrades:       pm.TotalTrades,
		WinRate:           pm.GetWinRate(),
		ExposurePct:       exposurePct,
	}
}

// HasOpenPosition checks if a symbol has an open position
func (pm *PortfolioManager) HasOpenPosition(symbol string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	_, exists := pm.OpenPositions[symbol]
	return exists
}
