package tokenreport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
)

// CodexUsageCollector derives one accepted delta from monotonic rollout totals.
// Repeated token_count observations have zero delta and do not create a call.
// The configured model is stored with the cursor: a pod that switches models
// cannot assign unread old-rollout records to its new configured model.
type CodexUsageCollector struct {
	SessionsRoot     string
	CursorPath       string
	RouteFingerprint string
	ConfiguredModel  string
	Sink             ClaudeUsageSink
}

func (c *CodexUsageCollector) Scan(ctx context.Context) error {
	if c.SessionsRoot == "" || c.CursorPath == "" || c.RouteFingerprint == "" || c.Sink == nil {
		return ErrIncompleteUsage
	}
	paths, err := claudeSessionFiles(c.SessionsRoot)
	if err != nil {
		return err
	}
	state, err := readClaudeCursor(c.CursorPath)
	if errors.Is(err, os.ErrNotExist) {
		state = claudeCursorState{RouteFingerprint: c.RouteFingerprint, ConfiguredModel: c.ConfiguredModel,
			Files: map[string]claudeFileCursor{}}
		for _, path := range paths {
			file, err := c.baseline(path)
			if err != nil {
				return err
			}
			state.Files[path] = file
		}
		return saveClaudeCursor(c.CursorPath, state)
	}
	if err != nil {
		return err
	}
	if state.RouteFingerprint != c.RouteFingerprint || state.ConfiguredModel != c.ConfiguredModel {
		// Preserve stable gap identities across a failed ACK by registering
		// unseen files before any gap is sent.
		for _, path := range paths {
			if _, known := state.Files[path]; !known {
				state.Files[path] = claudeFileCursor{Namespace: uuid.NewString()}
				if err := saveClaudeCursor(c.CursorPath, state); err != nil {
					return err
				}
			}
		}
		for _, path := range paths {
			old := state.Files[path]
			fresh, err := c.baseline(path)
			if err != nil {
				return err
			}
			if fresh.Offset != old.Offset || (old.Inode != 0 && (fresh.Inode != old.Inode || fresh.Device != old.Device)) {
				cursor := UsageAckCursor{Namespace: old.Namespace, Offset: fresh.Offset}
				if err := c.Sink.AcceptGap(ctx, old.Namespace+":route-or-model-change-"+fmt.Sprint(old.Offset), "route_changed", time.Now().UTC(), cursor); err != nil {
					return err
				}
			}
			state.Files[path] = fresh
		}
		state.RouteFingerprint, state.ConfiguredModel = c.RouteFingerprint, c.ConfiguredModel
		return saveClaudeCursor(c.CursorPath, state)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		dev, ino := fileIdentity(info)
		file, known := state.Files[path]
		if !known {
			file = claudeFileCursor{Namespace: uuid.NewString(), Device: dev, Inode: ino}
			state.Files[path] = file
			if err := saveClaudeCursor(c.CursorPath, state); err != nil {
				return err
			}
		} else if info.Size() < file.Offset || (file.Inode != 0 && (file.Inode != ino || file.Device != dev)) {
			cursor := UsageAckCursor{Namespace: file.Namespace, Offset: file.Offset}
			if err := c.Sink.AcceptGap(ctx, file.Namespace+":truncated-"+fmt.Sprint(file.Offset), "truncated", time.Now().UTC(), cursor); err != nil {
				return err
			}
			file = claudeFileCursor{Namespace: uuid.NewString(), Device: dev, Inode: ino}
			state.Files[path] = file
			if err := saveClaudeCursor(c.CursorPath, state); err != nil {
				return err
			}
		}
		if err := c.scanFile(ctx, path, &state, file); err != nil {
			return err
		}
	}
	return nil
}

func (c *CodexUsageCollector) baseline(path string) (claudeFileCursor, error) {
	info, err := os.Stat(path)
	if err != nil {
		return claudeFileCursor{}, err
	}
	dev, ino := fileIdentity(info)
	file := claudeFileCursor{Namespace: uuid.NewString(), Device: dev, Inode: ino}
	end, err := completeEOF(path)
	if err != nil {
		return file, err
	}
	f, err := os.Open(path)
	if err != nil {
		return file, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(io.LimitReader(f, end), 64*1024)
	for {
		line, size, complete, large, err := readUsageLine(r)
		if err != nil {
			return file, err
		}
		if !complete {
			break
		}
		file.Offset += size
		if !large {
			if model, isContext := codexModelLine(line); isContext {
				file.CodexModel = model
			}
			if observation, parseErr := ParseCodexUsageLine(line, codexModelOrPlaceholder(file.CodexModel, c.ConfiguredModel)); parseErr == nil && observation != nil {
				total := observation.Total
				file.CodexTotal = &total
			}
		}
	}
	file.Offset = end
	return file, nil
}

func (c *CodexUsageCollector) scanFile(ctx context.Context, path string, state *claudeCursorState, file claudeFileCursor) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(file.Offset, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, size, complete, large, err := readUsageLine(r)
		if err != nil {
			return err
		}
		if !complete {
			return nil
		}
		cursor := UsageAckCursor{Namespace: file.Namespace, Offset: file.Offset + size}
		switch {
		case large:
			if err := c.Sink.AcceptGap(ctx, file.Namespace+":oversized-"+fmt.Sprint(file.Offset), "malformed", time.Now().UTC(), cursor); err != nil {
				return err
			}
		default:
			if model, isContext := codexModelLine(line); isContext {
				file.CodexModel = model
			}
			observation, parseErr := ParseCodexUsageLine(line, codexModelOrPlaceholder(file.CodexModel, c.ConfiguredModel))
			if parseErr != nil {
				if err := c.Sink.AcceptGap(ctx, file.Namespace+":malformed-"+fmt.Sprint(file.Offset), "malformed", time.Now().UTC(), cursor); err != nil {
					return err
				}
			} else if observation != nil {
				evidence, positive, reset := codexUsageDelta(file.CodexTotal, observation.Total)
				if reset {
					if err := c.Sink.AcceptGap(ctx, file.Namespace+":counter-reset-"+fmt.Sprint(file.Offset), "cursor_gap", observation.ObservedAt, cursor); err != nil {
						return err
					}
				} else if positive && file.CodexModel == "" && c.ConfiguredModel == "" {
					if err := c.Sink.AcceptGap(ctx, file.Namespace+":model-unknown-"+fmt.Sprint(file.Offset), "missing_identity", observation.ObservedAt, cursor); err != nil {
						return err
					}
				} else if positive {
					evidence.Runtime = "codex"
					evidence.SourceIdentity = file.Namespace + ":" + fmt.Sprint(cursor.Offset)
					evidence.ObservedAt = observation.ObservedAt
					evidence.Model = c.ConfiguredModel
					evidence.ModelEvidence = "configured"
					if file.CodexModel != "" {
						evidence.Model = file.CodexModel
						evidence.ModelEvidence = "observed"
					}
					if err := c.Sink.AcceptUsage(ctx, evidence, cursor); err != nil {
						return err
					}
				}
				total := observation.Total
				file.CodexTotal = &total
			}
		}
		file.Offset = cursor.Offset
		state.Files[path] = file
		if err := saveClaudeCursor(c.CursorPath, *state); err != nil {
			return err
		}
	}
}

func codexModelOrPlaceholder(observed, configured string) string {
	if observed != "" {
		return observed
	}
	if configured != "" {
		return configured
	}
	return "unknown"
}

func codexModelLine(line []byte) (string, bool) {
	var envelope struct {
		Type    string `json:"type"`
		Payload struct {
			Model string `json:"model"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &envelope) != nil || envelope.Type != "turn_context" {
		return "", false
	}
	return envelope.Payload.Model, true
}

func codexUsageDelta(previous *UsageEvidence, current UsageEvidence) (UsageEvidence, bool, bool) {
	var result UsageEvidence
	positive, reset := false, false
	calc := func(old, next *int64) *int64 {
		if next == nil {
			return nil
		}
		if previous == nil {
			if *next > 0 {
				positive = true
			}
			return tokenCount(*next)
		}
		if old == nil {
			return nil
		}
		if *next < *old {
			reset = true
			return nil
		}
		if *next > *old {
			positive = true
		}
		return tokenCount(*next - *old)
	}
	var old UsageEvidence
	if previous != nil {
		old = *previous
	}
	result.InputUncached = calc(old.InputUncached, current.InputUncached)
	result.Output = calc(old.Output, current.Output)
	result.CacheRead = calc(old.CacheRead, current.CacheRead)
	result.CacheWrite = calc(old.CacheWrite, current.CacheWrite)
	return result, positive, reset
}
