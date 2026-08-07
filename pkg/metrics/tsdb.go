package metrics

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// TSDBClient is a minimal Prometheus HTTP API client scoped to range and
// instant queries. All PromQL is constructed server-side from package-level
// constants — no raw user input reaches the query API.
type TSDBClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewTSDBClient builds a TSDBClient for the given Prometheus base URL.
// Returns nil when url is empty (telemetry not configured).
func NewTSDBClient(cfg Config) *TSDBClient {
	if cfg.PrometheusURL == "" {
		return nil
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.PrometheusInsecureSkipVerify, //nolint:gosec
		},
	}
	return &TSDBClient{
		baseURL: cfg.PrometheusURL,
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
	}
}

// promRangeResponse is the Prometheus /api/v1/query_range response envelope.
type promRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][]interface{}   `json:"values"`
		} `json:"result"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// promInstantResponse is the Prometheus /api/v1/query response envelope.
type promInstantResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
		} `json:"result"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// SeriesPoint is a single (timestamp, value) pair.
type SeriesPoint struct {
	Timestamp int64   `json:"ts"`
	Value     float64 `json:"v"`
}

// Series is a labelled time series.
type Series struct {
	Labels map[string]string `json:"labels"`
	Points []SeriesPoint     `json:"points"`
}

// InstantSample is a single instant-query sample.
type InstantSample struct {
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

// QueryRange executes a Prometheus range query and returns structured series.
// Returns empty slice (not error) when the TSDB is unreachable or returns no data.
func (c *TSDBClient) QueryRange(ctx context.Context, query string, start, end int64, step string) ([]Series, error) {
	if c == nil {
		return nil, nil
	}
	u := fmt.Sprintf("%s/api/v1/query_range", c.baseURL)
	params := url.Values{
		"query": {query},
		"start": {strconv.FormatInt(start, 10)},
		"end":   {strconv.FormatInt(end, 10)},
		"step":  {step},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+params.Encode(), nil)
	if err != nil {
		return nil, nil
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close() //nolint:errcheck

	var pr promRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil || pr.Status != "success" {
		return nil, nil
	}

	out := make([]Series, 0, len(pr.Data.Result))
	for _, r := range pr.Data.Result {
		pts := make([]SeriesPoint, 0, len(r.Values))
		for _, v := range r.Values {
			if len(v) < 2 {
				continue
			}
			ts, ok1 := v[0].(float64)
			valStr, ok2 := v[1].(string)
			if !ok1 || !ok2 {
				continue
			}
			val, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				continue
			}
			pts = append(pts, SeriesPoint{Timestamp: int64(ts), Value: val})
		}
		out = append(out, Series{Labels: r.Metric, Points: pts})
	}
	return out, nil
}

// QueryInstant executes a Prometheus instant query evaluated at "now" and
// returns samples. Returns empty slice (not error) when the TSDB is unreachable
// or returns no data.
func (c *TSDBClient) QueryInstant(ctx context.Context, query string) ([]InstantSample, error) {
	return c.QueryInstantAt(ctx, query, 0)
}

// QueryInstantAt executes a Prometheus instant query evaluated at evalUnix
// (Unix seconds) and returns samples. When evalUnix <= 0 the query is evaluated
// at the server's "now" (Prometheus default). Evaluating at an explicit time is
// what lets a range function like increase(metric[<dur>]) be anchored to the
// end of a requested window rather than to the current instant (kyber#428).
// Returns empty slice (not error) when the TSDB is unreachable or returns no data.
func (c *TSDBClient) QueryInstantAt(ctx context.Context, query string, evalUnix int64) ([]InstantSample, error) {
	if c == nil {
		return nil, nil
	}
	u := fmt.Sprintf("%s/api/v1/query", c.baseURL)
	params := url.Values{
		"query": {query},
	}
	if evalUnix > 0 {
		params.Set("time", strconv.FormatInt(evalUnix, 10))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+params.Encode(), nil)
	if err != nil {
		return nil, nil
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close() //nolint:errcheck

	var pr promInstantResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil || pr.Status != "success" {
		return nil, nil
	}

	out := make([]InstantSample, 0, len(pr.Data.Result))
	for _, r := range pr.Data.Result {
		if len(r.Value) < 2 {
			continue
		}
		valStr, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		out = append(out, InstantSample{Labels: r.Metric, Value: val})
	}
	return out, nil
}

