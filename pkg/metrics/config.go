package metrics

// Config holds settings for the metrics query subsystem.
type Config struct {
	// PrometheusURL is the base URL of the Prometheus-compatible query API
	// (e.g. "http://prometheus.kyber-system:9090"). When empty, all time-series
	// panels return empty results rather than erroring — telemetry is optional.
	PrometheusURL string

	// StalenessThresholdSeconds is the age (in seconds) beyond which a
	// missing heartbeat is considered stale. Agents whose last heartbeat
	// exceeds this threshold are flagged in the Last Active panel.
	// Defaults to 300 (5 minutes) when zero.
	StalenessThresholdSeconds int

	// TokenRatesPath is the file path of the provider-rates.yaml ConfigMap
	// mount. When empty, token cost computation is skipped (costs show as 0).
	TokenRatesPath string

	// PrometheusInsecureSkipVerify allows connecting to a Prometheus endpoint
	// with a self-signed certificate. Defaults to false (TLS verification enforced).
	PrometheusInsecureSkipVerify bool
}

func (c Config) stalenessThreshold() int {
	if c.StalenessThresholdSeconds <= 0 {
		return 300
	}
	return c.StalenessThresholdSeconds
}
