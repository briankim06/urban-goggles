package graph

// Stop represents a GTFS station or platform.
type Stop struct {
	ID            string
	Name          string
	Lat           float64
	Lon           float64
	LocationType  int    // 1 = station (parent), 0/empty = platform (child)
	ParentStation string // empty for parent stations
}

// Route represents a subway line.
type Route struct {
	ID        string
	ShortName string
	LongName  string
	Color     string
}

// Trip maps a trip_id to its route, service, and direction.
type Trip struct {
	ID          string
	RouteID     string
	ServiceID   string
	DirectionID int
}

// ScheduledStopTime is one row from stop_times.txt with the time converted
// to seconds since midnight (may exceed 86400 for past-midnight trips).
type ScheduledStopTime struct {
	TripID       string
	StopID       string
	ArrivalSecs  int // seconds since midnight
	DepartureSecs int
	StopSequence int
}

// Transfer represents one row from transfers.txt.
type Transfer struct {
	FromStopID      string
	ToStopID        string
	TransferType    int
	MinTransferTime int // seconds; relevant when TransferType == 2
}
