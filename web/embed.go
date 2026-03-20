package web

import "embed"

// DashboardFS holds the embedded dashboard static files.
// The dashboard must be built before compiling the Go binary.
// If the dist/ directory doesn't exist, the embed will be empty
// and the dashboard will not be available.
//
//go:embed all:dashboard/dist
var DashboardFS embed.FS
