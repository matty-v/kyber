package tokenreport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/matty-v/kyber/pkg/activity"
)

func postWorkSnapshot(ctx context.Context, client *http.Client, sidecarURL string, snapshot activity.Snapshot) error {
	if sidecarURL == "" {
		sidecarURL = "http://127.0.0.1:8091"
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(sidecarURL, "/")+"/work-inventory", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("work inventory sidecar unavailable")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 204 {
		return errors.New("work inventory not accepted")
	}
	return nil
}
