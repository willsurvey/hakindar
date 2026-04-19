package llm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"stockbit-haka-haki/database"
	"stockbit-haka-haki/database/types"
)

// Constants for value formatting
const (
	billionDivisor = 1_000_000_000
	millionDivisor = 1_000_000
	maxAnomalies   = 10
	maxPromptWords = 200
)

// SymbolRedisContext holds enriched real-time data from Redis for a specific symbol.
// All fields are optional — zero values mean data was not available and the
// corresponding prompt section will be omitted gracefully.
type SymbolRedisContext struct {
	// Company Info (stockbit:company_info:{SYM}) — populated every 10 min
	Name      string
	Sector    string
	SubSector string
	Price     string
	ChangePct float64
	Volume    string
	Indexes   []string

	// Intraday Broker Flow (stockbit:running_trade) — populated every 3 min
	RTBuyLot        int
	RTSellLot       int
	RTNetLot        int
	RTForeignNet    int
	RTDominantBuyer string
	RTAvailable     bool

	// Bandar Detector / Broker Signal (stockbit:broker:{SYM}) — populated every 5 min
	BrokerLabel string
	BrokerScore int
	BrokerAvail bool

	// Fase 2: Fundamental Keystats (stockbit:keystats:{SYM}) — populated every 10 min
	PeTTM            *float64
	RoeTTM           *float64
	PBV              *float64
	DividendYield    *float64
	RevenueGrowthYoY *float64
	PiotroskiScore   *float64
	High52W          *float64
	Low52W           *float64
	DebtToEquity     *float64
	KeystatsAvail    bool

	// Fase 2: Historical Daily Bars (stockbit:hist:{SYM}) — populated every 5 min
	HistBars []HistBar

	// Fase 3: Orderbook Bid/Ask walls (stockbit:orderbook:{SYM}) — populated every 3 min
	TopBids      []OBLevel
	TopAsks      []OBLevel
	OrderbookAvail bool

	// Fase 3: Market Mover Positioning (stockbit:universe:*) — populated every 5 min
	InTopVolume  bool
	InTopValue   bool
	InForeignBuy bool
	InTopGainer  bool
	InTopLoser   bool
}

// HistBar represents one day of OHLCV + foreign flow data for the prompt context.
type HistBar struct {
	Date       string
	Close      float64
	ChangePct  float64
	Volume     int64
	NetForeign float64
}

// OBLevel represents one price level in the orderbook (bid or ask).
type OBLevel struct {
	Price float64
	Lot   int64
}


// alertCounts aggregates alert statistics by action type
type alertCounts struct {
	buyCount          int
	sellCount         int
	unknownCount      int
	totalBuyValue     float64
	totalSellValue    float64
	totalUnknownValue float64
	maxBuyAlert       database.WhaleAlert
	maxSellAlert      database.WhaleAlert
	maxUnknownAlert   database.WhaleAlert
	maxBuyValue       float64
	maxSellValue      float64
	maxUnknownValue   float64
}

// countAlerts processes a list of whale alerts and returns aggregated statistics
func countAlerts(alerts []database.WhaleAlert, trackMax bool) alertCounts {
	counts := alertCounts{}

	for _, a := range alerts {
		switch a.Action {
		case "BUY":
			counts.buyCount++
			counts.totalBuyValue += a.TriggerValue
			if trackMax && a.TriggerValue > counts.maxBuyValue {
				counts.maxBuyValue = a.TriggerValue
				counts.maxBuyAlert = a
			}
		case "SELL":
			counts.sellCount++
			counts.totalSellValue += a.TriggerValue
			if trackMax && a.TriggerValue > counts.maxSellValue {
				counts.maxSellValue = a.TriggerValue
				counts.maxSellAlert = a
			}
		default:
			counts.unknownCount++
			counts.totalUnknownValue += a.TriggerValue
			if trackMax && a.TriggerValue > counts.maxUnknownValue {
				counts.maxUnknownValue = a.TriggerValue
				counts.maxUnknownAlert = a
			}
		}
	}

	return counts
}

// safeFloat64 safely dereferences a float64 pointer, returning defaultValue if nil
func safeFloat64(ptr *float64, defaultValue float64) float64 {
	if ptr != nil {
		return *ptr
	}
	return defaultValue
}

// FormatAccumulationPrompt creates a prompt for LLM to analyze accumulation/distribution patterns
func FormatAccumulationPrompt(patterns []types.AccumulationPattern, regimes map[string]database.MarketRegime) string {
	var sb strings.Builder
	sb.Grow(1024 + len(patterns)*300)

	sb.WriteString("Anda adalah analis market saham expert spesialis Bandarlogy (Institusi). Analisis pola berikut berdasarkan DATA FAKTUAL:\n\n")

	for i, p := range patterns {
		duration := p.LastAlertTime.Sub(p.FirstAlertTime).Minutes()
		avgPrice := 0.0
		if p.TotalVolumeLots > 0 {
			avgPrice = p.TotalValue / (p.TotalVolumeLots * 100)
		}

		regimeText := "N/A"
		if r, ok := regimes[p.StockSymbol]; ok {
			regimeText = fmt.Sprintf("%s (Conf: %.0f%%)", r.Regime, r.Confidence*100)
		}

		sb.WriteString(fmt.Sprintf("%d. **%s** (%s)\n", i+1, p.StockSymbol, p.Action))
		sb.WriteString(fmt.Sprintf("   - Intensitas: %d alert dlm %.0f mnt (Interval: %.1f mnt)\n", p.AlertCount, duration, duration/float64(p.AlertCount)))
		sb.WriteString(fmt.Sprintf("   - Total Value: Rp %.2f Miliar | Avg Price: %.0f\n", p.TotalValue/billionDivisor, avgPrice))
		sb.WriteString(fmt.Sprintf("   - Market Context: %s\n", regimeText))
		sb.WriteString(fmt.Sprintf("   - Kekuatan Anomali (Avg Z-Score): %.2f\n\n", p.AvgZScore))
	}

	sb.WriteString("Tugas Analisis (DATA DRIVEN):\n")
	sb.WriteString("1. **Identifikasi Fase**: Lihat interval & volume, apakah Akumulasi rapi atau Haka panik?\n")
	sb.WriteString("2. **Signifikansi**: Apakah nilai ini cukup besar dibanding 'Market Context' (Regime) saham tersebut?\n")
	sb.WriteString("3. **Skenario**: Jika harga koreksi ke level average price, apakah 'Buy on Dip' valid?\n")
	sb.WriteString(fmt.Sprintf("\nJawab tajam, fokus pada angka. Maksimal %d kata.", maxPromptWords))

	return sb.String()
}

// FormatAnomalyPrompt creates a prompt for analyzing extreme Z-score events with market context
func FormatAnomalyPrompt(anomalies []database.WhaleAlert, regimes map[string]database.MarketRegime) string {
	var sb strings.Builder
	sb.Grow(1024 + len(anomalies)*300)

	sb.WriteString("Analisis Black Swan Event / Anomali Statistik Ekstrem:\n\n")

	for i, a := range anomalies {
		if i >= maxAnomalies {
			break
		}

		zScore := safeFloat64(a.ZScore, 0.0)
		volPct := safeFloat64(a.VolumeVsAvgPct, 0.0)
		timeSince := time.Since(a.DetectedAt).Minutes()
		avgPrice := safeFloat64(a.AvgPrice, a.TriggerPrice)

		devPct := 0.0
		if avgPrice > 0 {
			devPct = ((a.TriggerPrice - avgPrice) / avgPrice) * 100
		}

		regimeText := "N/A"
		if r, ok := regimes[a.StockSymbol]; ok {
			volatility := 0.0
			if r.Volatility != nil {
				volatility = *r.Volatility
			}
			regimeText = fmt.Sprintf("%s (Volatilitas: %.2f%%)", r.Regime, volatility*100)
		}

		sb.WriteString(fmt.Sprintf("%d. **%s** (%s) - Z:%.2f\n", i+1, a.StockSymbol, a.Action, zScore))
		sb.WriteString(fmt.Sprintf("   - Waktu: %.0f mnt lalu | Vol Spike: %.0f%% vs Avg\n", timeSince, volPct))
		sb.WriteString(fmt.Sprintf("   - Harga: %.0f (Dev: %+.2f%%) | Value: Rp %.1f Juta\n", a.TriggerPrice, devPct, a.TriggerValue/millionDivisor))
		sb.WriteString(fmt.Sprintf("   - Market Regime: %s\n\n", regimeText))
	}

	sb.WriteString("Analisis Forensik:\n")
	sb.WriteString("1. **Sifat**: Berdasarkan Regime & Deviasi, apakah ini 'Breakout' valid atau 'Fat Finger'?\n")
	sb.WriteString("2. **Psikologi**: Adakah urgensi ekstrim (Panic/FOMO) dlm konteks volatilitas saat ini?\n")
	sb.WriteString("3. **Rekomendasi**: Follow the flow atau Fade the move?\n")
	sb.WriteString("\nBerikan insight algoritmik, singkat & padat.")

	return sb.String()
}

// FormatTimingPrompt creates a prompt for time-based pattern analysis
func FormatTimingPrompt(stats []types.TimeBasedStat) string {
	var sb strings.Builder
	sb.Grow(1024 + len(stats)*100)

	sb.WriteString("Analisis Time-Series Profiling dari aktivitas Smart Money:\n\n")

	for _, s := range stats {
		hour := s.TimeBucket
		netBuyVal := (s.TotalValue / float64(s.AlertCount)) // Rough avg value per alert

		sb.WriteString(fmt.Sprintf("🕒 **Jam %s:00**\n", hour))
		sb.WriteString(fmt.Sprintf("   - Aktivitas: %d alert (Beli: %d | Jual: %d)\n", s.AlertCount, s.BuyCount, s.SellCount))
		sb.WriteString(fmt.Sprintf("   - Total Perputaran Uang: Rp %.1f Miliar\n", s.TotalValue/billionDivisor))
		sb.WriteString(fmt.Sprintf("   - Avg Value per Alert: Rp %.1f Juta\n", netBuyVal/millionDivisor))
	}

	sb.WriteString("\nEvaluasi Strategis:\n")
	sb.WriteString("1. **Time Discovery**: Kapan 'Big Money' paling agresif? Apakah di Open, Mid-day, atau Close?\n")
	sb.WriteString("2. **Identifikasi Pola**: Adakah pola 'Morning Pump' atau 'Afternoon Dump'?\n")
	sb.WriteString("3. **Saran Eksekusi**: Kapan waktu terbaik bagi Retail untuk 'menumpang' arus ini?\n")
	sb.WriteString("\nJawab dengan gaya mentoring trading profesional.")

	return sb.String()
}

// AnalyzeSymbolContext generates LLM insights for a specific stock
func AnalyzeSymbolContext(client *Client, symbol string, alerts []database.WhaleAlert) (string, error) {
	if len(alerts) == 0 {
		return "", fmt.Errorf("tidak ada data aktivitas whale yang cukup untuk analisis %s", symbol)
	}

	var sb strings.Builder
	sb.Grow(1024)
	sb.WriteString(fmt.Sprintf("Lakukan Bedah Saham (Stock Opname) untuk **%s** berdasarkan aliran dana Bandar (Whale Flow):\n\n", symbol))

	counts := countAlerts(alerts, false)

	// Metrics Calculation
	totalTrans := counts.buyCount + counts.sellCount + counts.unknownCount
	totalVal := counts.totalBuyValue + counts.totalSellValue + counts.totalUnknownValue

	buyRatio := 0.0
	if totalVal > 0 {
		buyRatio = (counts.totalBuyValue / totalVal) * 100
	}

	avgBuySize := 0.0
	if counts.buyCount > 0 {
		avgBuySize = counts.totalBuyValue / float64(counts.buyCount)
	}

	avgSellSize := 0.0
	if counts.sellCount > 0 {
		avgSellSize = counts.totalSellValue / float64(counts.sellCount)
	}

	sb.WriteString(fmt.Sprintf("📊 **Statistik Kunci (%d Data Terakhir)**:\n", totalTrans))
	sb.WriteString(fmt.Sprintf("- 🟢 **Bulls (Buy)**: %d ord | Rp %.2f M | Avg Size: Rp %.1f Juta\n", counts.buyCount, counts.totalBuyValue/billionDivisor, avgBuySize/millionDivisor))
	sb.WriteString(fmt.Sprintf("- 🔴 **Bears (Sell)**: %d ord | Rp %.2f M | Avg Size: Rp %.1f Juta\n", counts.sellCount, counts.totalSellValue/billionDivisor, avgSellSize/millionDivisor))
	sb.WriteString(fmt.Sprintf("- ⚖️ **Dominasi Buyer**: %.1f%%\n", buyRatio))

	if counts.unknownCount > 0 {
		sb.WriteString(fmt.Sprintf("- ⚪ **Netral/Crossing**: %d transaksi (Total Rp %.2f M)\n", counts.unknownCount, counts.totalUnknownValue/billionDivisor))
	}

	// Add trend context
	if buyRatio > 65 {
		sb.WriteString("\nKonteks: **Strong Accumulation** (>65% Flow is Buy).\n")
	} else if buyRatio < 35 {
		sb.WriteString("\nKonteks: **Strong Distribution** (<35% Flow is Buy).\n")
	} else {
		sb.WriteString("\nKonteks: **Consolidation / Battle** (Power seimbang).\n")
	}

	sb.WriteString("\nAnalisis Cepat (Micro-Structure):\n")
	sb.WriteString("1. Bandingkan 'Avg Size' Buy vs Sell. Apakah pembeli lebih 'berani' (ukuran order lebih besar)?\n")
	sb.WriteString("2. Prediksi jangka pendek berdasarkan dominasi flow?\n")
	sb.WriteString("3. Skor Potensi Kenaikan (1-10)?\n")
	sb.WriteString("Jawab <100 kata. Langsung pada inti.")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	return client.Analyze(ctx, sb.String())
}

// FormatSymbolAnalysisPrompt creates a detailed prompt for symbol-specific streaming analysis.
// redisCtx enriches the prompt with real-time data from Redis; pass nil to skip enrichment.
func FormatSymbolAnalysisPrompt(
	symbol string,
	alerts []database.WhaleAlert,
	baseline *database.StatisticalBaseline,
	orderFlow *database.OrderFlowImbalance,
	followups []database.WhaleAlertFollowup,
	redisCtx *SymbolRedisContext,
) string {
	var sb strings.Builder
	sb.Grow(2048 + len(alerts)*100)

	sb.WriteString(fmt.Sprintf("Lakukan Deep Dive Analisis Arus Dana untuk **%s**:\n\n", symbol))

	// 0. Redis Enrichment — Real-time context (Fase 1)
	if redisCtx != nil {
		// 0a. Company Info & Harga Terkini
		if redisCtx.Name != "" {
			// Build index string, prioritize notable ones
			idxStr := "-"
			priority := []string{"LQ45", "IDX30", "IDX80", "IDXBUMN20", "IDXHIDIV20", "IHSG"}
			notable := []string{}
			for _, p := range priority {
				for _, idx := range redisCtx.Indexes {
					if idx == p {
						notable = append(notable, p)
						break
					}
				}
			}
			if len(notable) > 0 {
				idxStr = strings.Join(notable, ", ")
			}
			changeSign := "+"
			if redisCtx.ChangePct < 0 {
				changeSign = ""
			}
			sb.WriteString("🏢 **Profil & Harga Terkini**:\n")
			sb.WriteString(fmt.Sprintf("- %s | %s | %s\n", redisCtx.Name, redisCtx.Sector, redisCtx.SubSector))
			sb.WriteString(fmt.Sprintf("- Harga: Rp%s (%s%.2f%%) | Volume: %s\n",
				redisCtx.Price, changeSign, redisCtx.ChangePct, redisCtx.Volume))
			sb.WriteString(fmt.Sprintf("- Indeks: %s\n", idxStr))
			sb.WriteString("\n")
		}

		// 0b. Intraday Broker Flow (Running Trade)
		if redisCtx.RTAvailable {
			foreignTag := "NETRAL"
			if redisCtx.RTForeignNet > 500 {
				foreignTag = "🟢 ASING BELI"
			} else if redisCtx.RTForeignNet < -500 {
				foreignTag = "🔴 ASING JUAL"
			}
			netSign := "+"
			if redisCtx.RTNetLot < 0 {
				netSign = ""
			}
			sb.WriteString("📊 **Intraday Broker Flow (Running Trade)**:\n")
			sb.WriteString(fmt.Sprintf("- Buy: %d lot | Sell: %d lot | Net: %s%d lot\n",
				redisCtx.RTBuyLot, redisCtx.RTSellLot, netSign, redisCtx.RTNetLot))
			sb.WriteString(fmt.Sprintf("- Foreign Net: %+d lot — %s\n", redisCtx.RTForeignNet, foreignTag))
			if redisCtx.RTDominantBuyer != "" {
				sb.WriteString(fmt.Sprintf("- Broker Dominan (Buy): %s\n", redisCtx.RTDominantBuyer))
			}
			sb.WriteString("\n")
		}

		// 0c. Bandar Detector
		if redisCtx.BrokerAvail && redisCtx.BrokerLabel != "" {
			emoji := "⚪"
			switch redisCtx.BrokerLabel {
			case "Big Acc":
				emoji = "🟢🟢"
			case "Acc":
				emoji = "🟢"
			case "Dist":
				emoji = "🔴"
			case "Big Dist":
				emoji = "🔴🔴"
			}
			sb.WriteString(fmt.Sprintf("🎯 **Bandar Detector**: %s %s (Score: %d)\n\n",
				emoji, redisCtx.BrokerLabel, redisCtx.BrokerScore))
		}

		// 0d. Fundamental Keystats (Fase 2)
		if redisCtx.KeystatsAvail {
			fmtPtr := func(v *float64, format, suffix string) string {
				if v == nil {
					return "-"
				}
				return fmt.Sprintf(format+suffix, *v)
			}
			sb.WriteString("📈 **Fundamental Keystats (TTM)**:\n")
			sb.WriteString(fmt.Sprintf("- PE: %s | ROE: %s | PBV: %s | Div Yield: %s\n",
				fmtPtr(redisCtx.PeTTM, "%.1f", "x"),
				fmtPtr(redisCtx.RoeTTM, "%.1f", "%"),
				fmtPtr(redisCtx.PBV, "%.2f", "x"),
				fmtPtr(redisCtx.DividendYield, "%.2f", "%"),
			))
			sb.WriteString(fmt.Sprintf("- Revenue Growth YoY: %s | D/E Ratio: %s | F-Score: %s\n",
				fmtPtr(redisCtx.RevenueGrowthYoY, "%.1f", "%"),
				fmtPtr(redisCtx.DebtToEquity, "%.2f", "x"),
				fmtPtr(redisCtx.PiotroskiScore, "%.0f", "/9"),
			))
			if redisCtx.High52W != nil && redisCtx.Low52W != nil {
				sb.WriteString(fmt.Sprintf("- 52W Range: Rp%.0f – Rp%.0f | Range Width: Rp%.0f\n",
					*redisCtx.Low52W, *redisCtx.High52W, *redisCtx.High52W-*redisCtx.Low52W))
			}
			sb.WriteString("\n")
		}

		// 0e. Historical Daily Bars (Fase 2)
		if len(redisCtx.HistBars) > 0 {
			limit := 5
			if len(redisCtx.HistBars) < limit {
				limit = len(redisCtx.HistBars)
			}
			sb.WriteString(fmt.Sprintf("📅 **Historical %d Hari Terakhir**:\n", limit))
			sb.WriteString("Format: Tanggal | Close | Change% | Volume | Foreign Net\n")
			for i := 0; i < limit; i++ {
				bar := redisCtx.HistBars[i]
				fgnTag := ""
				if bar.NetForeign > 0 {
					fgnTag = fmt.Sprintf(" 🟢+%.0f", bar.NetForeign)
				} else if bar.NetForeign < 0 {
					fgnTag = fmt.Sprintf(" 🔴%.0f", bar.NetForeign)
				}
				sb.WriteString(fmt.Sprintf("- %s | Rp%.0f | %+.2f%% | Vol %.0fM%s\n",
					bar.Date, bar.Close, bar.ChangePct,
					float64(bar.Volume)/1_000_000, fgnTag))
			}
			sb.WriteString("\n")
		}

		// 0f. Orderbook Bid/Ask Walls (Fase 3)
		if redisCtx.OrderbookAvail && (len(redisCtx.TopBids) > 0 || len(redisCtx.TopAsks) > 0) {
			sb.WriteString("📒 **Orderbook (Antrian Terkini)**:\n")

			// Total bid vs ask lots for ratio
			var totalBidLot, totalAskLot int64
			for _, b := range redisCtx.TopBids {
				totalBidLot += b.Lot
			}
			for _, a := range redisCtx.TopAsks {
				totalAskLot += a.Lot
			}

			if len(redisCtx.TopBids) > 0 {
				sb.WriteString("- BID (Antrian Beli): ")
				parts := make([]string, 0, len(redisCtx.TopBids))
				for _, b := range redisCtx.TopBids {
					parts = append(parts, fmt.Sprintf("Rp%.0f (%d lot)", b.Price, b.Lot))
				}
				sb.WriteString(strings.Join(parts, " | "))
				sb.WriteString("\n")
			}
			if len(redisCtx.TopAsks) > 0 {
				sb.WriteString("- ASK (Antrian Jual): ")
				parts := make([]string, 0, len(redisCtx.TopAsks))
				for _, a := range redisCtx.TopAsks {
					parts = append(parts, fmt.Sprintf("Rp%.0f (%d lot)", a.Price, a.Lot))
				}
				sb.WriteString(strings.Join(parts, " | "))
				sb.WriteString("\n")
			}
			if totalBidLot > 0 && totalAskLot > 0 {
				ratio := float64(totalBidLot) / float64(totalAskLot)
				sentiment := "SEIMBANG"
				if ratio > 1.5 {
					sentiment = "🟢 DEMAND > SUPPLY"
				} else if ratio < 0.67 {
					sentiment = "🔴 SUPPLY > DEMAND"
				}
				sb.WriteString(fmt.Sprintf("- Bid/Ask Ratio: %.2fx — %s\n", ratio, sentiment))
			}
			sb.WriteString("\n")
		}

		// 0g. Market Mover Positioning (Fase 3)
		var moverTags []string
		if redisCtx.InTopVolume {
			moverTags = append(moverTags, "📊 TOP VOLUME")
		}
		if redisCtx.InTopValue {
			moverTags = append(moverTags, "💰 TOP VALUE")
		}
		if redisCtx.InForeignBuy {
			moverTags = append(moverTags, "🌍 NET FOREIGN BUY")
		}
		if redisCtx.InTopGainer {
			moverTags = append(moverTags, "🚀 TOP GAINER")
		}
		if redisCtx.InTopLoser {
			moverTags = append(moverTags, "📉 TOP LOSER")
		}
		if len(moverTags) > 0 {
			sb.WriteString("🌐 **Market Positioning Hari Ini**:\n")
			sb.WriteString("- " + strings.Join(moverTags, " | ") + "\n\n")
		}
	}

	// 1. Market Context & Statistics
	sb.WriteString("🌐 **Market Context & Baselines**:\n")
	if baseline != nil {
		sb.WriteString(fmt.Sprintf("- Stat Baseline: Mean Price %.0f, StdDev Vol %.1f Lots (Sample: %d)\n",
			baseline.MeanPrice, baseline.StdDevVolume, baseline.SampleSize))
	}
	if orderFlow != nil {
		sb.WriteString(fmt.Sprintf("- Order Flow Imbalance: **%.1f%%** (Aggressive Buy: %.1f%%, Aggressive Sell: %.1f%%)\n",
			orderFlow.ValueImbalanceRatio*100,
			safeFloat64(orderFlow.AggressiveBuyPct, 0),
			safeFloat64(orderFlow.AggressiveSellPct, 0)))
	}
	sb.WriteString("\n")

	// 2. Whale Activity Summary
	counts := countAlerts(alerts, true)
	totalVal := counts.totalBuyValue + counts.totalSellValue + counts.totalUnknownValue
	buyPct := 0.0
	if totalVal > 0 {
		buyPct = (counts.totalBuyValue / totalVal) * 100
	}

	sb.WriteString(fmt.Sprintf("📊 **Whale Flow Metrics (%d Transaksi Terakhir)**:\n", len(alerts)))
	sb.WriteString(fmt.Sprintf("- Total Flow: Rp %.1f Miliar\n", totalVal/billionDivisor))
	sb.WriteString(fmt.Sprintf("- 🐂 Buyer: Rp %.1f M (%.1f%%) | Avg Order: Rp %.0f Jt\n",
		counts.totalBuyValue/millionDivisor, buyPct, (counts.totalBuyValue/float64(counts.buyCount+1))/millionDivisor))
	sb.WriteString(fmt.Sprintf("- 🐻 Seller: Rp %.1f M (%.1f%%) | Avg Order: Rp %.0f Jt\n",
		counts.totalSellValue/millionDivisor, 100-buyPct, (counts.totalSellValue/float64(counts.sellCount+1))/millionDivisor))
	sb.WriteString("\n")

	// 3. Post-Trade Impact (Followups)
	if len(followups) > 0 {
		sb.WriteString("🔄 **Historical Post-Whale Impact**:\n")
		posImpact, negImpact := 0, 0
		for _, f := range followups {
			if f.ImmediateImpact != nil {
				if *f.ImmediateImpact == "POSITIVE" {
					posImpact++
				} else if *f.ImmediateImpact == "NEGATIVE" {
					negImpact++
				}
			}
		}
		sb.WriteString(fmt.Sprintf("- Reactivity: %.0f%% Positive Impact, %.0f%% Negative Impact setelah Whale masuk.\n",
			float64(posImpact)/float64(len(followups))*100,
			float64(negImpact)/float64(len(followups))*100))
		sb.WriteString("\n")
	}

	sb.WriteString("**Analisis Strategis (Instruksi)**:\n")
	sb.WriteString("1. **Market Structure**: Bandingkan Order Size & Flow Imbalance. Apakah ada akumulasi stealth?\n")
	sb.WriteString("2. **Impact Analysis**: Berdasarkan historical reactivity, seberapa kuat probabilitas harga akan merespon whale saat ini?\n")
	sb.WriteString("3. **Executive Verdict**: \n")
	sb.WriteString("   - **Signal**: AGGRESSIVE BUY / ACCUMULATION / WAIT / DISTRIBUTION\n")
	sb.WriteString("   - **Rationale**: Penjelasan matematis berdasarkan Flow + Broker Signal + Impact.\n")
	sb.WriteString(fmt.Sprintf("\nJawab tajam, profesional, dilarang halusinasi. Maksimal %d kata.", maxPromptWords))

	return sb.String()
}
