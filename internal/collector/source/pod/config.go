// Package pod provides the Pod scraping metrics source implementation.
//
// This file contains configuration types and defaults for PodScrapingSource.
package pod

import "time"

// PodScrapingSourceConfig contains configuration for pod scraping.
type PodScrapingSourceConfig struct {
	// Service identification (required)
	ServiceName      string
	ServiceNamespace string

	// Metrics endpoint (provided by client/engine)
	MetricsPort   int32  // provided by client
	MetricsPath   string // provided by client, default: "/metrics"
	MetricsScheme string // provided by client, default: "http"

	// Authentication
	BearerToken string
	// BearerTokenFile, when set, is read on every scrape and takes precedence
	// over BearerToken, which then serves only as the fallback while the file
	// is unreadable. Read per scrape so a token rotated on disk -- a re-created
	// Secret, or a projected token the kubelet refreshes -- is used as soon as
	// it lands, instead of the copy taken when the pool was registered.
	BearerTokenFile string

	// Scraping behavior
	ScrapeTimeout        time.Duration // default: 5s per pod
	MaxConcurrentScrapes int           // default: 10

	// Cache configuration
	DefaultTTL time.Duration // default: 30s
}
