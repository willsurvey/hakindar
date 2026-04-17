package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"stockbit-haka-haki/cache"
	"stockbit-haka-haki/database"
	models "stockbit-haka-haki/database/models_pkg"
)

// SmartBootstrap handles intelligent cold-start data seeding
// Strategy: Fetch today's running trades for ALL symbols via WebSocket (Layer 1),
// then fetch 30-day OHLCV history ONLY for today's liquid stocks (Layer 2).
type SmartBootstrap struct {
	repo  *database.TradeRepository
	redis *cache.RedisClient

	// Progress tracking
	isComplete   atomic.Bool
	currentStep  atomic.Int32
	totalSteps   int32
	progressMsg  atomic.Value // stores string
	liquidStocks []string
}

// BootstrapProgress represents current bootstrap status for API/UI
type BootstrapProgress struct {
	IsComplete  bool     `json:"is_complete"`
	CurrentStep int      `json:"current_step"`
	TotalSteps  int      `json:"total_steps"`
	Message     string   `json:"message"`
	Stocks      []string `json:"stocks,omitempty"`
}

// NewSmartBootstrap creates a new bootstrap manager
func NewSmartBootstrap(repo *database.TradeRepository, redis *cache.RedisClient) *SmartBootstrap {
	sb := &SmartBootstrap{
		repo:       repo,
		redis:      redis,
		totalSteps: 5, // Steps 1-5
	}
	sb.progressMsg.Store("Initializing...")
	return sb
}

// NeedsBootstrap checks if the database needs bootstrapping
// Returns true if running_trades has fewer than 1000 records
func (sb *SmartBootstrap) NeedsBootstrap() bool {
	count, err := sb.repo.GetTradeCount()
	if err != nil {
		log.Printf("⚠️ Bootstrap: Cannot check trade count: %v", err)
		return true // Assume bootstrap needed if we can't check
	}
	return count < 1000
}

// IsBootstrapComplete returns whether bootstrap has finished
func (sb *SmartBootstrap) IsBootstrapComplete() bool {
	return sb.isComplete.Load()
}

// GetProgress returns current bootstrap progress for API/UI
func (sb *SmartBootstrap) GetProgress() interface{} {
	msg, _ := sb.progressMsg.Load().(string)
	return BootstrapProgress{
		IsComplete:  sb.isComplete.Load(),
		CurrentStep: int(sb.currentStep.Load()),
		TotalSteps:  int(sb.totalSteps),
		Message:     msg,
		Stocks:      sb.liquidStocks,
	}
}

// Run executes the full bootstrap pipeline
func (sb *SmartBootstrap) Run(ctx context.Context) error {
	log.Println("🚀 Smart Bootstrap: Starting...")
	startTime := time.Now()

	// Publish bootstrap status to Redis
	sb.setProgress(0, "Memulai bootstrap...")

	// Step 1: Get liquid stocks from today's top movers
	sb.setProgress(1, "Mengambil daftar saham liquid hari ini...")
	liquidStocks, err := sb.GetLiquidStocksToday(ctx)
	if err != nil {
		log.Printf("⚠️ Bootstrap Step 1 failed: %v. Using fallback list.", err)
		liquidStocks = sb.getFallbackLiquidStocks()
	}
	sb.liquidStocks = liquidStocks
	log.Printf("✅ Bootstrap Step 1: Found %d liquid stocks", len(liquidStocks))

	// Step 2: Fetch LONG-TERM daily OHLCV (period=max, IPO to present)
	// This provides data for MA200, trend analysis, support/resistance
	sb.setProgress(2, fmt.Sprintf("Mengambil data historis harian (IPO→hari ini) untuk %d saham...", len(liquidStocks)))
	if err := sb.FetchLongTermOHLCV(ctx, liquidStocks); err != nil {
		log.Printf("⚠️ Bootstrap Step 2 partial failure: %v", err)
	}

	// Step 3: Fetch SHORT-TERM 5-min OHLCV (last 60 days)
	// Yahoo Finance 5-min data limited to 60 days — used for intraday whale detection
	sb.setProgress(3, fmt.Sprintf("Mengambil data intraday 60 hari untuk %d saham...", len(liquidStocks)))
	if err := sb.FetchHistoricalOHLCV(ctx, liquidStocks); err != nil {
		log.Printf("⚠️ Bootstrap Step 3 partial failure: %v", err)
	}

	// Step 4: Build statistical baselines from imported data
	sb.setProgress(4, "Menghitung statistical baselines...")
	if err := sb.BuildBaselines(ctx); err != nil {
		log.Printf("⚠️ Bootstrap Step 4 failed: %v", err)
	}

	// Step 5: Run whale retrospective on historical data
	sb.setProgress(5, "Menjalankan whale detection retrospective...")
	if err := sb.RunWhaleRetrospective(ctx, liquidStocks); err != nil {
		log.Printf("⚠️ Bootstrap Step 5 failed: %v", err)
	}

	// Done!
	elapsed := time.Since(startTime)
	sb.isComplete.Store(true)
	sb.setProgress(5, fmt.Sprintf("Bootstrap selesai dalam %v", elapsed.Round(time.Second)))

	log.Printf("🎉 Smart Bootstrap completed in %v", elapsed.Round(time.Second))
	log.Printf("   • Liquid stocks: %d", len(liquidStocks))

	return nil
}

// setProgress updates the current progress and publishes to Redis
func (sb *SmartBootstrap) setProgress(step int32, msg string) {
	sb.currentStep.Store(step)
	sb.progressMsg.Store(msg)
	log.Printf("🚀 Bootstrap [%d/%d]: %s", step, sb.totalSteps, msg)

	// Publish to Redis for UI consumption
	if sb.redis != nil {
		ctx := context.Background()
		progress := sb.GetProgress()
		sb.redis.Set(ctx, "bootstrap:progress", progress, 1*time.Hour)
	}
}

// ============================================================================
// Step 1: Get Liquid Stocks Today
// ============================================================================

// GetLiquidStocksToday returns a deduplicated list of today's most liquid stocks
// Combines top volume + top value from running trades already in DB,
// plus fallback hardcoded blue chips if DB is empty
func (sb *SmartBootstrap) GetLiquidStocksToday(ctx context.Context) ([]string, error) {
	// Try to get active symbols from today's trades (WebSocket may have started before bootstrap)
	activeSymbols, err := sb.repo.GetActiveSymbols(time.Now().Add(-6 * time.Hour))
	if err == nil && len(activeSymbols) >= 10 {
		// Take top 50 most active (they're already sorted by trade count from DB)
		if len(activeSymbols) > 50 {
			activeSymbols = activeSymbols[:50]
		}
		return activeSymbols, nil
	}

	// Fallback: use hardcoded list of LQ45-like blue chips
	return sb.getFallbackLiquidStocks(), nil
}

// getFallbackLiquidStocks returns a hardcoded list of Indonesia's most liquid stocks
func (sb *SmartBootstrap) getFallbackLiquidStocks() []string {
	return []string{
		"BBRI", "BBCA", "BMRI", "BBNI", "TLKM",
		"ASII", "UNVR", "HMSP", "GGRM", "ICBP",
		"INDF", "KLBF", "PGAS", "PTBA", "ADRO",
		"ANTM", "INCO", "MEDC", "SMGR", "CPIN",
		"EXCL", "MNCN", "TBIG", "TOWR", "ACES",
		"INKP", "TKIM", "BRPT", "MDKA", "EMTK",
		"BRIS", "BUKA", "GOTO", "ARTO", "BBYB",
		"AMMN", "UNTR", "ISAT", "MAPI", "ERAA",
	}
}

// ============================================================================
// Yahoo Finance API Response Types
// ============================================================================

// yahooChartResponse represents Yahoo Finance v8 chart API response
type yahooChartResponse struct {
	Chart struct {
		Result []struct {
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Open   []float64 `json:"open"`
					High   []float64 `json:"high"`
					Low    []float64 `json:"low"`
					Close  []float64 `json:"close"`
					Volume []float64 `json:"volume"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// ============================================================================
// Step 2: Fetch Long-Term Daily OHLCV (period=max)
// ============================================================================

// FetchLongTermOHLCV downloads daily OHLCV from IPO to present for each symbol
// Uses Yahoo Finance period=max with interval=1d
func (sb *SmartBootstrap) FetchLongTermOHLCV(ctx context.Context, symbols []string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	totalInserted := 0
	successCount := 0

	for i, symbol := range symbols {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		sb.setProgress(2, fmt.Sprintf("Mengambil data harian %s... (%d/%d)", symbol, i+1, len(symbols)))

		// Skip if we already have substantial daily data for this symbol
		existingCount, _ := sb.repo.GetDailyOHLCVCount(symbol)
		if existingCount > 200 {
			log.Printf("⏭️ Bootstrap: %s already has %d daily records, skipping", symbol, existingCount)
			successCount++
			continue
		}

		records, err := sb.fetchYahooDailyOHLCV(ctx, client, symbol)
		if err != nil {
			log.Printf("⚠️ Bootstrap: Failed to fetch daily %s: %v", symbol, err)
			continue
		}

		if len(records) == 0 {
			continue
		}

		if err := sb.repo.BatchSaveDailyOHLCV(records); err != nil {
			log.Printf("⚠️ Bootstrap: Failed to save daily %s: %v", symbol, err)
			continue
		}

		totalInserted += len(records)
		successCount++
		log.Printf("✅ Bootstrap: %s — %d daily candles imported (from IPO)", symbol, len(records))

		// Rate limit
		time.Sleep(500 * time.Millisecond)
	}

	log.Printf("✅ Bootstrap Step 2 complete: %d stocks, %d total daily records", successCount, totalInserted)
	return nil
}

// fetchYahooDailyOHLCV fetches max-period daily OHLCV from Yahoo Finance
func (sb *SmartBootstrap) fetchYahooDailyOHLCV(ctx context.Context, client *http.Client, symbol string) ([]models.DailyOHLCV, error) {
	yahooSymbol := symbol + ".JK"

	// period1=0 means "from the beginning" (IPO), period2=now
	now := time.Now()
	url := fmt.Sprintf(
		"https://query1.finance.yahoo.com/v8/finance/chart/%s?period1=0&period2=%d&interval=1d&includePrePost=false",
		yahooSymbol, now.Unix(),
	)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var chartResp yahooChartResponse
	if err := json.Unmarshal(body, &chartResp); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}

	if chartResp.Chart.Error != nil {
		return nil, fmt.Errorf("Yahoo API error: %s", chartResp.Chart.Error.Description)
	}

	if len(chartResp.Chart.Result) == 0 || len(chartResp.Chart.Result[0].Timestamp) == 0 {
		return nil, fmt.Errorf("no data returned for %s", symbol)
	}

	result := chartResp.Chart.Result[0]
	if len(result.Indicators.Quote) == 0 {
		return nil, fmt.Errorf("no quote data for %s", symbol)
	}

	quote := result.Indicators.Quote[0]
	var records []models.DailyOHLCV

	for i, ts := range result.Timestamp {
		if i >= len(quote.Close) || i >= len(quote.Volume) {
			break
		}

		closePrice := quote.Close[i]
		volume := quote.Volume[i]

		if closePrice == 0 || math.IsNaN(closePrice) {
			continue
		}

		openPrice := 0.0
		if i < len(quote.Open) {
			openPrice = quote.Open[i]
		}
		highPrice := 0.0
		if i < len(quote.High) {
			highPrice = quote.High[i]
		}
		lowPrice := 0.0
		if i < len(quote.Low) {
			lowPrice = quote.Low[i]
		}

		records = append(records, models.DailyOHLCV{
			Date:        time.Unix(ts, 0).Truncate(24 * time.Hour),
			StockSymbol: symbol,
			Open:        openPrice,
			High:        highPrice,
			Low:         lowPrice,
			Close:       closePrice,
			Volume:      volume,
			AdjClose:    closePrice, // Yahoo v8 already adjusts close
		})
	}

	return records, nil
}

// ============================================================================
// Step 3: Fetch Short-Term 5-min OHLCV (last 60 days)
// ============================================================================

// FetchHistoricalOHLCV downloads 60 days of 5-minute OHLCV data from Yahoo Finance
// for each symbol and inserts synthetic Trade records into running_trades
func (sb *SmartBootstrap) FetchHistoricalOHLCV(ctx context.Context, symbols []string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	totalInserted := 0
	successCount := 0

	for i, symbol := range symbols {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		sb.setProgress(3, fmt.Sprintf("Mengambil data intraday %s... (%d/%d)", symbol, i+1, len(symbols)))

		trades, err := sb.fetchYahooOHLCV(ctx, client, symbol)
		if err != nil {
			log.Printf("⚠️ Bootstrap: Failed to fetch %s: %v", symbol, err)
			continue
		}

		if len(trades) == 0 {
			continue
		}

		// Batch insert
		if err := sb.repo.BatchSaveTrades(trades); err != nil {
			log.Printf("⚠️ Bootstrap: Failed to save %s trades: %v", symbol, err)
			continue
		}

		totalInserted += len(trades)
		successCount++
		log.Printf("✅ Bootstrap: %s — %d intraday candles imported", symbol, len(trades))

		// Rate limit
		time.Sleep(500 * time.Millisecond)
	}

	log.Printf("✅ Bootstrap Step 3 complete: %d stocks, %d total trades imported", successCount, totalInserted)
	return nil
}

// fetchYahooOHLCV fetches OHLCV data for a single symbol from Yahoo Finance
func (sb *SmartBootstrap) fetchYahooOHLCV(ctx context.Context, client *http.Client, symbol string) ([]*database.Trade, error) {
	// Yahoo Finance uses .JK suffix for Jakarta Stock Exchange
	yahooSymbol := symbol + ".JK"

	// Fetch 60 days of 5-minute data (Yahoo Finance max for 5-min interval)
	// Yahoo Finance v8 API: period1/period2 are Unix timestamps
	now := time.Now()
	period2 := now.Unix()
	period1 := now.AddDate(0, 0, -60).Unix()

	url := fmt.Sprintf(
		"https://query1.finance.yahoo.com/v8/finance/chart/%s?period1=%d&period2=%d&interval=5m&includePrePost=false",
		yahooSymbol, period1, period2,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var chartResp yahooChartResponse
	if err := json.Unmarshal(body, &chartResp); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}

	if chartResp.Chart.Error != nil {
		return nil, fmt.Errorf("Yahoo API error: %s", chartResp.Chart.Error.Description)
	}

	if len(chartResp.Chart.Result) == 0 || len(chartResp.Chart.Result[0].Timestamp) == 0 {
		return nil, fmt.Errorf("no data returned for %s", symbol)
	}

	result := chartResp.Chart.Result[0]
	if len(result.Indicators.Quote) == 0 {
		return nil, fmt.Errorf("no quote data for %s", symbol)
	}

	quote := result.Indicators.Quote[0]
	var trades []*database.Trade

	for i, ts := range result.Timestamp {
		// Skip invalid data points
		if i >= len(quote.Close) || i >= len(quote.Volume) {
			break
		}

		close := quote.Close[i]
		volume := quote.Volume[i]

		// Skip zero/NaN values
		if close == 0 || volume == 0 || math.IsNaN(close) || math.IsNaN(volume) {
			continue
		}

		// Determine action based on open/close comparison
		action := "BUY"
		if i < len(quote.Open) && quote.Open[i] > close {
			action = "SELL"
		}

		volumeLot := volume / 100 // 1 lot = 100 shares
		totalAmount := close * volume

		timestamp := time.Unix(ts, 0)

		trades = append(trades, &database.Trade{
			Timestamp:   timestamp,
			StockSymbol: symbol,
			Action:      action,
			Price:       close,
			Volume:      volume,
			VolumeLot:   volumeLot,
			TotalAmount: totalAmount,
			MarketBoard: "RG", // Historical data is regular board
		})
	}

	return trades, nil
}

// ============================================================================
// Step 4: Build Statistical Baselines
// ============================================================================

// BuildBaselines triggers baseline calculation for all symbols with data
// Reuses the existing BaselineCalculator logic
func (sb *SmartBootstrap) BuildBaselines(ctx context.Context) error {
	log.Println("📊 Bootstrap: Building statistical baselines from imported data...")

	// Use the same logic as BaselineCalculator but in a blocking manner
	lookbackPeriods := []struct {
		minutes   int
		minTrades int
	}{
		{24 * 60 * 30, 2}, // 30 days of data
		{24 * 60, 2},      // 24 hours
		{2 * 60, 2},       // 2 hours
	}

	calculated := 0
	processedSymbols := make(map[string]bool)
	batchToSave := make([]models.StatisticalBaseline, 0, 100)

	for _, period := range lookbackPeriods {
		baselines, err := sb.repo.CalculateBaselinesDB(period.minutes, period.minTrades)
		if err != nil {
			log.Printf("⚠️ Bootstrap: Baseline calc failed for %d min lookback: %v", period.minutes, err)
			continue
		}

		for _, baseline := range baselines {
			if processedSymbols[baseline.StockSymbol] {
				continue
			}
			if baseline.MeanPrice <= 0 || baseline.SampleSize < period.minTrades {
				continue
			}
			batchToSave = append(batchToSave, baseline)
			calculated++
			processedSymbols[baseline.StockSymbol] = true
		}
	}

	if len(batchToSave) > 0 {
		if err := sb.repo.BatchSaveStatisticalBaselines(batchToSave); err != nil {
			return fmt.Errorf("batch save baselines: %w", err)
		}
	}

	log.Printf("✅ Bootstrap Step 4: %d baselines calculated for %d symbols", len(batchToSave), calculated)
	return nil
}

// ============================================================================
// Step 5: Whale Retrospective
// ============================================================================

// RunWhaleRetrospective replays whale detection logic on historical data
// This populates whale_alerts with 30 days of synthetic whale alerts
func (sb *SmartBootstrap) RunWhaleRetrospective(ctx context.Context, symbols []string) error {
	log.Println("🐋 Bootstrap: Running whale retrospective on historical data...")
	totalWhales := 0

	for i, symbol := range symbols {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if (i+1)%10 == 0 {
			sb.setProgress(4, fmt.Sprintf("Whale retrospective %s... (%d/%d)", symbol, i+1, len(symbols)))
		}

		// Get stats for this symbol (from baselines we just calculated)
		stats, err := sb.repo.GetStockStats(symbol, 24*60*30) // 30 days lookback
		if err != nil || stats == nil || stats.MeanVolumeLots <= 0 {
			continue
		}

		// Get historical trades for this symbol (last 30 days, sampled)
		endTime := time.Now()
		startTime := endTime.AddDate(0, 0, -30)
		trades, err := sb.repo.GetTradesByTimeRange(symbol, startTime, endTime)
		if err != nil || len(trades) == 0 {
			continue
		}

		// Replay whale detection on historical trades
		for _, trade := range trades {
			if trade.TotalAmount < 100_000_000 { // minSafeValue
				continue
			}

			var zScore float64
			if stats.StdDevVolume > 0 {
				zScore = (trade.VolumeLot - stats.MeanVolumeLots) / stats.StdDevVolume
			}

			volVsAvgPct := 0.0
			if stats.MeanVolumeLots > 0 {
				volVsAvgPct = (trade.VolumeLot / stats.MeanVolumeLots) * 100
			}

			isWhale := false
			detectionType := "RETROSPECTIVE"

			if zScore >= 3.0 {
				isWhale = true
				detectionType = "RETROSPECTIVE Z-SCORE"
			} else if trade.VolumeLot >= stats.MeanVolumeLots*5.0 {
				isWhale = true
				detectionType = "RETROSPECTIVE VOL SPIKE"
			} else if trade.TotalAmount >= 1_000_000_000 { // billionIDR
				isWhale = true
				detectionType = "RETROSPECTIVE VALUE"
			}

			if isWhale {
				confidence := 70.0 + (zScore-3.0)*15.0
				if confidence > 100.0 {
					confidence = 100.0
				}
				if confidence < 40.0 {
					confidence = 40.0
				}

				alert := &database.WhaleAlert{
					DetectedAt:        trade.Timestamp,
					StockSymbol:       trade.StockSymbol,
					AlertType:         "SINGLE_TRADE",
					Action:            trade.Action,
					TriggerPrice:      trade.Price,
					TriggerVolumeLots: trade.VolumeLot,
					TriggerValue:      trade.TotalAmount,
					ConfidenceScore:   confidence,
					MarketBoard:       trade.MarketBoard,
					ZScore:            &zScore,
					VolumeVsAvgPct:    &volVsAvgPct,
				}

				_ = strings.Contains(detectionType, "Z-SCORE") // suppress unused warning

				if err := sb.repo.SaveWhaleAlert(alert); err != nil {
					// Ignore duplicates — just continue
					continue
				}
				totalWhales++
			}
		}
	}

	log.Printf("✅ Bootstrap Step 5: %d whale alerts generated from historical data", totalWhales)
	return nil
}
