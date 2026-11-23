package tokenreport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/matty-v/kyber/pkg/activity"
	"golang.org/x/sys/unix"
)

// HermesWorkInventory aggregates the snapshots published INSIDE every native
// Hermes process. PID start ticks validate only these registered publishers;
// this is not a scan that treats arbitrary OS processes as agent work.
func HermesWorkInventory(root, podUID string) (activity.Snapshot, error) {
	result := activity.Snapshot{ObservedAt: time.Now().UTC(), Complete: true}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) > 4096 || podUID == "" {
		return activity.Snapshot{}, errors.New("native publishers unavailable")
	}
	live := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return activity.Snapshot{}, errors.New("invalid publisher file")
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		parts := strings.Split(name, "-")
		if len(parts) != 2 {
			return activity.Snapshot{}, errors.New("invalid publisher identity")
		}
		pid, err := strconv.Atoi(parts[0])
		if err != nil || pid <= 0 {
			return activity.Snapshot{}, errors.New("invalid publisher identity")
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 4096 {
			return activity.Snapshot{}, errors.New("native publisher too large")
		}
		file, err := os.OpenFile(filepath.Join(root, entry.Name()), os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return activity.Snapshot{}, errors.New("native publisher unreadable")
		}
		data, err := io.ReadAll(io.LimitReader(file, 4097))
		_ = file.Close()
		if err != nil || len(data) > 4096 {
			return activity.Snapshot{}, errors.New("native publisher unreadable")
		}
		var source struct {
			Version    int               `json:"version"`
			PID        int               `json:"pid"`
			StartTicks string            `json:"startTicks"`
			PodUID     string            `json:"podUID"`
			Closed     bool              `json:"closed"`
			Snapshot   activity.Snapshot `json:"snapshot"`
		}
		if json.Unmarshal(data, &source) != nil || source.PID != pid || source.StartTicks != parts[1] || source.PodUID == "" || source.Version != 1 {
			return activity.Snapshot{}, errors.New("native publisher scope invalid")
		}
		// A different pod cannot retain this pod's processes. Its old scratch
		// files are history, even when the volume preserves /tmp across rolls.
		if source.PodUID != podUID {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", parts[0], "stat"))
		dead := os.IsNotExist(err)
		if err != nil && !dead {
			return activity.Snapshot{}, errors.New("native publisher process unavailable")
		}
		if !dead {
			fields := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+1:])
			if len(fields) < 20 {
				return activity.Snapshot{}, errors.New("native publisher process invalid")
			}
			dead = fields[19] != parts[1] || fields[0] == "Z" || fields[0] == "X"
		}
		if dead {
			// Losing a publisher may leave native background children alive.
			// Only its explicit clean, complete zero-job close proves absence.
			if !source.Closed || !source.Snapshot.Complete || source.Snapshot.Foreground != 0 || source.Snapshot.Background != 0 {
				result.Complete = false
			}
			continue
		}
		if source.Closed {
			result.Complete = false
			continue
		}
		if !source.Snapshot.Valid(time.Now().UTC()) {
			return activity.Snapshot{}, errors.New("native publisher stale")
		}
		live++
		if source.Snapshot.ObservedAt.Before(result.ObservedAt) {
			result.ObservedAt = source.Snapshot.ObservedAt
		}
		result.Complete = result.Complete && source.Snapshot.Complete
		result.Foreground += source.Snapshot.Foreground
		result.Background += source.Snapshot.Background
		result.NeedsInput = result.NeedsInput || source.Snapshot.NeedsInput
		if source.Snapshot.LastWorkedAt.After(result.LastWorkedAt) {
			result.LastWorkedAt = source.Snapshot.LastWorkedAt
		}
		if result.Foreground > activity.MaxJobs || result.Background > activity.MaxJobs {
			return activity.Snapshot{}, errors.New("native publisher inventory exceeds bounds")
		}
	}
	if live == 0 {
		return activity.Snapshot{}, errors.New("no live native publishers")
	}
	if result.LastWorkedAt.After(result.ObservedAt) {
		result.LastWorkedAt = result.ObservedAt
	}
	return result, nil
}

func RunHermesWorkReporter(ctx context.Context, root, podUID, sidecarURL string) {
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(activity.ObservationInterval)
	defer ticker.Stop()
	var lastWorked time.Time
	for {
		snapshot, err := HermesWorkInventory(root, podUID)
		if err != nil {
			snapshot = activity.Snapshot{ObservedAt: time.Now().UTC(), Complete: false}
		}
		if snapshot.Foreground > 0 || snapshot.Background > 0 {
			lastWorked = snapshot.ObservedAt
		}
		if snapshot.LastWorkedAt.After(lastWorked) {
			lastWorked = snapshot.LastWorkedAt
		}
		snapshot.LastWorkedAt = lastWorked
		_ = postWorkSnapshot(ctx, client, sidecarURL, snapshot)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
