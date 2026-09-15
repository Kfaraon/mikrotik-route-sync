package history

import "time"

type Record struct {
	Time                                             time.Time `json:"time"`
	Service, Method, Status, Error                   string    `json:",omitempty"`
	Collected, Aggregated, Added, Removed, Unchanged int
	DurationMS                                       int64 `json:"duration_ms"`
}
