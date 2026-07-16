package usage

import "time"

func cutoffStartUTCAt(now time.Time, days int) time.Time {
	return cutoffStartUTCAtLocation(now, days, getUsageLocation())
}

func cutoffStartUTCAtLocation(now time.Time, days int, loc *time.Location) time.Time {
	if days < 1 {
		days = 7
	}
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)
	todayStartLocal := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return todayStartLocal.AddDate(0, 0, -(days - 1)).UTC()
}

// CutoffStartUTC returns the start-of-day cutoff for the given number of days
// in the project-configured timezone, converted to UTC. Exported so that
// dashboard and other callers can reuse the same time-range semantics.
func CutoffStartUTC(days int) time.Time {
	return cutoffStartUTCAt(time.Now(), days)
}

func localDayKeyAt(t time.Time) string {
	return localDayKeyAtLocation(t, getUsageLocation())
}

func localDayKeyAtLocation(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("2006-01-02")
}

// LocalDayKeyAt returns the YYYY-MM-DD day key in the project-configured timezone.
func LocalDayKeyAt(t time.Time) string {
	return localDayKeyAt(t)
}

func cutoffDayKey(days int) string {
	return localDayKeyAt(CutoffStartUTC(days))
}
