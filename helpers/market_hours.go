package helpers

import "time"

// IsMarketOpen checks if the current time is within IDX market hours
// (Monday - Friday, 09:00 - 16:00 WIB)
func IsMarketOpen() bool {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		// Fallback to local time if timezone data is missing
		loc = time.Local
	}

	now := time.Now().In(loc)
	
	// Check if weekend
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		return false
	}

	hour := now.Hour()
	// Market opens at 09:00 and closes at 16:00
	if hour >= 9 && hour < 16 {
		return true
	}

	// For exactly 16:00, we might want to consider it recently closed
	// but generally strictly false after 16:00:00
	return false
}
