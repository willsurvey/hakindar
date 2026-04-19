package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"stockbit-haka-haki/cache"
	"stockbit-haka-haki/database"
	models "stockbit-haka-haki/database/models_pkg"
)

// SmartBootstrap handles intelligent cold-start data seeding
// V2: Also seeds master data (CompanyProfile + FundamentalKeystat) for all 958 BEI symbols.
type SmartBootstrap struct {
	repo      *database.TradeRepository
	redis     *cache.RedisClient
	authToken func() (string, error) // Stockbit token provider (set via SetTokenProvider)

	// Progress tracking
	isComplete   atomic.Bool
	currentStep  atomic.Int32
	totalSteps   int32
	progressMsg  atomic.Value // stores string
	liquidStocks []string
}

// SetTokenProvider injects a function that returns a valid Stockbit JWT token.
// Call this from app.go after authentication is established.
func (sb *SmartBootstrap) SetTokenProvider(fn func() (string, error)) {
	sb.authToken = fn
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

// NeedsFullMarketBootstrap checks if company master data is missing.
// Returns true if company_profiles has fewer than 100 rows.
// This triggers the autonomous 958-symbol ingestion pipeline.
func (sb *SmartBootstrap) NeedsFullMarketBootstrap() bool {
	count, err := sb.repo.CountCompanyProfiles()
	if err != nil {
		log.Printf("⚠️ Bootstrap: Cannot check company profiles count: %v", err)
		return true
	}
	return count < 100
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

// ============================================================================
// Task 2.1 — LoadAllSymbolsFromFile
// Reads all BEI stock codes from a flat text file (one symbol per line).
// Falls back to the hardcoded blue-chip list if the file is not found.
// ============================================================================

// LoadAllSymbolsFromFile reads the 958-symbol universe from kode_saham_958.txt.
// Each line should contain one stock code (e.g. "BBCA", "GOTO").
// Blank lines and leading/trailing spaces are ignored automatically.
func LoadAllSymbolsFromFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open symbols file: %w", err)
	}
	defer f.Close()

	var symbols []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		sym := strings.TrimSpace(scanner.Text())
		if sym != "" && !strings.HasPrefix(sym, "#") {
			symbols = append(symbols, strings.ToUpper(sym))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan symbols file: %w", err)
	}
	log.Printf("📂 Loaded %d symbols from %s", len(symbols), path)
	return symbols, nil
}

// ============================================================================
// Task 2.2 — Trigger checked in NeedsFullMarketBootstrap() (already added above)
// ============================================================================

// ============================================================================
// Task 2.3 — RunFullMarketProfileBootstrap
// Background goroutine that ingests Company Profile + Keystats for all symbols.
// ============================================================================

// RunFullMarketProfileBootstrap fetches and permanently stores CompanyProfile
// and FundamentalKeystat for every symbol in the 958-symbol universe.
//
// Strategy:
//   - Read all symbols from screener/kode_saham_958.txt
//   - Build a skip-set of symbols already in company_profiles
//   - Loop remaining symbols with 600ms delay (safe from Stockbit rate-limit)
//   - Dual-write: PostgreSQL (permanent) + Redis (fast cache)
//   - Estimated completion: ~10 minutes for 958 symbols
//
// This runs as a non-blocking background goroutine. The main system
// continues processing WebSocket data while this fills the database.
func (sb *SmartBootstrap) RunFullMarketProfileBootstrap(ctx context.Context, symbolsFilePath string) {
	log.Println("🌐 Full-Market Bootstrap: Starting autonomous 958-symbol ingestion...")

	// 1. Load symbol universe from file
	symbols, err := LoadAllSymbolsFromFile(symbolsFilePath)
	if err != nil {
		log.Printf("⚠️ Full-Market Bootstrap: Cannot read symbols file, using blue-chip fallback: %v", err)
		symbols = fallbackBlueChips()
	}
	if len(symbols) == 0 {
		log.Println("⚠️ Full-Market Bootstrap: No symbols loaded, aborting")
		return
	}

	// 2. Build skip-set: symbols already in DB
	existing, err := sb.repo.GetAllProfiledSymbols()
	if err != nil {
		log.Printf("⚠️ Full-Market Bootstrap: Could not fetch existing profiles: %v", err)
	}
	skipSet := make(map[string]bool, len(existing))
	for _, s := range existing {
		skipSet[s] = true
	}
	log.Printf("🌐 Full-Market Bootstrap: %d symbols total, %d already profiled, %d to fetch",
		len(symbols), len(skipSet), len(symbols)-len(skipSet))

	// 3. Get Stockbit token
	if sb.authToken == nil {
		log.Println("⚠️ Full-Market Bootstrap: No token provider set, aborting profile fetch")
		return
	}
	token, err := sb.authToken()
	if err != nil || token == "" {
		log.Printf("⚠️ Full-Market Bootstrap: Cannot get Stockbit token: %v", err)
		return
	}

	client := &http.Client{Timeout: 15 * time.Second}
	success, skipped, failed := 0, 0, 0

	for i, sym := range symbols {
		// Check context cancellation (graceful shutdown)
		select {
		case <-ctx.Done():
			log.Printf("🛑 Full-Market Bootstrap: Stopped at %d/%d (shutdown signal)", i+1, len(symbols))
			return
		default:
		}

		// Skip already-profiled symbols
		if skipSet[sym] {
			skipped++
			continue
		}

		// Refresh token every 200 symbols (tokens expire in ~2 hours)
		if i > 0 && i%200 == 0 && sb.authToken != nil {
			if freshToken, err := sb.authToken(); err == nil && freshToken != "" {
				token = freshToken
				log.Printf("🔑 Full-Market Bootstrap: Token refreshed at symbol %d/%d", i+1, len(symbols))
			}
		}

		// Fetch + persist profile and keystats
		if err := sb.fetchAndStoreCompanyProfileToDB(ctx, client, sym, token); err != nil {
			log.Printf("⚠️ Full-Market Bootstrap: [%d/%d] %s failed: %v", i+1, len(symbols), sym, err)
			failed++
		} else {
			success++
		}

		// Progress log every 50 symbols
		if (i+1)%50 == 0 {
			log.Printf("🌐 Full-Market Bootstrap: %d/%d symbols processed (✅%d ⚠️%d ⏭️%d)",
				i+1, len(symbols), success, failed, skipped)
		}

		// Anti-rate-limit delay: 600ms between requests
		select {
		case <-ctx.Done():
			return
		case <-time.After(600 * time.Millisecond):
		}
	}

	log.Printf("🎉 Full-Market Bootstrap COMPLETE: ✅%d success | ⚠️%d failed | ⏭️%d skipped (total: %d)",
		success, failed, skipped, len(symbols))
}

// fetchAndStoreCompanyProfileToDB fetches Company Info and Keystats for a single
// symbol from the Stockbit API and persists them to PostgreSQL (permanent) and
// Redis (fast cache). This implements the dual-write strategy.
func (sb *SmartBootstrap) fetchAndStoreCompanyProfileToDB(ctx context.Context, client *http.Client, symbol, token string) error {
	baseURL := "https://exodus.stockbit.com"

	// ── Step A: Fetch Company Info ─────────────────────────────────────────────
	infoURL := fmt.Sprintf("%s/emitten/%s/info", baseURL, symbol)
	infoBody, err := sbHTTPGet(ctx, client, infoURL, token)
	if err != nil {
		return fmt.Errorf("company_info fetch: %w", err)
	}

	var infoResp struct {
		Data struct {
			Symbol    string   `json:"symbol"`
			Name      string   `json:"name"`
			Sector    string   `json:"sector"`
			SubSector string   `json:"sub_sector"`
			Price     string   `json:"price"`
			Change    string   `json:"change"`
			Pct       float64  `json:"percentage"`
			Volume    string   `json:"volume"`
			Indexes   []string `json:"indexes"`
			Updated   string   `json:"updated"`
		} `json:"data"`
	}
	if err := json.Unmarshal(infoBody, &infoResp); err != nil {
		return fmt.Errorf("company_info parse: %w", err)
	}

	// Build indexes JSON string
	idxBytes, _ := json.Marshal(infoResp.Data.Indexes)
	idxStr := string(idxBytes)
	if idxStr == "null" {
		idxStr = "[]"
	}

	// Persist profile to PostgreSQL (permanent)
	profile := &models.CompanyProfile{
		Symbol:    symbol,
		Name:      infoResp.Data.Name,
		Sector:    infoResp.Data.Sector,
		SubSector: infoResp.Data.SubSector,
		Indexes:   idxStr,
	}
	if err := sb.repo.UpsertCompanyProfile(profile); err != nil {
		return fmt.Errorf("upsert company_profile: %w", err)
	}

	// Also write to Redis as fast cache (TTL 25h)
	if sb.redis != nil {
		type redisCI struct {
			Symbol    string   `json:"symbol"`
			Name      string   `json:"name"`
			Sector    string   `json:"sector"`
			SubSector string   `json:"sub_sector"`
			Price     string   `json:"price"`
			Change    string   `json:"change"`
			Pct       float64  `json:"pct"`
			Volume    string   `json:"volume"`
			Indexes   []string `json:"indexes"`
			UpdatedAt string   `json:"updated_at"`
		}
		_ = sb.redis.Set(ctx, fmt.Sprintf("stockbit:company_info:%s", symbol), redisCI{
			Symbol:    symbol,
			Name:      infoResp.Data.Name,
			Sector:    infoResp.Data.Sector,
			SubSector: infoResp.Data.SubSector,
			Price:     infoResp.Data.Price,
			Change:    infoResp.Data.Change,
			Pct:       infoResp.Data.Pct,
			Volume:    infoResp.Data.Volume,
			Indexes:   infoResp.Data.Indexes,
			UpdatedAt: time.Now().Format(time.RFC3339),
		}, 25*time.Hour)
	}

	// ── Step B: Fetch Keystats (Fundamental) ──────────────────────────────────
	// Small pause between the two requests for the same symbol
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(150 * time.Millisecond):
	}

	ksURL := fmt.Sprintf("%s/company-balance-sheet/keystats/%s?currency=IDR", baseURL, symbol)
	ksBody, err := sbHTTPGet(ctx, client, ksURL, token)
	if err != nil {
		// Keystats fetch failure is non-fatal — profile already saved
		log.Printf("  ℹ️ keystats not available for %s: %v", symbol, err)
		return nil
	}

	var ksRaw map[string]interface{}
	if err := json.Unmarshal(ksBody, &ksRaw); err != nil {
		return nil // Non-fatal
	}

	ks := &models.FundamentalKeystat{
		Symbol:    symbol,
		FetchedAt: time.Now(),
	}

	// Helper to extract float pointer from nested response map
	getF := func(path ...string) *float64 {
		var node interface{} = ksRaw
		for _, key := range path {
			m, ok := node.(map[string]interface{})
			if !ok {
				return nil
			}
			node = m[key]
		}
		switch v := node.(type) {
		case float64:
			return &v
		case int:
			f := float64(v)
			return &f
		}
		return nil
	}

	ks.PeTTM = getF("data", "pe_ttm")
	ks.EpsTTM = getF("data", "eps_ttm")
	ks.RoeTTM = getF("data", "roe_ttm")
	ks.RoaTTM = getF("data", "roa_ttm")
	ks.NetProfitMargin = getF("data", "net_profit_margin")
	ks.RevenueGrowthYoY = getF("data", "revenue_growth_yoy")
	ks.NetIncomeGrowth = getF("data", "net_income_growth_yoy")
	ks.DividendYield = getF("data", "dividend_yield")
	ks.PiotroskiScore = getF("data", "piotroski_score")
	ks.High52W = getF("data", "high_52w")
	ks.Low52W = getF("data", "low_52w")
	ks.PriceReturnYTD = getF("data", "price_return_ytd")
	ks.DebtToEquity = getF("data", "debt_to_equity")
	ks.EvEbitda = getF("data", "ev_ebitda")
	ks.PBV = getF("data", "pbv")

	// Persist keystats to PostgreSQL
	if err := sb.repo.UpsertFundamentalKeystat(ks); err != nil {
		log.Printf("  ⚠️ upsert keystats %s: %v", symbol, err)
	}

	// Also write to Redis as fast cache (TTL 25h)
	if sb.redis != nil {
		_ = sb.redis.Set(ctx, fmt.Sprintf("stockbit:keystats:%s", symbol), ks, 25*time.Hour)
	}

	return nil
}

// sbHTTPGet is a shared HTTP GET helper for Stockbit API calls within bootstrap.
// It sets the Authorization header and reads the full response body.
func sbHTTPGet(ctx context.Context, client *http.Client, url, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return io.ReadAll(resp.Body)
}

// fallbackBlueChips returns hardcoded LQ45-like symbols used when the 958-symbol
// file cannot be found. Identical to the original getFallbackLiquidStocks list.
func fallbackBlueChips() []string {
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
// FASE 3: HISTORICAL TICK-DATA SCRAPER (REVOLUSIONER)
// Menarik histori running trade dari Stockbit API dengan full pagination.
// Endpoint: GET /order-trade/running-trade?symbols[]={SYMBOL}&date={DATE}&trade_number={CURSOR}
// ============================================================================

// RunHistoricalTickSweep fetches historical running trade data from Stockbit
// for each symbol over the past N trading days.
//
// Strategy:
//   - Loop dari hari ini mundur sebanyak daysBack hari kerja
//   - Skip Sabtu & Minggu (bursa tutup)
//   - Per-symbol: cek apakah data untuk tanggal tersebut sudah ada di DB
//   - Jika belum: fetch semua halaman (full pagination) lalu simpan ke running_trades
//   - Jika sudah: skip (idempotent — aman dijalankan berulang kali)
//
// Setelah sweep selesai, jalankan ulang RunWhaleRetrospective agar Whale Alert
// dibangun dari data riil Stockbit (bukan sintetis Yahoo Finance).
func (sb *SmartBootstrap) RunHistoricalTickSweep(ctx context.Context, symbols []string, daysBack int) {
	if daysBack <= 0 {
		daysBack = 30
	}
	if len(symbols) == 0 {
		log.Println("⚠️ HistoricalTickSweep: No symbols provided, aborting")
		return
	}

	log.Printf("📜 Historical Tick Sweep: Starting %d-day sweep for %d symbols...", daysBack, len(symbols))

	// Get Stockbit token
	if sb.authToken == nil {
		log.Println("⚠️ HistoricalTickSweep: No token provider set, aborting")
		return
	}
	token, err := sb.authToken()
	if err != nil || token == "" {
		log.Printf("⚠️ HistoricalTickSweep: Cannot get Stockbit token: %v", err)
		return
	}

	client := &http.Client{Timeout: 20 * time.Second}

	// Loop mundur dari kemarin (data hari ini sedang berjalan live)
	now := time.Now()
	totalFetched := 0
	totalSkipped := 0

	for dayOffset := 1; dayOffset <= daysBack; dayOffset++ {
		// Hitung tanggal target
		targetDate := now.AddDate(0, 0, -dayOffset)

		// Skip weekend: Sabtu (6) dan Minggu (0)
		weekday := targetDate.Weekday()
		if weekday == time.Saturday || weekday == time.Sunday {
			continue
		}

		dateStr := targetDate.Format("2006-01-02")

		// Refresh token setiap 100 hari (untuk sweep panjang)
		if dayOffset > 0 && dayOffset%100 == 0 && sb.authToken != nil {
			if freshToken, err := sb.authToken(); err == nil && freshToken != "" {
				token = freshToken
				log.Printf("🔑 HistoricalTickSweep: Token refreshed at day offset %d", dayOffset)
			}
		}

		// Loop semua simbol untuk tanggal ini
		for _, sym := range symbols {
			select {
			case <-ctx.Done():
				log.Printf("🛑 HistoricalTickSweep: Stopped (shutdown signal) at date=%s symbol=%s", dateStr, sym)
				return
			default:
			}

			// Cek apakah data sudah ada di database (idempotent check)
			if sb.hasTickDataForDate(sym, targetDate) {
				totalSkipped++
				continue
			}

			// Fetch semua halaman untuk (symbol, date) ini
			count, err := sb.fetchHistoricalRunningTradeForDate(ctx, client, sym, dateStr, token)
			if err != nil {
				log.Printf("  ⚠️ HistoricalTickSweep: %s@%s failed: %v", sym, dateStr, err)
			} else if count > 0 {
				totalFetched += count
			}

			// Jeda antar simbol: 500ms (aman dari rate-limit)
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}

		log.Printf("📜 HistoricalTickSweep: Date %s done — fetched=%d skipped=%d",
			dateStr, totalFetched, totalSkipped)

		// Jeda antar hari: 1 detik
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}

	log.Printf("🎉 HistoricalTickSweep COMPLETE: Total fetched=%d skipped=%d", totalFetched, totalSkipped)

	// Task 3.4: Rebuild Whale Alert dari data historis yang baru masuk
	// Data kali ini adalah transaksi riil Stockbit — akurasi jauh lebih tinggi
	log.Println("🐋 HistoricalTickSweep: Triggering Whale Retrospective on new tick data...")
	if err := sb.runWhaleRetrospective(ctx); err != nil {
		log.Printf("⚠️ HistoricalTickSweep: Whale retrospective failed: %v", err)
	}
}

// fetchHistoricalRunningTradeForDate fetches all pages of running trade history
// for a single symbol on a single date from the Stockbit API.
//
// Pagination Strategy:
//   - First call: tanpa trade_number parameter (halaman pertama)
//   - Selanjutnya: gunakan trade_number dari data terakhir sebagai cursor
//   - Stop: ketika response mengembalikan 0 data atau trade_number = 0
//
// Endpoint:
//
//	GET /order-trade/running-trade
//	  ?sort=DESC
//	  &limit=50
//	  &order_by=RUNNING_TRADE_ORDER_BY_TIME
//	  &symbols[]={symbol}
//	  &date={date}                    ← format: YYYY-MM-DD
//	  &trade_number={lastTradeNum}    ← cursor pagination (optional, omit for first page)
func (sb *SmartBootstrap) fetchHistoricalRunningTradeForDate(
	ctx context.Context,
	client *http.Client,
	symbol, dateStr, token string,
) (int, error) {
	baseURL := "https://exodus.stockbit.com/order-trade/running-trade"

	// Parse target date for timestamp construction
	targetDate, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return 0, fmt.Errorf("invalid date format: %w", err)
	}

	var (
		lastTradeNumber int64 = 0 // 0 = first page (no cursor)
		totalSaved      int   = 0
		page            int   = 0
		maxPages        int   = 200 // Safety cap: max 200 pages × 50 = 10.000 trades per symbol per day
	)

	for page < maxPages {
		page++

		// Build URL dengan query parameters
		url := fmt.Sprintf(
			"%s?sort=DESC&limit=50&order_by=RUNNING_TRADE_ORDER_BY_TIME&symbols%%5B%%5D=%s&date=%s",
			baseURL, symbol, dateStr,
		)
		if lastTradeNumber > 0 {
			url += fmt.Sprintf("&trade_number=%d", lastTradeNumber)
		}

		// HTTP GET ke Stockbit API
		body, err := sbHTTPGet(ctx, client, url, token)
		if err != nil {
			return totalSaved, fmt.Errorf("page %d fetch: %w", page, err)
		}

		// Parse response
		var resp struct {
			Data struct {
				List []struct {
					TradeNumber  int64   `json:"trade_number"`
					Symbol       string  `json:"symbol"`
					Price        float64 `json:"price"`
					Volume       int64   `json:"volume"`       // shares
					Value        float64 `json:"value"`        // total amount
					Action       string  `json:"action"`       // "buy" or "sell"
					MarketBoard  string  `json:"market_board"` // "RG", "TN", "NG"
					Change       float64 `json:"change"`
					TransactedAt string  `json:"transacted_at"` // ISO 8601
				} `json:"list"`
			} `json:"data"`
		}

		if err := json.Unmarshal(body, &resp); err != nil {
			return totalSaved, fmt.Errorf("page %d parse: %w", page, err)
		}

		trades := resp.Data.List

		// Stop condition: halaman kosong = tidak ada data lagi
		if len(trades) == 0 {
			break
		}

		// Convert ke slice *models.Trade untuk BatchSaveTrades
		var batch []*models.Trade
		for _, t := range trades {
			// Parse timestamp dari Stockbit
			ts, err := time.Parse(time.RFC3339, t.TransactedAt)
			if err != nil {
				// Fallback: gunakan tanggal trading (jam 09:00 WIB)
				ts = targetDate.Add(9 * time.Hour)
			}

			// Normalize action: "buy" → "BUY", "sell" → "SELL"
			action := strings.ToUpper(t.Action)

			// Hitung volume dalam lot (1 lot = 100 saham)
			volumeLot := float64(t.Volume) / 100.0

			// Hitung total amount jika tidak ada dari API
			totalAmount := t.Value
			if totalAmount == 0 && t.Price > 0 {
				totalAmount = t.Price * float64(t.Volume)
			}

			tradeNum := t.TradeNumber
			change := t.Change

			trade := &models.Trade{
				Timestamp:   ts,
				StockSymbol: symbol,
				Action:      action,
				Price:       t.Price,
				Volume:      float64(t.Volume),
				VolumeLot:   volumeLot,
				TotalAmount: totalAmount,
				MarketBoard: t.MarketBoard,
				Change:      &change,
				TradeNumber: &tradeNum,
			}
			batch = append(batch, trade)
		}

		// Simpan batch ke database (duplikat otomatis diabaikan oleh unique index)
		if len(batch) > 0 {
			if err := sb.repo.BatchSaveTrades(batch); err != nil {
				log.Printf("  ⚠️ BatchSave %s@%s page %d: %v", symbol, dateStr, page, err)
			} else {
				totalSaved += len(batch)
			}
		}

		// Update cursor: trade_number dari elemen TERAKHIR (sort=DESC, jadi ini yang paling lama)
		lastTradeNumber = trades[len(trades)-1].TradeNumber

		// Stop jika trade_number = 0 (data habis atau API tidak mendukung pagination lebih)
		if lastTradeNumber <= 0 {
			break
		}

		// Stop jika halaman tidak penuh (< 50 data = halaman terakhir)
		if len(trades) < 50 {
			break
		}

		// Jeda kecil antar halaman: 200ms (cegah burst request)
		select {
		case <-ctx.Done():
			return totalSaved, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	return totalSaved, nil
}

// hasTickDataForDate checks whether running_trades already has data
// for a given symbol on a given calendar date.
// Returns true if at least 10 trades exist (threshold to avoid re-fetching partial data).
func (sb *SmartBootstrap) hasTickDataForDate(symbol string, date time.Time) bool {
	startOfDay := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, date.Location())
	endOfDay := startOfDay.Add(24 * time.Hour)

	var count int64
	err := sb.repo.CountTradesForDateSymbol(symbol, startOfDay, endOfDay, &count)
	if err != nil {
		return false // Assume not fetched if check fails
	}
	return count >= 10
}

// runWhaleRetrospective is a convenience wrapper that re-triggers the whale
// alert generation logic using newly imported historical tick data.
// Called by RunHistoricalTickSweep after all data is ingested (Task 3.4).
// It fetches active symbols directly from the database to cover all newly imported data.
func (sb *SmartBootstrap) runWhaleRetrospective(ctx context.Context) error {
	// Ambil simbol aktif dari database (berisi data yang baru saja diimport)
	activeSymbols, err := sb.repo.GetActiveSymbols(time.Now().AddDate(0, 0, -30))
	if err != nil || len(activeSymbols) == 0 {
		log.Printf("⚠️ runWhaleRetrospective: Cannot get active symbols: %v", err)
		return err
	}
	log.Printf("🐋 runWhaleRetrospective: Processing %d active symbols...", len(activeSymbols))
	return sb.RunWhaleRetrospective(ctx, activeSymbols)
}
