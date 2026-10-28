package server

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

const breadcrumbsFilename = "breadcrumbs.json"

// Breadcrumb mirrors the Swift client's Breadcrumb.
type Breadcrumb struct {
	At      time.Time         `json:"at"`
	Kind    string            `json:"kind"`
	Message string            `json:"message"`
	Data    map[string]string `json:"data,omitempty"`
}

// BreadcrumbRow is one line of the issue page's timeline.
type BreadcrumbRow struct {
	Breadcrumb
	Before string // how long before the report, e.g. "1m 05s"
	Failed bool   // a request that failed or returned 4xx/5xx
}

// breadcrumbs reads a report's breadcrumbs.json, newest last.
func (s *Server) breadcrumbs(rec *Record) []BreadcrumbRow {
	for _, f := range rec.Files {
		if f.Filename != breadcrumbsFilename {
			continue
		}
		path, _, err := s.store.FilePath(rec.ID, f.Name)
		if err != nil {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var crumbs []Breadcrumb
		if json.Unmarshal(data, &crumbs) != nil {
			return nil
		}
		sort.SliceStable(crumbs, func(i, j int) bool { return crumbs[i].At.Before(crumbs[j].At) })
		rows := make([]BreadcrumbRow, 0, len(crumbs))
		for _, c := range crumbs {
			row := BreadcrumbRow{Breadcrumb: c, Before: beforeReport(rec.CreatedAt.Sub(c.At))}
			if c.Kind == "network" {
				row.Failed = c.Data["error"] != "" || (len(c.Data["status"]) == 3 && c.Data["status"] >= "400")
			}
			rows = append(rows, row)
		}
		return rows
	}
	return nil
}

func beforeReport(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}
