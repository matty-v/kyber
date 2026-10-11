package tokenreport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// HermesUsageCollector follows the per-call summary log with a durable PVC
// cursor. The log's timestamps have no zone, so Location must be the pod's
// configured timezone captured at collection time.
type HermesUsageCollector struct {
	LogPath          string
	CursorPath       string
	RouteFingerprint string
	Location         *time.Location
	Sink             ClaudeUsageSink
}

func (c *HermesUsageCollector) Scan(ctx context.Context) error {
	if c.LogPath == "" || c.CursorPath == "" || c.RouteFingerprint == "" || c.Location == nil || c.Sink == nil {
		return ErrIncompleteUsage
	}
	f, err := os.Open(c.LogPath)
	if errors.Is(err, fs.ErrNotExist) {
		if _, cursorErr := os.Stat(c.CursorPath); errors.Is(cursorErr, fs.ErrNotExist) {
			return saveClaudeCursor(c.CursorPath, claudeCursorState{RouteFingerprint: c.RouteFingerprint, Files: map[string]claudeFileCursor{}})
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening Hermes usage log: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	dev, ino := fileIdentity(info)
	state, err := readClaudeCursor(c.CursorPath)
	if errors.Is(err, fs.ErrNotExist) {
		end, err := completeEOF(c.LogPath)
		if err != nil {
			return err
		}
		return saveClaudeCursor(c.CursorPath, claudeCursorState{RouteFingerprint: c.RouteFingerprint,
			Files: map[string]claudeFileCursor{c.LogPath: {Namespace: uuid.NewString(), Offset: end, Device: dev, Inode: ino}}})
	}
	if err != nil {
		return err
	}
	file, known := state.Files[c.LogPath]
	if !known {
		file = claudeFileCursor{Namespace: uuid.NewString(), Device: dev, Inode: ino}
		state.Files[c.LogPath] = file
		if err := saveClaudeCursor(c.CursorPath, state); err != nil {
			return err
		}
	}
	if state.RouteFingerprint != c.RouteFingerprint {
		end, err := completeEOF(c.LogPath)
		if err != nil {
			return err
		}
		if end > file.Offset || end < file.Offset || (file.Inode != 0 && (file.Inode != ino || file.Device != dev)) {
			cursor := UsageAckCursor{Namespace: file.Namespace, Offset: end}
			if err := c.Sink.AcceptGap(ctx, file.Namespace+":route-change-"+fmt.Sprint(file.Offset), "route_changed", time.Now().UTC(), cursor); err != nil {
				return err
			}
		}
		state.RouteFingerprint = c.RouteFingerprint
		state.Files[c.LogPath] = claudeFileCursor{Namespace: uuid.NewString(), Offset: end, Device: dev, Inode: ino}
		return saveClaudeCursor(c.CursorPath, state)
	}
	if info.Size() < file.Offset || (file.Inode != 0 && (file.Inode != ino || file.Device != dev)) {
		cursor := UsageAckCursor{Namespace: file.Namespace, Offset: file.Offset}
		if err := c.Sink.AcceptGap(ctx, file.Namespace+":truncated-"+fmt.Sprint(file.Offset), "truncated", time.Now().UTC(), cursor); err != nil {
			return err
		}
		file = claudeFileCursor{Namespace: uuid.NewString(), Device: dev, Inode: ino}
		state.Files[c.LogPath] = file
		if err := saveClaudeCursor(c.CursorPath, state); err != nil {
			return err
		}
	}
	if _, err := f.Seek(file.Offset, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		line, size, complete, large, err := readUsageLine(reader)
		if err != nil {
			return err
		}
		if !complete {
			return nil
		}
		cursor := UsageAckCursor{Namespace: file.Namespace, Offset: file.Offset + size}
		if large {
			if err := c.Sink.AcceptGap(ctx, file.Namespace+":oversized-"+fmt.Sprint(file.Offset), "malformed", time.Now().UTC(), cursor); err != nil {
				return err
			}
		} else {
			evidence, parseErr := ParseHermesUsageLine(strings.TrimRight(string(line), "\r\n"), file.Namespace, c.Location)
			if parseErr != nil {
				if err := c.Sink.AcceptGap(ctx, file.Namespace+":malformed-"+fmt.Sprint(file.Offset), "malformed", time.Now().UTC(), cursor); err != nil {
					return err
				}
			} else if evidence != nil && evidence.SourceIdentity != file.LastSourceIdentity {
				if err := c.Sink.AcceptUsage(ctx, *evidence, cursor); err != nil {
					return err
				}
				file.LastSourceIdentity = evidence.SourceIdentity
			}
		}
		file.Offset = cursor.Offset
		state.Files[c.LogPath] = file
		if err := saveClaudeCursor(c.CursorPath, state); err != nil {
			return err
		}
	}
}
