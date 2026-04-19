package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────────────────────

// StockbitRedis is a minimal interface for what the collector needs from Redis
type StockbitRedis interface {
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error
	Get(ctx context.Context, key string, dest interface{}) error
}

// StockbitTokenProvider is satisfied by auth.AuthClient (via authManager.GetClient())
type StockbitTokenProvider interface {
	GetValidToken() (string, error)
}

// RunningTradeSummaryItem aggregates buy/sell per symbol
type RunningTradeSummaryItem struct {
	Symbol        string `json:"symbol"`
	BuyLot        int    `json:"buy_lot"`
	SellLot       int    `json:"sell_lot"`
	NetLot        int    `json:"net_lot"`
	ForeignNet    int    `json:"foreign_net"` // positive = net asing beli
	DominantBuyer string `json:"dominant_buyer"`
	TxCount       int    `json:"tx_count"`
}

// RunningTradeSummary is what gets stored in Redis
type RunningTradeSummary struct {
	Timestamp    string                     `json:"timestamp"`
	Date         string                     `json:"date"`
	TotalSymbols int                        `json:"total_symbols"`
	Summary      map[string]RunningTradeSummaryItem `json:"summary"`
}

// CompanyInfo is what gets stored in Redis per-symbol
type CompanyInfo struct {
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

// MarketMoverItem represents a single stock from market mover
type MarketMoverItem struct {
	Symbol       string  `json:"symbol"`
	Name         string  `json:"name"`
	Price        float64 `json:"price"`
	ChangePct    float64 `json:"change_pct"`
	ValueToday   float64 `json:"value_today"`
	FreqToday    int     `json:"frequency_today"`
	NetForeign   float64 `json:"net_foreign_today"`
}

// HistoricalBar represents one day of OHLCV + foreign flow
type HistoricalBar struct {
	Date        string  `json:"date"`
	Open        float64 `json:"open"`
	High        float64 `json:"high"`
	Low         float64 `json:"low"`
	Close       float64 `json:"close"`
	Volume      int64   `json:"volume"`
	Frequency   int     `json:"frequency"`
	Value       float64 `json:"value"`
	ForeignBuy  float64 `json:"foreign_buy"`
	ForeignSell float64 `json:"foreign_sell"`
	NetForeign  float64 `json:"net_foreign"`
	ChangePct   float64 `json:"change_pct"`
}

// HistoricalSummary is stored per ticker in Redis
type HistoricalSummary struct {
	Ticker    string         `json:"ticker"`
	Bars      []HistoricalBar `json:"bars"`
	UpdatedAt string         `json:"updated_at"`
}

// BrokerSignal represents bandar detector result
type BrokerSignal struct {
	Ticker    string  `json:"ticker"`
	Label     string  `json:"label"`
	Score     int     `json:"score"`
	UpdatedAt string  `json:"updated_at"`
}

// OrderbookLevel represents one price level in the orderbook
type OrderbookLevel struct {
	Price float64 `json:"price"`
	Lot   int64   `json:"lot"`
}

// Orderbook is stored per ticker in Redis
type Orderbook struct {
	Ticker    string           `json:"ticker"`
	Bids      []OrderbookLevel `json:"bids"`
	Asks      []OrderbookLevel `json:"asks"`
	UpdatedAt string           `json:"updated_at"`
}

// KeystatsData is stored per ticker in Redis (compatible with Python format)
type KeystatsData struct {
	Symbol           string   `json:"symbol"`
	PeTTM            *float64 `json:"pe_ttm"`
	EpsTTM           *float64 `json:"eps_ttm"`
	RoeTTM           *float64 `json:"roe_ttm"`
	RoaTTM           *float64 `json:"roa_ttm"`
	NetProfitMargin  *float64 `json:"net_profit_margin"`
	RevenueGrowthYoY *float64 `json:"revenue_growth_yoy"`
	NetIncomeGrowth  *float64 `json:"net_income_growth_yoy"`
	DividendYield    *float64 `json:"dividend_yield"`
	PiotroskiScore   *float64 `json:"piotroski_score"`
	High52W          *float64 `json:"high_52w"`
	Low52W           *float64 `json:"low_52w"`
	PriceReturnYTD   *float64 `json:"price_return_ytd"`
	DebtToEquity     *float64 `json:"debt_to_equity"`
	EvEbitda         *float64 `json:"ev_ebitda"`
	PBV              *float64 `json:"pbv"`
	UpdatedAt        string   `json:"updated_at"`
}

// ─────────────────────────────────────────────────────────────────────────────
// StockbitCollector — runs as background goroutine
// ─────────────────────────────────────────────────────────────────────────────

type StockbitCollector struct {
	auth      StockbitTokenProvider
	redis     StockbitRedis
	client    *http.Client
	baseURL   string

	// Symbols to collect company info for (set by watchlist sync)
	mu      sync.RWMutex
	symbols []string
}

// NewStockbitCollector creates the collector. baseURL defaults to Stockbit exodus.
func NewStockbitCollector(auth StockbitTokenProvider, redis StockbitRedis) *StockbitCollector {
	return &StockbitCollector{
		auth:    auth,
		redis:   redis,
		baseURL: "https://exodus.stockbit.com",
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// SetSymbols updates the list of symbols to fetch company info for.
// Thread-safe — can be called from watchlist sync goroutine.
func (c *StockbitCollector) SetSymbols(symbols []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.symbols = make([]string, len(symbols))
	copy(c.symbols, symbols)
}

// Start begins all collection loops. Call as goroutine.
func (c *StockbitCollector) Start(ctx context.Context) {
	log.Println("📡 StockbitCollector: started")

	// Running trade: fetch every 3 minutes
	go c.runLoop(ctx, "running_trade", 3*time.Minute, c.fetchAndStoreRunningTrade)

	// Company info: fetch every 10 minutes (only for watchlist symbols)
	go c.runLoop(ctx, "company_info", 10*time.Minute, c.fetchAndStoreCompanyInfoBatch)

	// Phase 1 — New fetchers
	go c.runLoop(ctx, "market_mover", 5*time.Minute, c.fetchAndStoreMarketMover)
	go c.runLoop(ctx, "hist_summary", 5*time.Minute, c.fetchAndStoreHistoricalBatch)
	go c.runLoop(ctx, "broker_signal", 5*time.Minute, c.fetchAndStoreBrokerSignalBatch)
	go c.runLoop(ctx, "orderbook", 3*time.Minute, c.fetchAndStoreOrderbookBatch)
	go c.runLoop(ctx, "keystats", 10*time.Minute, c.fetchAndStoreKeystatsBatch)
	go c.runLoop(ctx, "screener_tmpl", 10*time.Minute, c.fetchAndStoreScreenerTemplates)

	<-ctx.Done()
	log.Println("📡 StockbitCollector: stopped")
}

// runLoop executes fn on each tick, with immediate first run
func (c *StockbitCollector) runLoop(ctx context.Context, name string, interval time.Duration, fn func(ctx context.Context) error) {
	log.Printf("📡 [%s] collecting every %v", name, interval)

	// Immediate first run
	if err := fn(ctx); err != nil {
		log.Printf("⚠️  [%s] first run error: %v", name, err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := fn(ctx); err != nil {
				log.Printf("⚠️  [%s] collect error: %v", name, err)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Running Trade collector
// ─────────────────────────────────────────────────────────────────────────────

func (c *StockbitCollector) fetchAndStoreRunningTrade(ctx context.Context) error {
	token, err := c.auth.GetValidToken()
	if err != nil || token == "" {
		return fmt.Errorf("no token: %v", err)
	}

	// Fetch multiple pages to get broad coverage (limit=200 per page, 3 pages)
	allTrades := make([]map[string]interface{}, 0, 600)
	var lastTradeNumber string

	for page := 0; page < 3; page++ {
		url := fmt.Sprintf("%s/order-trade/running-trade?sort=DESC&limit=200&order_by=RUNNING_TRADE_ORDER_BY_TIME", c.baseURL)
		if lastTradeNumber != "" {
			url += "&trade_number=" + lastTradeNumber
		}

		body, err := c.sbGet(ctx, url, token)
		if err != nil {
			log.Printf("⚠️  running_trade page %d: %v", page, err)
			break
		}

		var resp struct {
			Data struct {
				RunningTrade []map[string]interface{} `json:"running_trade"`
				Date         string                   `json:"date"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			break
		}

		if len(resp.Data.RunningTrade) == 0 {
			break
		}
		allTrades = append(allTrades, resp.Data.RunningTrade...)

		// Get smallest trade_number for next page cursor
		if last := resp.Data.RunningTrade[len(resp.Data.RunningTrade)-1]; last != nil {
			if tn, ok := last["trade_number"].(string); ok {
				lastTradeNumber = tn
			}
		}
	}

	if len(allTrades) == 0 {
		return fmt.Errorf("running_trade: no trades returned")
	}

	// Aggregate per symbol
	type agg struct {
		buyLot     int
		sellLot    int
		foreignNet int
		buyers     map[string]int // broker → lot count
		txCount    int
	}
	aggMap := make(map[string]*agg)

	for _, t := range allTrades {
		sym, _ := t["code"].(string)
		if sym == "" {
			continue
		}
		sym = strings.ToUpper(sym)

		lotRaw, _ := t["lot"].(string)
		lot := parseLotStr(lotRaw)

		action, _ := t["action"].(string)
		buyerType, _ := t["buyer_type"].(string)
		sellerType, _ := t["seller_type"].(string)
		buyerCode, _ := t["buyer"].(string)

		if _, ok := aggMap[sym]; !ok {
			aggMap[sym] = &agg{buyers: make(map[string]int)}
		}
		a := aggMap[sym]
		a.txCount++

		if action == "buy" {
			a.buyLot += lot
			// foreign net: if buyer is foreign
			if strings.Contains(buyerType, "FOREIGN") {
				a.foreignNet += lot
			}
			if strings.Contains(sellerType, "FOREIGN") {
				a.foreignNet -= lot
			}
			// dominant buyer tracking
			if buyerCode != "" {
				a.buyers[buyerCode] += lot
			}
		} else {
			a.sellLot += lot
			// if seller is foreign, foreign is net selling
			if strings.Contains(sellerType, "FOREIGN") {
				a.foreignNet -= lot
			}
			if strings.Contains(buyerType, "FOREIGN") {
				a.foreignNet += lot
			}
		}
	}

	// Build summary, sort by abs(net_lot) descending
	summary := make(map[string]RunningTradeSummaryItem, len(aggMap))
	for sym, a := range aggMap {
		netLot := a.buyLot - a.sellLot
		dominantBuyer := topBroker(a.buyers)
		summary[sym] = RunningTradeSummaryItem{
			Symbol:        sym,
			BuyLot:        a.buyLot,
			SellLot:       a.sellLot,
			NetLot:        netLot,
			ForeignNet:    a.foreignNet,
			DominantBuyer: dominantBuyer,
			TxCount:       a.txCount,
		}
	}

	now := time.Now()
	result := RunningTradeSummary{
		Timestamp:    now.Format("15:04 WIB"),
		Date:         now.Format("2006-01-02"),
		TotalSymbols: len(summary),
		Summary:      summary,
	}

	if err := c.redis.Set(ctx, "stockbit:running_trade", result, 4*time.Hour); err != nil {
		return fmt.Errorf("redis set running_trade: %v", err)
	}
	log.Printf("✅ running_trade: %d symbols aggregated from %d trades", len(summary), len(allTrades))
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Company Info collector (per watchlist symbol)
// ─────────────────────────────────────────────────────────────────────────────

func (c *StockbitCollector) fetchAndStoreCompanyInfoBatch(ctx context.Context) error {
	c.mu.RLock()
	syms := make([]string, len(c.symbols))
	copy(syms, c.symbols)
	c.mu.RUnlock()

	if len(syms) == 0 {
		log.Println("📡 company_info: no symbols in watchlist yet, skipping")
		return nil
	}

	token, err := c.auth.GetValidToken()
	if err != nil || token == "" {
		return fmt.Errorf("no token: %v", err)
	}

	success := 0
	for _, sym := range syms {
		if err := c.fetchAndStoreCompanyInfo(ctx, sym, token); err != nil {
			log.Printf("⚠️  company_info %s: %v", sym, err)
		} else {
			success++
		}
		// Small delay to avoid rate limiting
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	log.Printf("✅ company_info: %d/%d symbols updated", success, len(syms))
	return nil
}

func (c *StockbitCollector) fetchAndStoreCompanyInfo(ctx context.Context, symbol, token string) error {
	url := fmt.Sprintf("%s/emitten/%s/info", c.baseURL, symbol)
	body, err := c.sbGet(ctx, url, token)
	if err != nil {
		return err
	}

	var resp struct {
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
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse: %v", err)
	}

	info := CompanyInfo{
		Symbol:    resp.Data.Symbol,
		Name:      resp.Data.Name,
		Sector:    resp.Data.Sector,
		SubSector: resp.Data.SubSector,
		Price:     resp.Data.Price,
		Change:    resp.Data.Change,
		Pct:       resp.Data.Pct,
		Volume:    resp.Data.Volume,
		Indexes:   resp.Data.Indexes,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}

	key := fmt.Sprintf("stockbit:company_info:%s", symbol)
	return c.redis.Set(ctx, key, info, 12*time.Hour)
}

// ─────────────────────────────────────────────────────────────────────────────
// Market Mover collector (universe harian)
// ─────────────────────────────────────────────────────────────────────────────

// fetchAndStoreMarketMover fetches 5 mover types and stores deduplicated ticker
// lists into Redis universe keys. Each key stores a JSON array of MarketMoverItem.
func (c *StockbitCollector) fetchAndStoreMarketMover(ctx context.Context) error {
	token, err := c.auth.GetValidToken()
	if err != nil || token == "" {
		return fmt.Errorf("no token: %v", err)
	}

	// Boards to include in every request (matches Python CONFIG["MM_BOARDS"])
	boards := []string{
		"FILTER_STOCKS_TYPE_MAIN_BOARD",
		"FILTER_STOCKS_TYPE_DEVELOPMENT_BOARD",
		"FILTER_STOCKS_TYPE_ACCELERATION_BOARD",
		"FILTER_STOCKS_TYPE_NEW_ECONOMY_BOARD",
	}

	// (mover_type, redis_key) pairs
	types := []struct {
		moverType string
		redisKey  string
	}{
		{"MOVER_TYPE_TOP_VOLUME", "stockbit:universe:mover"},
		{"MOVER_TYPE_TOP_VALUE", "stockbit:universe:top_value"},
		{"MOVER_TYPE_NET_FOREIGN_BUY", "stockbit:universe:foreign"},
		{"MOVER_TYPE_TOP_GAINER", "stockbit:universe:gainer"},
		{"MOVER_TYPE_TOP_LOSER", "stockbit:universe:loser"},
	}

	totalSuccess := 0
	for _, mt := range types {
		// Build URL with query params (Stockbit uses repeated filter_stocks keys)
		params := "mover_type=" + mt.moverType
		for _, b := range boards {
			params += "&filter_stocks=" + b
		}
		url := fmt.Sprintf("%s/order-trade/market-mover?%s", c.baseURL, params)

		body, err := c.sbGet(ctx, url, token)
		if err != nil {
			log.Printf("⚠️  market_mover %s: %v", mt.moverType, err)
			continue
		}

		var resp struct {
			Data struct {
				MoverList []struct {
					StockDetail struct {
						Code string `json:"code"`
						Name string `json:"name"`
					} `json:"stock_detail"`
					Price     float64 `json:"price"`
					Change    struct {
						Percentage float64 `json:"percentage"`
					} `json:"change"`
					Value struct {
						Raw float64 `json:"raw"`
					} `json:"value"`
					Frequency struct {
						Raw int `json:"raw"`
					} `json:"frequency"`
					NetForeignBuy struct {
						Raw float64 `json:"raw"`
					} `json:"net_foreign_buy"`
					NetForeignSell struct {
						Raw float64 `json:"raw"`
					} `json:"net_foreign_sell"`
				} `json:"mover_list"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			log.Printf("⚠️  market_mover parse %s: %v", mt.moverType, err)
			continue
		}

		// Build items list
		items := make([]MarketMoverItem, 0, len(resp.Data.MoverList))
		for _, m := range resp.Data.MoverList {
			if m.StockDetail.Code == "" {
				continue
			}
			items = append(items, MarketMoverItem{
				Symbol:     m.StockDetail.Code,
				Name:       m.StockDetail.Name,
				Price:      m.Price,
				ChangePct:  m.Change.Percentage,
				ValueToday: m.Value.Raw,
				FreqToday:  m.Frequency.Raw,
				NetForeign: m.NetForeignBuy.Raw - m.NetForeignSell.Raw,
			})
		}

		if err := c.redis.Set(ctx, mt.redisKey, items, 6*time.Hour); err != nil {
			log.Printf("⚠️  redis set %s: %v", mt.redisKey, err)
			continue
		}
		totalSuccess++
		log.Printf("  ✅ %s: %d stocks → %s", mt.moverType, len(items), mt.redisKey)

		// Small delay between requests
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	log.Printf("✅ market_mover: %d/%d types stored", totalSuccess, len(types))
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Historical Summary collector (per watchlist symbol)
// ─────────────────────────────────────────────────────────────────────────────

func (c *StockbitCollector) fetchAndStoreHistoricalBatch(ctx context.Context) error {
	c.mu.RLock()
	syms := make([]string, len(c.symbols))
	copy(syms, c.symbols)
	c.mu.RUnlock()

	if len(syms) == 0 {
		log.Println("📡 hist_summary: no symbols in watchlist yet, skipping")
		return nil
	}

	token, err := c.auth.GetValidToken()
	if err != nil || token == "" {
		return fmt.Errorf("no token: %v", err)
	}

	success := 0
	for _, sym := range syms {
		if err := c.fetchAndStoreHistorical(ctx, sym, token); err != nil {
			log.Printf("⚠️  hist %s: %v", sym, err)
		} else {
			success++
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	log.Printf("✅ hist_summary: %d/%d symbols updated", success, len(syms))
	return nil
}

func (c *StockbitCollector) fetchAndStoreHistorical(ctx context.Context, symbol, token string) error {
	url := fmt.Sprintf("%s/company-price-feed/historical/summary/%s?period=HS_PERIOD_DAILY&limit=20", c.baseURL, symbol)
	body, err := c.sbGet(ctx, url, token)
	if err != nil {
		return err
	}

	var resp struct {
		Data struct {
			Result []struct {
				Date            string  `json:"date"`
				Open            float64 `json:"open"`
				High            float64 `json:"high"`
				Low             float64 `json:"low"`
				Close           float64 `json:"close"`
				Volume          int64   `json:"volume"`
				Frequency       int     `json:"frequency"`
				Value           float64 `json:"value"`
				ForeignBuy      float64 `json:"foreign_buy"`
				ForeignSell     float64 `json:"foreign_sell"`
				NetForeign      float64 `json:"net_foreign"`
				ChangePct       float64 `json:"change_percentage"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse: %v", err)
	}

	if len(resp.Data.Result) == 0 {
		return fmt.Errorf("no historical data")
	}

	bars := make([]HistoricalBar, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		bars = append(bars, HistoricalBar{
			Date:        r.Date,
			Open:        r.Open,
			High:        r.High,
			Low:         r.Low,
			Close:       r.Close,
			Volume:      r.Volume,
			Frequency:   r.Frequency,
			Value:       r.Value,
			ForeignBuy:  r.ForeignBuy,
			ForeignSell: r.ForeignSell,
			NetForeign:  r.NetForeign,
			ChangePct:   r.ChangePct,
		})
	}

	summary := HistoricalSummary{
		Ticker:    symbol,
		Bars:      bars,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}

	key := fmt.Sprintf("stockbit:hist:%s", symbol)
	return c.redis.Set(ctx, key, summary, 6*time.Hour)
}

// ─────────────────────────────────────────────────────────────────────────────
// Broker Signal collector (bandar detector per symbol)
// ─────────────────────────────────────────────────────────────────────────────

func (c *StockbitCollector) fetchAndStoreBrokerSignalBatch(ctx context.Context) error {
	c.mu.RLock()
	syms := make([]string, len(c.symbols))
	copy(syms, c.symbols)
	c.mu.RUnlock()

	if len(syms) == 0 {
		log.Println("📡 broker_signal: no symbols in watchlist yet, skipping")
		return nil
	}

	token, err := c.auth.GetValidToken()
	if err != nil || token == "" {
		return fmt.Errorf("no token: %v", err)
	}

	success := 0
	for _, sym := range syms {
		if err := c.fetchAndStoreBrokerSignal(ctx, sym, token); err != nil {
			log.Printf("⚠️  broker %s: %v", sym, err)
		} else {
			success++
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	log.Printf("✅ broker_signal: %d/%d symbols updated", success, len(syms))
	return nil
}

func (c *StockbitCollector) fetchAndStoreBrokerSignal(ctx context.Context, symbol, token string) error {
	url := fmt.Sprintf("%s/marketdetectors/%s?transaction_type=TRANSACTION_TYPE_NET&market_board=MARKET_BOARD_REGULER&investor_type=INVESTOR_TYPE_ALL&limit=25",
		c.baseURL, symbol)
	body, err := c.sbGet(ctx, url, token)
	if err != nil {
		return err
	}

	// Try multiple response paths (Stockbit API is inconsistent)
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("parse: %v", err)
	}

	accdist := extractAccdist(raw)

	scoreMap := map[string]int{
		"Big Acc":  35,
		"Acc":      20,
		"Neutral":  5,
		"Dist":     -999,
		"Big Dist": -999,
	}
	score, ok := scoreMap[accdist]
	if !ok {
		score = 5
	}

	signal := BrokerSignal{
		Ticker:    symbol,
		Label:     accdist,
		Score:     score,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}

	key := fmt.Sprintf("stockbit:broker:%s", symbol)
	return c.redis.Set(ctx, key, signal, 6*time.Hour)
}

// extractAccdist navigates multiple possible paths in the Stockbit marketdetectors response
func extractAccdist(raw map[string]interface{}) string {
	// Path 1: data.bandar_detector.avg.accdist
	if data, ok := raw["data"].(map[string]interface{}); ok {
		if bd, ok := data["bandar_detector"].(map[string]interface{}); ok {
			if avg, ok := bd["avg"].(map[string]interface{}); ok {
				if v, ok := avg["accdist"].(string); ok && v != "" {
					return v
				}
			}
			if v, ok := bd["accdist"].(string); ok && v != "" {
				return v
			}
		}
		// Path 2: data.result.bandar_detector.avg.accdist
		if result, ok := data["result"].(map[string]interface{}); ok {
			if bd, ok := result["bandar_detector"].(map[string]interface{}); ok {
				if avg, ok := bd["avg"].(map[string]interface{}); ok {
					if v, ok := avg["accdist"].(string); ok && v != "" {
						return v
					}
				}
			}
		}
		// Path 3: data.accdist
		if v, ok := data["accdist"].(string); ok && v != "" {
			return v
		}
	}
	return "Neutral"
}

// ─────────────────────────────────────────────────────────────────────────────
// Orderbook collector (bid/ask walls per symbol)
// ─────────────────────────────────────────────────────────────────────────────

func (c *StockbitCollector) fetchAndStoreOrderbookBatch(ctx context.Context) error {
	c.mu.RLock()
	syms := make([]string, len(c.symbols))
	copy(syms, c.symbols)
	c.mu.RUnlock()

	if len(syms) == 0 {
		log.Println("📡 orderbook: no symbols in watchlist yet, skipping")
		return nil
	}

	token, err := c.auth.GetValidToken()
	if err != nil || token == "" {
		return fmt.Errorf("no token: %v", err)
	}

	success := 0
	for _, sym := range syms {
		if err := c.fetchAndStoreOrderbook(ctx, sym, token); err != nil {
			log.Printf("⚠️  orderbook %s: %v", sym, err)
		} else {
			success++
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	log.Printf("✅ orderbook: %d/%d symbols updated", success, len(syms))
	return nil
}

func (c *StockbitCollector) fetchAndStoreOrderbook(ctx context.Context, symbol, token string) error {
	url := fmt.Sprintf("%s/company-price-feed/v2/orderbook/companies/%s", c.baseURL, symbol)
	body, err := c.sbGet(ctx, url, token)
	if err != nil {
		return err
	}

	var resp struct {
		Data struct {
			Bids []struct {
				Price float64 `json:"price"`
				Lot   int64   `json:"lot"`
			} `json:"bid"`
			Asks []struct {
				Price float64 `json:"price"`
				Lot   int64   `json:"lot"`
			} `json:"ask"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse: %v", err)
	}

	bids := make([]OrderbookLevel, 0, len(resp.Data.Bids))
	for _, b := range resp.Data.Bids {
		bids = append(bids, OrderbookLevel{Price: b.Price, Lot: b.Lot})
	}
	asks := make([]OrderbookLevel, 0, len(resp.Data.Asks))
	for _, a := range resp.Data.Asks {
		asks = append(asks, OrderbookLevel{Price: a.Price, Lot: a.Lot})
	}

	ob := Orderbook{
		Ticker:    symbol,
		Bids:      bids,
		Asks:      asks,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}

	key := fmt.Sprintf("stockbit:orderbook:%s", symbol)
	return c.redis.Set(ctx, key, ob, 5*time.Minute)
}

// ─────────────────────────────────────────────────────────────────────────────
// Keystats collector (fundamental ratios per symbol)
// ─────────────────────────────────────────────────────────────────────────────

func (c *StockbitCollector) fetchAndStoreKeystatsBatch(ctx context.Context) error {
	c.mu.RLock()
	syms := make([]string, len(c.symbols))
	copy(syms, c.symbols)
	c.mu.RUnlock()

	if len(syms) == 0 {
		log.Println("📡 keystats: no symbols in watchlist yet, skipping")
		return nil
	}

	token, err := c.auth.GetValidToken()
	if err != nil || token == "" {
		return fmt.Errorf("no token: %v", err)
	}

	success := 0
	for _, sym := range syms {
		if err := c.fetchAndStoreKeystats(ctx, sym, token); err != nil {
			log.Printf("⚠️  keystats %s: %v", sym, err)
		} else {
			success++
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	log.Printf("✅ keystats: %d/%d symbols updated", success, len(syms))
	return nil
}

// keystatsNameMap maps Stockbit field names to our JSON keys (matches Python format)
var keystatsNameMap = map[string]string{
	"Current PE Ratio (TTM)":          "pe_ttm",
	"Current EPS (TTM)":               "eps_ttm",
	"Return on Equity (TTM)":          "roe_ttm",
	"Return on Assets (TTM)":          "roa_ttm",
	"Net Profit Margin (Quarter)":     "net_profit_margin",
	"Revenue (Quarter YoY Growth)":    "revenue_growth_yoy",
	"Net Income (Quarter YoY Growth)": "net_income_growth_yoy",
	"Dividend Yield":                  "dividend_yield",
	"Piotroski F-Score":               "piotroski_score",
	"52 Week High":                    "high_52w",
	"52 Week Low":                     "low_52w",
	"Year to Date Price Returns":      "price_return_ytd",
	"Debt to Equity Ratio (Quarter)":  "debt_to_equity",
	"EV to EBITDA (TTM)":              "ev_ebitda",
	"Current Price to Book Value":     "pbv",
}

func (c *StockbitCollector) fetchAndStoreKeystats(ctx context.Context, symbol, token string) error {
	url := fmt.Sprintf("%s/keystats/ratio/v1/%s?year_limit=1", c.baseURL, symbol)
	body, err := c.sbGet(ctx, url, token)
	if err != nil {
		return err
	}

	var resp struct {
		Data struct {
			Items []struct {
				FinNameResults []struct {
					Fitem struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"fitem"`
				} `json:"fin_name_results"`
			} `json:"closure_fin_items_results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse: %v", err)
	}

	// Parse all fields into a map
	parsed := make(map[string]*float64)
	for _, group := range resp.Data.Items {
		for _, fn := range group.FinNameResults {
			jsonKey, ok := keystatsNameMap[fn.Fitem.Name]
			if !ok {
				continue
			}
			parsed[jsonKey] = parseKeystatsValue(fn.Fitem.Value)
		}
	}

	ks := KeystatsData{
		Symbol:           symbol,
		PeTTM:            parsed["pe_ttm"],
		EpsTTM:           parsed["eps_ttm"],
		RoeTTM:           parsed["roe_ttm"],
		RoaTTM:           parsed["roa_ttm"],
		NetProfitMargin:  parsed["net_profit_margin"],
		RevenueGrowthYoY: parsed["revenue_growth_yoy"],
		NetIncomeGrowth:  parsed["net_income_growth_yoy"],
		DividendYield:    parsed["dividend_yield"],
		PiotroskiScore:   parsed["piotroski_score"],
		High52W:          parsed["high_52w"],
		Low52W:           parsed["low_52w"],
		PriceReturnYTD:   parsed["price_return_ytd"],
		DebtToEquity:     parsed["debt_to_equity"],
		EvEbitda:         parsed["ev_ebitda"],
		PBV:              parsed["pbv"],
		UpdatedAt:        time.Now().Format(time.RFC3339),
	}

	key := fmt.Sprintf("stockbit:keystats:%s", symbol)
	return c.redis.Set(ctx, key, ks, 12*time.Hour)
}

// parseKeystatsValue cleans Stockbit value strings: "12.5%" → 12.5, "(3.2)" → -3.2, "-" → nil
func parseKeystatsValue(raw string) *float64 {
	s := strings.TrimSpace(raw)
	s = strings.ReplaceAll(s, "%", "")
	s = strings.ReplaceAll(s, ",", "")
	if s == "" || s == "-" || s == "N/A" {
		return nil
	}
	// Handle negative in parentheses: (3.2) → -3.2
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		s = "-" + s[1:len(s)-1]
	}
	// Take first word only (some values have units)
	parts := strings.Fields(s)
	if len(parts) > 0 {
		s = parts[0]
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%f", &v); err == nil {
		return &v
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Screener Templates collector (guru screener + volume explosion)
// ─────────────────────────────────────────────────────────────────────────────

// guruTemplate defines a Stockbit guru screener template
type guruTemplate struct {
	ID      int
	Label   string
	MaxPage int
}

// guruTemplates matches Python CONFIG["SB_GURU_SCREENER_LIST"]
var guruTemplates = []guruTemplate{
	{92, "Big Accumulation", 20},
	{77, "Foreign Flow Uptrend", 20},
	{94, "Bandar Bullish Reversal", 20},
	{87, "Reversal on Bearish Trend", 20},
	{88, "Potential Reversal on Bearish Trend", 20},
	{63, "High Volume Breakout", 20},
	{97, "Frequency Spike", 10},
	{72, "IHSG Short-term Outperformers", 20},
	{78, "Daily Net Foreign Flow", 50},
	{79, "1 Week Net Foreign Flow", 50},
}

func (c *StockbitCollector) fetchAndStoreScreenerTemplates(ctx context.Context) error {
	token, err := c.auth.GetValidToken()
	if err != nil || token == "" {
		return fmt.Errorf("no token: %v", err)
	}

	// --- Guru Screener templates ---
	guruSuccess := 0
	for _, tmpl := range guruTemplates {
		tickers := c.fetchGuruScreenerTickers(ctx, tmpl, token)
		if len(tickers) == 0 {
			continue
		}

		key := fmt.Sprintf("stockbit:universe:guru:%d", tmpl.ID)
		if err := c.redis.Set(ctx, key, tickers, 12*time.Hour); err != nil {
			log.Printf("⚠️  redis set guru %d: %v", tmpl.ID, err)
			continue
		}
		guruSuccess++
		log.Printf("  ✅ guru %d (%s): %d tickers", tmpl.ID, tmpl.Label, len(tickers))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	log.Printf("✅ screener_tmpl: %d/%d guru templates stored", guruSuccess, len(guruTemplates))
	return nil
}

// fetchGuruScreenerTickers fetches all pages of a guru screener template and returns ticker list
func (c *StockbitCollector) fetchGuruScreenerTickers(ctx context.Context, tmpl guruTemplate, token string) []string {
	var tickers []string
	seen := make(map[string]bool)

	for page := 1; page <= tmpl.MaxPage; page++ {
		url := fmt.Sprintf("%s/screener/templates/%d?type=TEMPLATE_TYPE_GURU&page=%d",
			c.baseURL, tmpl.ID, page)

		body, err := c.sbGet(ctx, url, token)
		if err != nil {
			log.Printf("⚠️  guru %d p%d: %v", tmpl.ID, page, err)
			break
		}

		var resp struct {
			Data struct {
				Calcs []struct {
					Company struct {
						Symbol string `json:"symbol"`
					} `json:"company"`
				} `json:"calcs"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			break
		}
		if len(resp.Data.Calcs) == 0 {
			break // empty page → stop pagination
		}

		for _, calc := range resp.Data.Calcs {
			if calc.Company.Symbol != "" && !seen[calc.Company.Symbol] {
				seen[calc.Company.Symbol] = true
				tickers = append(tickers, calc.Company.Symbol)
			}
		}

		// Rate limiting
		select {
		case <-ctx.Done():
			return tickers
		case <-time.After(300 * time.Millisecond):
		}
	}

	return tickers
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// sbGet performs authenticated GET to Stockbit exodus
func (c *StockbitCollector) sbGet(ctx context.Context, url, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://stockbit.com")
	req.Header.Set("Referer", "https://stockbit.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("401 Unauthorized — token expired")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	return io.ReadAll(resp.Body)
}

// parseLotStr converts "1,000" or "50" string → int
func parseLotStr(s string) int {
	s = strings.ReplaceAll(s, ",", "")
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

// topBroker returns the broker code with highest lot volume
func topBroker(m map[string]int) string {
	type kv struct {
		k string
		v int
	}
	var pairs []kv
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	if len(pairs) == 0 {
		return "-"
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].v > pairs[j].v })
	return pairs[0].k
}
