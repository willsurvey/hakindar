package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"stockbit-haka-haki/database"
	"stockbit-haka-haki/llm"
)

// safeFloat64 safely dereferences a float64 pointer, returning defaultValue if nil
func safeFloat64(ptr *float64, defaultValue float64) float64 {
	if ptr != nil {
		return *ptr
	}
	return defaultValue
}

// handleSymbolAnalysisStream streams symbol analysis via SSE
func (s *Server) handleSymbolAnalysisStream(w http.ResponseWriter, r *http.Request) {
	// Check if LLM is enabled
	if !s.llmEnabled || s.llmClient == nil {
		http.Error(w, "LLM is not enabled", http.StatusServiceUnavailable)
		return
	}

	// Get symbol from query param
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		http.Error(w, "symbol parameter is required", http.StatusBadRequest)
		return
	}

	// Get limit (default 20, max 50)
	maxLimit := 50
	limit := getIntParam(r, "limit", 20, nil, &maxLimit)

	// Get recent alerts for symbol
	alerts, err := s.repo.GetRecentAlertsBySymbol(symbol, limit)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error(), err)
		return
	}

	if len(alerts) == 0 {
		// Tidak ada whale alert — tetap lanjut ke LLM dengan info ini
		// LLM akan memberikan respons umum + info bahwa tidak ada aktivitas whale
		log.Printf("ℹ️  No whale alerts found for %s, proceeding with empty context for LLM", symbol)
	}

	// Fetch enriched metadata for context
	baseline, _ := s.repo.GetLatestBaseline(symbol)
	orderFlow, _ := s.repo.GetLatestOrderFlow(symbol)

	// OPTIMIZATION: Use batch query to avoid N+1 problem
	var alertIDs []int64
	for _, a := range alerts {
		alertIDs = append(alertIDs, a.ID)
	}

	followups, err := s.repo.GetWhaleFollowupsByAlertIDs(alertIDs)
	if err != nil {
		log.Printf("Warning: failed to batch fetch followups: %v", err)
		// Non-fatal error, continue without followups
		followups = []database.WhaleAlertFollowup{}
	}

	// ── Fase 1: Fetch enrichment — Redis first, PostgreSQL fallback ───────────
	redisCtx := &llm.SymbolRedisContext{}
	if s.redisCache != nil {
		// 1. Company Info — Redis first
		var ci struct {
			Name      string   `json:"name"`
			Sector    string   `json:"sector"`
			SubSector string   `json:"sub_sector"`
			Price     string   `json:"price"`
			Pct       float64  `json:"pct"`
			Volume    string   `json:"volume"`
			Indexes   []string `json:"indexes"`
		}
		if err := s.redisCache.Get(r.Context(), fmt.Sprintf("stockbit:company_info:%s", symbol), &ci); err == nil {
			redisCtx.Name = ci.Name
			redisCtx.Sector = ci.Sector
			redisCtx.SubSector = ci.SubSector
			redisCtx.Price = ci.Price
			redisCtx.ChangePct = ci.Pct
			redisCtx.Volume = ci.Volume
			redisCtx.Indexes = ci.Indexes
		} else {
			// Task 5.1 — Redis miss: fallback ke PostgreSQL (data permanen)
			log.Printf("ℹ️  Redis company_info miss for %s, querying PostgreSQL...", symbol)
			if s.repo != nil {
				if profile, dbErr := s.repo.GetCompanyProfile(symbol); dbErr == nil && profile != nil {
					redisCtx.Name = profile.Name
					redisCtx.Sector = profile.Sector
					redisCtx.SubSector = profile.SubSector
					// Parse JSON indexes string back to []string
					_ = json.Unmarshal([]byte(profile.Indexes), &redisCtx.Indexes)
					log.Printf("✅ PostgreSQL company_profile hit for %s: %s", symbol, profile.Name)
				} else {
					log.Printf("ℹ️  PostgreSQL also has no profile for %s", symbol)
				}
			}
		}

		// 2. Running Trade — extract symbol's entry
		var rt struct {
			Summary map[string]struct {
				BuyLot        int    `json:"buy_lot"`
				SellLot       int    `json:"sell_lot"`
				NetLot        int    `json:"net_lot"`
				ForeignNet    int    `json:"foreign_net"`
				DominantBuyer string `json:"dominant_buyer"`
			} `json:"summary"`
		}
		if err := s.redisCache.Get(r.Context(), "stockbit:running_trade", &rt); err == nil {
			if item, ok := rt.Summary[symbol]; ok {
				redisCtx.RTBuyLot = item.BuyLot
				redisCtx.RTSellLot = item.SellLot
				redisCtx.RTNetLot = item.NetLot
				redisCtx.RTForeignNet = item.ForeignNet
				redisCtx.RTDominantBuyer = item.DominantBuyer
				redisCtx.RTAvailable = true
			} else {
				log.Printf("ℹ️  Running trade: symbol %s not found in snapshot", symbol)
			}
		} else {
			log.Printf("ℹ️  Redis running_trade miss: %v", err)
		}

		// 3. Broker Signal (Bandar Detector)
		var bs struct {
			Label string `json:"label"`
			Score int    `json:"score"`
		}
		if err := s.redisCache.Get(r.Context(), fmt.Sprintf("stockbit:broker:%s", symbol), &bs); err == nil {
			redisCtx.BrokerLabel = bs.Label
			redisCtx.BrokerScore = bs.Score
			redisCtx.BrokerAvail = true
		} else {
			log.Printf("ℹ️  Redis broker_signal miss for %s: %v", symbol, err)
		}

		// ── Fase 2: Keystats & Historical ─────────────────────────────────────

		// Task 5.2 — Keystats: Redis first, lalu PostgreSQL fallback
		var ks struct {
			PeTTM            *float64 `json:"pe_ttm"`
			RoeTTM           *float64 `json:"roe_ttm"`
			PBV              *float64 `json:"pbv"`
			DividendYield    *float64 `json:"dividend_yield"`
			RevenueGrowthYoY *float64 `json:"revenue_growth_yoy"`
			PiotroskiScore   *float64 `json:"piotroski_score"`
			High52W          *float64 `json:"high_52w"`
			Low52W           *float64 `json:"low_52w"`
			DebtToEquity     *float64 `json:"debt_to_equity"`
		}
		if err := s.redisCache.Get(r.Context(), fmt.Sprintf("stockbit:keystats:%s", symbol), &ks); err == nil {
			redisCtx.PeTTM = ks.PeTTM
			redisCtx.RoeTTM = ks.RoeTTM
			redisCtx.PBV = ks.PBV
			redisCtx.DividendYield = ks.DividendYield
			redisCtx.RevenueGrowthYoY = ks.RevenueGrowthYoY
			redisCtx.PiotroskiScore = ks.PiotroskiScore
			redisCtx.High52W = ks.High52W
			redisCtx.Low52W = ks.Low52W
			redisCtx.DebtToEquity = ks.DebtToEquity
			redisCtx.KeystatsAvail = true
		} else {
			// Redis miss: fallback ke PostgreSQL (data permanen, tidak hilang saat Redis restart)
			log.Printf("ℹ️  Redis keystats miss for %s, querying PostgreSQL...", symbol)
			if s.repo != nil {
				if dbKs, dbErr := s.repo.GetFundamentalKeystat(symbol); dbErr == nil && dbKs != nil {
					redisCtx.PeTTM = dbKs.PeTTM
					redisCtx.RoeTTM = dbKs.RoeTTM
					redisCtx.PBV = dbKs.PBV
					redisCtx.DividendYield = dbKs.DividendYield
					redisCtx.RevenueGrowthYoY = dbKs.RevenueGrowthYoY
					redisCtx.PiotroskiScore = dbKs.PiotroskiScore
					redisCtx.High52W = dbKs.High52W
					redisCtx.Low52W = dbKs.Low52W
					redisCtx.DebtToEquity = dbKs.DebtToEquity
					redisCtx.KeystatsAvail = true
					log.Printf("✅ PostgreSQL keystats hit for %s", symbol)
				} else {
					log.Printf("ℹ️  PostgreSQL also has no keystats for %s", symbol)
				}
			}
		}

		// 5. Historical Daily Bars (last 5 days)
		var hist struct {
			Bars []struct {
				Date        string  `json:"date"`
				Close       float64 `json:"close"`
				Volume      int64   `json:"volume"`
				NetForeign  float64 `json:"net_foreign"`
				ChangePct   float64 `json:"change_pct"`
			} `json:"bars"`
		}
		if err := s.redisCache.Get(r.Context(), fmt.Sprintf("stockbit:hist:%s", symbol), &hist); err == nil {
			limit := 5
			if len(hist.Bars) < limit {
				limit = len(hist.Bars)
			}
			for i := 0; i < limit; i++ {
				b := hist.Bars[i]
				redisCtx.HistBars = append(redisCtx.HistBars, llm.HistBar{
					Date:       b.Date,
					Close:      b.Close,
					ChangePct:  b.ChangePct,
					Volume:     b.Volume,
					NetForeign: b.NetForeign,
				})
			}
		} else {
			log.Printf("ℹ️  Redis hist miss for %s: %v", symbol, err)
		}

		// ── Fase 3: Orderbook & Market Mover ──────────────────────────────────

		// 6. Orderbook (top 3 bid & ask)
		var ob struct {
			Bids []struct {
				Price float64 `json:"price"`
				Lot   int64   `json:"lot"`
			} `json:"bids"`
			Asks []struct {
				Price float64 `json:"price"`
				Lot   int64   `json:"lot"`
			} `json:"asks"`
		}
		if err := s.redisCache.Get(r.Context(), fmt.Sprintf("stockbit:orderbook:%s", symbol), &ob); err == nil {
			limit := 3
			for i := 0; i < limit && i < len(ob.Bids); i++ {
				redisCtx.TopBids = append(redisCtx.TopBids, llm.OBLevel{
					Price: ob.Bids[i].Price,
					Lot:   ob.Bids[i].Lot,
				})
			}
			for i := 0; i < limit && i < len(ob.Asks); i++ {
				redisCtx.TopAsks = append(redisCtx.TopAsks, llm.OBLevel{
					Price: ob.Asks[i].Price,
					Lot:   ob.Asks[i].Lot,
				})
			}
			redisCtx.OrderbookAvail = true
		} else {
			log.Printf("ℹ️  Redis orderbook miss for %s: %v", symbol, err)
		}

		// 7. Market Mover Positioning — check which universe lists contain this symbol
		moverChecks := []struct {
			key   string
			setFn func()
		}{
			{"stockbit:universe:mover", func() { redisCtx.InTopVolume = true }},
			{"stockbit:universe:top_value", func() { redisCtx.InTopValue = true }},
			{"stockbit:universe:foreign", func() { redisCtx.InForeignBuy = true }},
			{"stockbit:universe:gainer", func() { redisCtx.InTopGainer = true }},
			{"stockbit:universe:loser", func() { redisCtx.InTopLoser = true }},
		}
		for _, mc := range moverChecks {
			var items []struct {
				Symbol string `json:"symbol"`
			}
			if err := s.redisCache.Get(r.Context(), mc.key, &items); err == nil {
				for _, item := range items {
					if item.Symbol == symbol {
						mc.setFn()
						break
					}
				}
			}
		}
	}
	// ─────────────────────────────────────────────────────────────────────────

	// Set SSE headers
	flusher, ok := setupSSE(w)
	if !ok {
		respondWithError(w, http.StatusInternalServerError, "Streaming not supported", nil)
		return
	}

	// Task 5.3 & 5.4 — AI Sterilization: Strict Context Grounding
	// Tentukan apakah data cukup untuk analisis atau harus return DATA_UNAVAILABLE
	hasProfileData := redisCtx.Name != "" // Profil emiten harus ada
	hasWhaleData := len(alerts) > 0       // Minimal ada aktivitas whale

	var prompt string
	switch {
	case !hasProfileData && !hasWhaleData:
		// Task 5.4: Data kosong — kirim event data_unavailable via SSE, JANGAN biarkan LLM menebak
		fmt.Fprintf(w,
			"event: data_unavailable\ndata: {\"symbol\":\"%s\",\"reason\":\"no_profile_no_whale\",\"message\":\"Data profil dan aktivitas whale untuk %s belum tersedia di database. Sistem sedang mengisi data secara otomatis (Full-Market Bootstrap berjalan di background). Silakan coba lagi dalam beberapa menit.\"}\n\n",
			symbol, symbol)
		fmt.Fprintf(w, "event: done\ndata: DATA_UNAVAILABLE\n\n")
		flusher.Flush()
		return

	case !hasProfileData && hasWhaleData:
		// Ada whale data tapi tidak ada profil — analisis terbatas tanpa identitas perusahaan
		prompt = llm.FormatSymbolAnalysisPromptNoProfile(symbol, alerts, baseline, orderFlow, followups, redisCtx)

	case !hasWhaleData:
		// Ada profil tapi tidak ada whale alert — data real-time belum masuk
		fmt.Fprintf(w,
			"event: data_unavailable\ndata: {\"symbol\":\"%s\",\"reason\":\"no_whale_activity\",\"message\":\"Tidak ada aktivitas Whale Alert terdeteksi untuk %s dalam periode yang diminta. Data real-time sedang dikumpulkan via WebSocket. Coba lagi saat market buka atau setelah beberapa menit.\"}\n\n",
			symbol, symbol)
		fmt.Fprintf(w, "event: done\ndata: DATA_UNAVAILABLE\n\n")
		flusher.Flush()
		return

	default:
		// Data lengkap — jalankan analisis penuh, semua klaim harus dari data DB
		prompt = llm.FormatSymbolAnalysisPrompt(symbol, alerts, baseline, orderFlow, followups, redisCtx)
	}

	// Stream LLM response
	err = s.llmClient.AnalyzeStream(r.Context(), prompt, func(chunk string) error {
		// Properly format multi-line chunks for SSE
		lines := strings.Split(chunk, "\n")
		for i, line := range lines {
			if i < len(lines)-1 {
				fmt.Fprintf(w, "data: %s\n", line)
			} else {
				fmt.Fprintf(w, "data: %s\n\n", line)
			}
		}
		flusher.Flush()
		return nil
	})

	if err != nil {
		log.Printf("LLM streaming failed: %v", err)
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return
	}

	// Send completion event
	fmt.Fprintf(w, "event: done\ndata: Stream completed\n\n")
	flusher.Flush()
}

// handleCustomPromptStream streams AI analysis based on custom user prompt with database context
func (s *Server) handleCustomPromptStream(w http.ResponseWriter, r *http.Request) {
	// Check if LLM is enabled
	if !s.llmEnabled || s.llmClient == nil {
		http.Error(w, "LLM is not enabled", http.StatusServiceUnavailable)
		return
	}

	// Parse JSON request body
	var reqBody struct {
		Prompt      string   `json:"prompt"`
		Symbols     []string `json:"symbols"`      // optional: specific symbols to analyze
		HoursBack   int      `json:"hours_back"`   // hours of data to include
		IncludeData string   `json:"include_data"` // comma-separated: alerts,regimes,patterns,signals
	}

	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if reqBody.Prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}

	// Default values
	if reqBody.HoursBack <= 0 {
		reqBody.HoursBack = 24
	}
	if reqBody.IncludeData == "" {
		reqBody.IncludeData = "alerts,regimes"
	}

	// Set SSE headers
	flusher, ok := setupSSE(w)
	if !ok {
		respondWithError(w, http.StatusInternalServerError, "Streaming not supported", nil)
		return
	}

	// Build context data based on user selection
	var contextBuilder strings.Builder
	contextBuilder.WriteString("KONTEKS DATA DARI DATABASE:\n\n")

	includeTypes := strings.Split(reqBody.IncludeData, ",")

	for _, dataType := range includeTypes {
		dataType = strings.TrimSpace(dataType)

		switch dataType {
		case "alerts":
			// Get whale alerts
			var alerts []database.WhaleAlert

			if len(reqBody.Symbols) > 0 {
				// Get alerts for specific symbols
				for _, symbol := range reqBody.Symbols {
					symbolAlerts, e := s.repo.GetRecentAlertsBySymbol(symbol, 50)
					if e == nil {
						alerts = append(alerts, symbolAlerts...)
					}
				}
			} else {
				// Get recent alerts from accumulation patterns (top active stocks)
				patterns, e := s.repo.GetAccumulationPattern(reqBody.HoursBack, 2)
				if e == nil && len(patterns) > 0 {
					// Get alerts for top 10 most active symbols
					limit := 10
					if len(patterns) < limit {
						limit = len(patterns)
					}
					for i := 0; i < limit; i++ {
						symbolAlerts, ae := s.repo.GetRecentAlertsBySymbol(patterns[i].StockSymbol, 10)
						if ae == nil {
							alerts = append(alerts, symbolAlerts...)
						}
					}
				}
			}

			if len(alerts) > 0 {
				contextBuilder.WriteString("=== WHALE ALERTS (Transaksi Besar) ===\n")
				for i, a := range alerts {
					if i >= 20 { // Limit to 20 alerts
						break
					}
					zScore := safeFloat64(a.ZScore, 0.0)
					timeSince := time.Since(a.DetectedAt).Minutes()
					contextBuilder.WriteString(fmt.Sprintf(
						"- %s (%s): Rp %.1fM, Z-Score: %.2f, %.0f menit lalu\n",
						a.StockSymbol, a.Action, a.TriggerValue/1000000.0, zScore, timeSince,
					))
				}
				contextBuilder.WriteString("\n")
			}

		case "patterns":
			// Get accumulation patterns
			patterns, err := s.repo.GetAccumulationPattern(reqBody.HoursBack, 3)
			if err == nil && len(patterns) > 0 {
				contextBuilder.WriteString("=== POLA AKUMULASI/DISTRIBUSI ===\n")
				for i, p := range patterns {
					if i >= 10 {
						break
					}
					avgPrice := 0.0
					if p.TotalVolumeLots > 0 {
						avgPrice = p.TotalValue / (p.TotalVolumeLots * 100)
					}
					contextBuilder.WriteString(fmt.Sprintf(
						"- %s (%s): %d alerts, Total: Rp %.2fM, Avg Price: %.0f, Z-Score: %.2f\n",
						p.StockSymbol, p.Action, p.AlertCount,
						p.TotalValue/1000000.0, avgPrice, p.AvgZScore,
					))
				}
				contextBuilder.WriteString("\n")
			}

		case "signals":
			// Get recent signals (lookback 24 hours * 60 minutes)
			signals, err := s.repo.GetRecentSignalsWithOutcomes(reqBody.HoursBack*60, 0.0, "")
			if err == nil && len(signals) > 0 {
				contextBuilder.WriteString("=== TRADING SIGNALS (AI) ===\n")
				for i, sig := range signals {
					if i >= 15 {
						break
					}
					result := "OPEN"
					if sig.Outcome != "" {
						result = sig.Outcome
					}
					contextBuilder.WriteString(fmt.Sprintf(
						"- %s (%s): %s, Price: %.0f, Confidence: %.0f%%, Result: %s\n",
						sig.StockSymbol, sig.Strategy, sig.Decision,
						sig.Price, sig.Confidence*100, result,
					))
				}
				contextBuilder.WriteString("\n")
			}

		case "running_trade":
			// Get running trade summary from Redis (published by screener)
			if s.redisCache == nil {
				log.Printf("running_trade: redisCache not configured")
				break
			}
			var rt struct {
				Timestamp    string `json:"timestamp"`
				Date         string `json:"date"`
				TotalSymbols int    `json:"total_symbols"`
				Summary      map[string]struct {
					BuyLot        int    `json:"buy_lot"`
					SellLot       int    `json:"sell_lot"`
					NetLot        int    `json:"net_lot"`
					ForeignNet    int    `json:"foreign_net"`
					DominantBuyer string `json:"dominant_buyer"`
				} `json:"summary"`
			}
			if err := s.redisCache.Get(r.Context(), "stockbit:running_trade", &rt); err == nil {
				contextBuilder.WriteString(fmt.Sprintf("=== RUNNING TRADE (%s %s) — TOP 15 AKTIF ===\n", rt.Date, rt.Timestamp))
				contextBuilder.WriteString("Format: SIMBOL | Net Lot | Buy | Sell | Foreign Net | Broker Dominan\n")
				count := 0
				for sym, sv := range rt.Summary {
					if count >= 15 {
						break
					}
					foreignTag := ""
					if sv.ForeignNet > 500 {
						foreignTag = " 🟢ASING BELI"
					} else if sv.ForeignNet < -500 {
						foreignTag = " 🔴ASING JUAL"
					}
					contextBuilder.WriteString(fmt.Sprintf(
						"- %-6s | net=%+d | buy=%d | sell=%d | foreign=%+d%s | broker=%s\n",
						sym, sv.NetLot, sv.BuyLot, sv.SellLot, sv.ForeignNet, foreignTag, sv.DominantBuyer,
					))
					count++
				}
				contextBuilder.WriteString("\n")
			} else {
				log.Printf("running_trade: Redis get failed: %v", err)
			}

		case "keystats":
			// Get keystats fundamental per symbol from Redis
			if s.redisCache == nil {
				log.Printf("keystats: redisCache not configured")
				break
			}
			if len(reqBody.Symbols) > 0 {
				contextBuilder.WriteString("=== FUNDAMENTAL KEYSTATS ===\n")
				contextBuilder.WriteString("Format: SIMBOL | PE | ROE% | DivYield% | PBV | RevGrowth% | F-Score\n")
				for _, sym := range reqBody.Symbols {
					var ks map[string]interface{}
					ksErr := s.redisCache.Get(r.Context(), fmt.Sprintf("stockbit:keystats:%s", sym), &ks)
					if ksErr != nil {
						contextBuilder.WriteString(fmt.Sprintf("- %s: data fundamental belum tersedia\n", sym))
						continue
					}
					getF := func(key string) string {
						if v, ok := ks[key]; ok && v != nil {
							return fmt.Sprintf("%.2f", v)
						}
						return "-"
					}
					contextBuilder.WriteString(fmt.Sprintf(
						"- %-6s | PE=%s | ROE=%s%% | Div=%s%% | PBV=%s | RevGrow=%s%% | F-Score=%s\n",
						sym, getF("pe_ttm"), getF("roe_ttm"), getF("dividend_yield"),
						getF("pbv"), getF("revenue_growth_yoy"), getF("piotroski_score"),
					))
				}
				contextBuilder.WriteString("\n")
			}

		case "company_info":
			// Get company profile per symbol from Redis (populated by Go StockbitCollector)
			if s.redisCache == nil {
				log.Printf("company_info: redisCache not configured")
				break
			}
			if len(reqBody.Symbols) > 0 {
				contextBuilder.WriteString("=== PROFIL PERUSAHAAN ===\n")
				contextBuilder.WriteString("Format: SIMBOL | Nama | Sektor | Harga | Change% | Indeks utama\n")
				for _, sym := range reqBody.Symbols {
					var ci struct {
						Symbol    string   `json:"symbol"`
						Name      string   `json:"name"`
						Sector    string   `json:"sector"`
						SubSector string   `json:"sub_sector"`
						Price     string   `json:"price"`
						Change    string   `json:"change"`
						Pct       float64  `json:"pct"`
						Indexes   []string `json:"indexes"`
					}
					if err := s.redisCache.Get(r.Context(), fmt.Sprintf("stockbit:company_info:%s", sym), &ci); err != nil {
						contextBuilder.WriteString(fmt.Sprintf("- %s: profil belum tersedia\n", sym))
						continue
					}
					// Pick notable indexes
					notable := []string{}
					priority := []string{"LQ45", "IDX30", "IDX80", "IHSG", "IDXBUMN20", "IDXHIDIV20"}
					for _, p := range priority {
						for _, idx := range ci.Indexes {
							if idx == p {
								notable = append(notable, p)
								break
							}
						}
					}
					idxStr := strings.Join(notable, ", ")
					if idxStr == "" {
						idxStr = "-"
					}
					contextBuilder.WriteString(fmt.Sprintf(
						"- %-6s | %s | %s/%s | Rp%s (%+.2f%%) | Indeks: %s\n",
						sym, ci.Name, ci.Sector, ci.SubSector, ci.Price, ci.Pct, idxStr,
					))
				}
				contextBuilder.WriteString("\n")
			}
		}
	}

	contextBuilder.WriteString("=== PERTANYAAN USER ===\n")
	contextBuilder.WriteString(reqBody.Prompt)
	contextBuilder.WriteString("\n\nJawab berdasarkan DATA di atas. Jangan membuat asumsi atau data yang tidak ada. Fokus pada insight yang actionable.")

	fullPrompt := contextBuilder.String()

	// Stream LLM response
	err := s.llmClient.AnalyzeStream(r.Context(), fullPrompt, func(chunk string) error {
		// Properly format multi-line chunks for SSE
		lines := strings.Split(chunk, "\n")
		for i, line := range lines {
			if i < len(lines)-1 {
				fmt.Fprintf(w, "data: %s\n", line)
			} else {
				fmt.Fprintf(w, "data: %s\n\n", line)
			}
		}
		flusher.Flush()
		return nil
	})

	if err != nil {
		log.Printf("LLM streaming failed: %v", err)
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return
	}

	// Send completion event
	fmt.Fprintf(w, "event: done\ndata: Stream completed\n\n")
	flusher.Flush()
}
