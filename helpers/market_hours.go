package helpers

import (
	"fmt"
	"time"
)

// Market hours constants — single source of truth for the entire system
const (
	MarketOpenHour  = 9             // 09:00 WIB
	MarketCloseHour = 16            // 16:00 WIB
	MarketTimeZone  = "Asia/Jakarta"
)

// WIBLocation returns the Asia/Jakarta timezone, with a safe fallback.
func WIBLocation() *time.Location {
	loc, err := time.LoadLocation(MarketTimeZone)
	if err != nil {
		return time.FixedZone("WIB", 7*60*60)
	}
	return loc
}

// IsMarketOpen checks if the current time is within IDX market hours
// (Monday - Friday, 09:00 - 16:00 WIB)
func IsMarketOpen() bool {
	now := time.Now().In(WIBLocation())

	// Weekend → always closed
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		return false
	}

	hour := now.Hour()
	return hour >= MarketOpenHour && hour < MarketCloseHour
}

// GetMarketStatus returns a human-readable market session string.
// Handles weekends correctly (returns "CLOSED").
func GetMarketStatus(now time.Time) string {
	wib := now.In(WIBLocation())

	// Weekend always closed
	if wib.Weekday() == time.Saturday || wib.Weekday() == time.Sunday {
		return "CLOSED"
	}

	h, m := wib.Hour(), wib.Minute()

	switch {
	case h == 8 && m >= 30:
		return "PRE_MARKET"
	case h == 9 || (h >= 9 && h < 12):
		return "SESSION_1"
	case h == 12 || (h == 13 && m < 30):
		return "LUNCH_BREAK"
	case (h == 13 && m >= 30) || (h >= 14 && h < 15):
		return "SESSION_2"
	case h == 15:
		return "PRE_CLOSING"
	default:
		return "CLOSED"
	}
}

// GetSmartTimeframe computes the appropriate start time for Aktivitas Bandar
// based on the current WIB time:
//   - Before 08:30 → last 24 hours (previous session)
//   - 08:30–09:00 → since 08:30 (pre-market)
//   - 09:00–close  → since 09:00 (live session)
func GetSmartTimeframe(now time.Time) (startTime time.Time, hoursBack float64, description string) {
	wib := now.In(WIBLocation())

	marketOpen := time.Date(wib.Year(), wib.Month(), wib.Day(), MarketOpenHour, 0, 0, 0, WIBLocation())
	preMarket := time.Date(wib.Year(), wib.Month(), wib.Day(), 8, 30, 0, 0, WIBLocation())

	switch {
	case wib.Before(preMarket):
		// Before 08:30 → show last 24 hours
		startTime = wib.Add(-24 * time.Hour)
		hoursBack = 24.0
		description = "Last 24 hours (pre-market view)"
	case wib.Before(marketOpen):
		// 08:30–09:00 → pre-market
		startTime = preMarket
		hoursBack = wib.Sub(preMarket).Hours()
		description = fmt.Sprintf("Pre-market activity (since 08:30 WIB, %.1f hours)", hoursBack)
	default:
		// 09:00+ → live session
		startTime = marketOpen
		hoursBack = wib.Sub(marketOpen).Hours()
		description = fmt.Sprintf("Today's session (since 09:00 WIB, %.1f hours)", hoursBack)
	}
	return
}
