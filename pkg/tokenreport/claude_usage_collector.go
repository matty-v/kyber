package tokenreport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const maxUsageLine = 4 << 20

// UsageAckCursor is the position after one complete source record. The sink
// must acknowledge a durable control-plane write before Scan saves it locally.
type UsageAckCursor struct {
	Namespace string `json:"namespace"`
	Offset    int64  `json:"offset"`
}

type ClaudeUsageSink interface {
	AcceptUsage(context.Context, UsageEvidence, UsageAckCursor) error
	AcceptGap(context.Context, string, string, time.Time, UsageAckCursor) error
}

type ClaudeUsageCollector struct {
	ProjectsRoot     string
	CursorPath       string // must be on the Agent PVC
	RouteFingerprint string // injected from the pod's route configuration
	Sink             ClaudeUsageSink
}

type claudeFileCursor struct {
	Namespace          string         `json:"namespace"`
	Offset             int64          `json:"offset"`
	Device             uint64         `json:"device"`
	Inode              uint64         `json:"inode"`
	LastSourceIdentity string         `json:"lastSourceIdentity,omitempty"`
	CodexTotal         *UsageEvidence `json:"codexTotal,omitempty"`
	CodexModel         string         `json:"codexModel,omitempty"`
}

type claudeCursorState struct {
	RouteFingerprint string                      `json:"routeFingerprint"`
	ConfiguredModel  string                      `json:"configuredModel,omitempty"`
	Files            map[string]claudeFileCursor `json:"files"`
}

// Scan processes every complete line added since the last acknowledged
// cursor. On first deployment it starts at each existing file's complete EOF:
// old snapshot-derived totals are not backfilled as accurate usage events.
func (c *ClaudeUsageCollector) Scan(ctx context.Context) error {
	if c.Sink == nil || c.ProjectsRoot == "" || c.CursorPath == "" || c.RouteFingerprint == "" {
		return ErrIncompleteUsage
	}
	paths, err := claudeSessionFiles(c.ProjectsRoot)
	if err != nil {
		return err
	}
	state, err := readClaudeCursor(c.CursorPath)
	if errors.Is(err, fs.ErrNotExist) {
		state = claudeCursorState{RouteFingerprint: c.RouteFingerprint, Files: map[string]claudeFileCursor{}}
		for _, path := range paths {
			info, statErr := os.Stat(path)
			if statErr != nil {
				return fmt.Errorf("stating Claude session: %w", statErr)
			}
			dev, ino := fileIdentity(info)
			offset, offsetErr := completeEOF(path)
			if offsetErr != nil {
				return offsetErr
			}
			state.Files[path] = claudeFileCursor{Namespace: uuid.NewString(), Offset: offset, Device: dev, Inode: ino}
		}
		return saveClaudeCursor(c.CursorPath, state)
	}
	if err != nil {
		return err
	}
	if state.Files == nil {
		return ErrIncompleteUsage
	}
	if state.RouteFingerprint != c.RouteFingerprint {
		// A prior pod may have left unreported calls in its transcript. They
		// cannot be assigned to the new route, so acknowledge a gap and skip
		// them. Even a file not seen by the old collector may contain an old
		// route call. Files created after this boundary start from zero.
		// Give newly discovered files a durable namespace before publishing
		// any gap. A retry after a lost ACK must use the same gap identity.
		for _, path := range paths {
			if _, known := state.Files[path]; !known {
				state.Files[path] = claudeFileCursor{Namespace: uuid.NewString()}
				if err := saveClaudeCursor(c.CursorPath, state); err != nil {
					return err
				}
			}
		}
		for _, path := range paths {
			file := state.Files[path]
			offset, offsetErr := completeEOF(path)
			if offsetErr != nil {
				return offsetErr
			}
			info, statErr := os.Stat(path)
			if statErr != nil {
				return fmt.Errorf("stating Claude session: %w", statErr)
			}
			dev, ino := fileIdentity(info)
			if offset > file.Offset || (file.Inode != 0 && (offset < file.Offset || file.Inode != ino || file.Device != dev)) {
				cursor := UsageAckCursor{Namespace: file.Namespace, Offset: offset}
				if err := c.Sink.AcceptGap(ctx, file.Namespace+":route-change-"+fmt.Sprint(file.Offset), "route_changed", time.Now().UTC(), cursor); err != nil {
					return err
				}
			}
			state.Files[path] = claudeFileCursor{Namespace: uuid.NewString(), Offset: offset, Device: dev, Inode: ino}
		}
		// Commit the new route and all file positions together. If the
		// process stops after a gap ACK, replay uses the same old namespace
		// and gap key, so the control plane can deduplicate it.
		state.RouteFingerprint = c.RouteFingerprint
		return saveClaudeCursor(c.CursorPath, state)
	}
	for _, path := range paths {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return fmt.Errorf("stating Claude session: %w", statErr)
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

func (c *ClaudeUsageCollector) scanFile(ctx context.Context, path string, state *claudeCursorState, file claudeFileCursor) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening Claude session: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(file.Offset, io.SeekStart); err != nil {
		return fmt.Errorf("seeking Claude session: %w", err)
	}
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, n, complete, large, readErr := readUsageLine(r)
		if readErr != nil {
			return fmt.Errorf("reading Claude session: %w", readErr)
		}
		if !complete {
			return nil
		}
		next := file.Offset + n
		cursor := UsageAckCursor{Namespace: file.Namespace, Offset: next}
		if large {
			if err := c.Sink.AcceptGap(ctx, file.Namespace+":oversized-"+fmt.Sprint(file.Offset), "malformed", time.Now().UTC(), cursor); err != nil {
				return err
			}
		} else {
			evidence, parseErr := ParseClaudeUsageLine(line, file.Namespace)
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
		file.Offset = next
		state.Files[path] = file
		if err := saveClaudeCursor(c.CursorPath, *state); err != nil {
			return err
		}
	}
}

func claudeSessionFiles(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if d.Type().IsRegular() && filepath.Ext(path) == ".jsonl" {
			paths = append(paths, path)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("walking Claude sessions: %w", err)
	}
	sort.Strings(paths)
	return paths, nil
}

func fileIdentity(info os.FileInfo) (uint64, uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Dev), stat.Ino
	}
	return 0, 0
}

func readUsageLine(r *bufio.Reader) ([]byte, int64, bool, bool, error) {
	var line []byte
	var n int64
	large := false
	for {
		frag, err := r.ReadSlice('\n')
		n += int64(len(frag))
		if !large {
			if len(line)+len(frag) > maxUsageLine {
				large = true
				line = nil
			} else {
				line = append(line, frag...)
			}
		}
		switch {
		case err == nil:
			return line, n, true, large, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil, 0, false, false, nil
		default:
			return nil, 0, false, false, err
		}
	}
}

func completeEOF(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("opening Claude session: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stating Claude session: %w", err)
	}
	buf := make([]byte, 64*1024)
	for end := info.Size(); end > 0; {
		start := end - int64(len(buf))
		if start < 0 {
			start = 0
		}
		n, readErr := f.ReadAt(buf[:end-start], start)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return 0, fmt.Errorf("reading Claude session tail: %w", readErr)
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				return start + int64(i) + 1, nil
			}
		}
		end = start
	}
	return 0, nil
}

func readClaudeCursor(path string) (claudeCursorState, error) {
	f, err := os.Open(path)
	if err != nil {
		return claudeCursorState{}, err
	}
	defer f.Close()
	// Cursor files hold paths and offsets only. Treat an unexpectedly large
	// or corrupt file as unavailable rather than loading it without a bound.
	const maxCursorBytes = 8 << 20
	raw, err := io.ReadAll(io.LimitReader(f, maxCursorBytes+1))
	if err != nil {
		return claudeCursorState{}, fmt.Errorf("reading Claude usage cursor: %w", err)
	}
	if len(raw) > maxCursorBytes {
		return claudeCursorState{}, ErrIncompleteUsage
	}
	var state claudeCursorState
	if err := json.Unmarshal(raw, &state); err != nil {
		return claudeCursorState{}, fmt.Errorf("decoding Claude usage cursor: %w", err)
	}
	if state.RouteFingerprint == "" || state.Files == nil {
		return claudeCursorState{}, ErrIncompleteUsage
	}
	for _, file := range state.Files {
		if file.Namespace == "" || file.Offset < 0 {
			return claudeCursorState{}, ErrIncompleteUsage
		}
	}
	return state, nil
}

func saveClaudeCursor(path string, state claudeCursorState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating Claude usage cursor directory: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".usage-cursor-*")
	if err != nil {
		return fmt.Errorf("creating Claude usage cursor: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return fmt.Errorf("securing Claude usage cursor: %w", err)
	}
	if err := json.NewEncoder(f).Encode(state); err != nil {
		f.Close()
		return fmt.Errorf("writing Claude usage cursor: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("syncing Claude usage cursor: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing Claude usage cursor: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing Claude usage cursor: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("opening Claude usage cursor directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("syncing Claude usage cursor directory: %w", err)
	}
	return nil
}
