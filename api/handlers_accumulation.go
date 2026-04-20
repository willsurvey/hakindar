package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"stockbit-haka-haki/helpers"
)

// handleAccumulationSummary returns separate top 20 accumulation and distribution lists.
// Smart timeframe logic is handled by helpers.GetSmartTimeframe().
func (s *Server) handleAccumulationSummary(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	now := time.Now()
	var startTime time.Time
	var hoursBack float64
	var timeframeDescription string

	if h := query.Get("hours"); h != "" {
		// Manual override via query parameter
		if parsed, err := strconv.Atoi(h); err == nil {
			hoursBack = float64(parsed)
			startTime = now.Add(-time.Duration(parsed) * time.Hour)
			timeframeDescription = "Last " + strconv.Itoa(parsed) + " hours (manual)"
		} else {
			startTime, hoursBack, timeframeDescription = helpers.GetSmartTimeframe(now)
		}
	} else {
		startTime, hoursBack, timeframeDescription = helpers.GetSmartTimeframe(now)
	}

	log.Printf("[handleAccumulationSummary] startTime=%s, hoursBack=%.2f, description=%s",
		startTime.Format("2006-01-02 15:04:05"), hoursBack, timeframeDescription)

	accumulation, distribution, err := s.repo.GetAccumulationDistributionSummary(startTime)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accumulation":       accumulation,
		"distribution":       distribution,
		"accumulation_count": len(accumulation),
		"distribution_count": len(distribution),
		"hours_back":         hoursBack,
		"timeframe":          timeframeDescription,
		"current_time":       now.In(helpers.WIBLocation()).Format("2006-01-02 15:04:05"),
		"market_status":      helpers.GetMarketStatus(now),
	})
}
