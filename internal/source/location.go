package source

import "time"

// Location is an inventory observation, not a query-time availability probe.
// Selectors run from the original file towards the requested document.
type Location struct {
	RootID       string    `json:"root_id"`
	RelativePath string    `json:"relative_path"`
	DisplayPath  string    `json:"display_path"`
	Selectors    []string  `json:"selectors"`
	Status       string    `json:"status"`
	ObservedAt   time.Time `json:"observed_at"`
}

func DisplayLocations(locations []Location) (file, container string) {
	if len(locations) == 0 {
		return "", ""
	}
	l := locations[0]
	if len(l.Selectors) == 0 {
		return l.DisplayPath, ""
	}
	return "", l.DisplayPath
}
