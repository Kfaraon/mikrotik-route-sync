package version

import (
	"fmt"
	"runtime"
	"time"
)

// Переменные устанавливаются через ldflags при сборке:
// go build -ldflags "-X github.com/Kfaraon/mikrotik-route-sync/internal/version.Version=1.0.0
//                    -X github.com/Kfaraon/mikrotik-route-sync/internal/version.Commit=abc123
//                    -X github.com/Kfaraon/mikrotik-route-sync/internal/version.BuildDate=2024-01-01T00:00:00Z"
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// Info содержит полную информацию о сборке.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Get возвращает полную информацию о сборке.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildDate: BuildDate,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

// String возвращает человекочитаемую строку с информацией о версии.
func String() string {
	return fmt.Sprintf("Version: %s\nCommit: %s\nBuild Date: %s\nGo Version: %s\nOS/Arch: %s/%s",
		Version, Commit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Short возвращает короткую строку версии.
func Short() string {
	return fmt.Sprintf("%s (%s)", Version, Commit[:7])
}

// ParseBuildDate парсит дату сборки в time.Time.
func ParseBuildDate() (time.Time, error) {
	if BuildDate == "unknown" {
		return time.Time{}, fmt.Errorf("build date is unknown")
	}
	
	// Пробуем разные форматы
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	
	for _, format := range formats {
		if t, err := time.Parse(format, BuildDate); err == nil {
			return t, nil
		}
	}
	
	return time.Time{}, fmt.Errorf("unable to parse build date: %s", BuildDate)
}
