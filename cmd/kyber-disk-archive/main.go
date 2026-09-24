// Command kyber-disk-archive runs inside the short-lived pods Kyber creates
// for agent disk export (MAT-87) and restore (MAT-88). It never talks to the
// Kubernetes API: the control plane hands it one job's endpoint and a
// per-job token, and it can do nothing else with them.
//
//	kyber-disk-archive export --root /persist
//	    Walks the agent volume and streams a Kyber disk archive to
//	    $KYBER_ARCHIVE_UPLOAD_URL.
//
//	kyber-disk-archive restore --root /persist
//	    Reads the archive at $KYBER_ARCHIVE_SOURCE_URL by byte range,
//	    validates it, extracts it into the (empty) new volume, and re-scans
//	    the volume against the manifest. Exit 0 means the volume is complete.
//
//	kyber-disk-archive verify
//	    Reads the stored archive back from $KYBER_ARCHIVE_SOURCE_URL by byte
//	    range, checks every entry against the manifest, and posts the
//	    manifest summary to $KYBER_ARCHIVE_SUMMARY_URL. It runs in its own
//	    pod so the control plane never holds a large manifest in memory.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/diskarchive"
)

// terminationLog is where Kubernetes reads a container's exit message. It is
// the only diagnostic channel back to the control plane; messages name paths
// and causes, never file contents.
const terminationLog = "/dev/termination-log"

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("usage: kyber-disk-archive export|restore --root DIR | verify"))
	}
	mode := os.Args[1]
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	root := fs.String("root", "/persist", "volume root")
	_ = fs.Parse(os.Args[2:])

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	var err error
	switch mode {
	case "export":
		err = runExport(ctx, *root)
	case "verify":
		err = runVerify(ctx)
	case "restore":
		err = runRestore(ctx, *root)
	default:
		err = fmt.Errorf("unknown mode %q", mode)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	msg := err.Error()
	if len(msg) > 1024 {
		msg = msg[:1024]
	}
	_ = os.WriteFile(terminationLog, []byte(msg), 0o644)
	fmt.Fprintln(os.Stderr, "kyber-disk-archive:", msg)
	os.Exit(1)
}

func runExport(ctx context.Context, root string) error {
	url := os.Getenv("KYBER_ARCHIVE_UPLOAD_URL")
	token := os.Getenv("KYBER_ARCHIVE_TOKEN")
	if url == "" || token == "" {
		return errors.New("KYBER_ARCHIVE_UPLOAD_URL and KYBER_ARCHIVE_TOKEN are required")
	}
	desc, err := fetchDescription(ctx, os.Getenv("KYBER_ARCHIVE_DESCRIPTION_URL"), token)
	if err != nil {
		return err
	}
	image, err := loadImageManifest(root)
	if err != nil {
		return err
	}
	maxBytes, _ := strconv.ParseInt(os.Getenv("KYBER_ARCHIVE_MAX_BYTES"), 10, 64)
	maxEntries, _ := strconv.Atoi(os.Getenv("KYBER_ARCHIVE_MAX_ENTRIES"))

	pr, pw := io.Pipe()
	walkErr := make(chan error, 1)
	go func() {
		m, err := diskarchive.Write(ctx, root, pw, diskarchive.WriteOptions{
			Scan:   diskarchive.ScanOptions{Exclude: diskarchive.DefaultExclusions(), MaxBytes: maxBytes, MaxEntries: maxEntries},
			Source: desc.Source,
			Mounts: desc.Mounts,
			Image:  image,
		})
		if err == nil {
			fmt.Fprintf(os.Stderr, "kyber-disk-archive: archived %d entries, %d bytes\n", m.Totals.Entries, m.Totals.Bytes)
		}
		// Closing with the walk's error aborts the upload, so the control plane
		// never records a partial archive as complete.
		pw.CloseWithError(err)
		walkErr <- err
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/zip")
	resp, err := http.DefaultClient.Do(req)
	// The server may answer before reading everything (a rejection); closing
	// the read side unblocks the walker either way.
	pr.CloseWithError(errors.New("upload finished"))
	werr := <-walkErr
	if resp != nil {
		defer resp.Body.Close()
	}
	switch {
	case err != nil && werr != nil:
		// The walk's error (for example an unreadable file) is the cause; the
		// broken upload is its consequence.
		return fmt.Errorf("archiving: %w", werr)
	case err != nil:
		return fmt.Errorf("uploading: %w", err)
	case resp.StatusCode != http.StatusNoContent:
		return fmt.Errorf("upload rejected: HTTP %d", resp.StatusCode)
	case werr != nil:
		return fmt.Errorf("archiving: %w", werr)
	}
	return nil
}

type archiveDescription struct {
	Source diskarchive.Source  `json:"source"`
	Mounts []diskarchive.Mount `json:"mounts"`
}

// fetchDescription reads the manifest's source description from the job's
// endpoint. It carries the agent's configuration, which is too large for the
// environment.
func fetchDescription(ctx context.Context, url, token string) (archiveDescription, error) {
	var desc archiveDescription
	if url == "" {
		return desc, errors.New("KYBER_ARCHIVE_DESCRIPTION_URL is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return desc, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return desc, fmt.Errorf("reading archive description: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return desc, fmt.Errorf("reading archive description: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&desc); err != nil {
		return desc, fmt.Errorf("decoding archive description: %w", err)
	}
	return desc, nil
}

// loadImageManifest reads kyber-rootfs's record of the base image from the
// volume, keyed by archive path under the durable root. A volume without one
// (not rootfs persistence, or a root that predates it) returns nil, and
// classification falls back to paths alone.
func loadImageManifest(root string) (map[string]diskarchive.ImageFile, error) {
	f, err := os.Open(filepath.Join(root, diskarchive.ImageManifestPath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("reading the image manifest: %w", err)
	}
	defer f.Close()
	return diskarchive.ParseImageManifest(f, diskarchive.RootfsDir+"/")
}

// summaryListCap bounds the exclusion and credential lists in a summary; the
// full lists stay in the archive's manifest.
const summaryListCap = 1000

func runVerify(ctx context.Context) error {
	url := os.Getenv("KYBER_ARCHIVE_SOURCE_URL")
	summaryURL := os.Getenv("KYBER_ARCHIVE_SUMMARY_URL")
	token := os.Getenv("KYBER_ARCHIVE_TOKEN")
	if url == "" || summaryURL == "" || token == "" {
		return errors.New("KYBER_ARCHIVE_SOURCE_URL, KYBER_ARCHIVE_SUMMARY_URL and KYBER_ARCHIVE_TOKEN are required")
	}
	maxBytes, _ := strconv.ParseInt(os.Getenv("KYBER_ARCHIVE_MAX_BYTES"), 10, 64)
	maxEntries, _ := strconv.Atoi(os.Getenv("KYBER_ARCHIVE_MAX_ENTRIES"))
	ra, err := newHTTPReaderAt(ctx, url, token)
	if err != nil {
		return err
	}
	m, err := diskarchive.Verify(ra, ra.size, diskarchive.Limits{MaxBytes: maxBytes, MaxEntries: maxEntries})
	if err != nil {
		return err
	}
	sum := archivejob.SummaryOf(m)
	if n := len(sum.Excluded); n > summaryListCap {
		sum.Excluded = append(sum.Excluded[:summaryListCap], diskarchive.Exclusion{
			Path: fmt.Sprintf("… and %d more", n-summaryListCap), Reason: "see kyber-export/manifest.json in the archive"})
	}
	if n := len(sum.Cron); n > summaryListCap {
		sum.Cron = append(sum.Cron[:summaryListCap], fmt.Sprintf("… and %d more", n-summaryListCap))
	}
	if n := len(sum.Sensitive); n > summaryListCap {
		sum.Sensitive = append(sum.Sensitive[:summaryListCap], fmt.Sprintf("… and %d more", n-summaryListCap))
	}
	body, err := json.Marshal(sum)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, summaryURL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("reporting result: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("reporting result: HTTP %d", resp.StatusCode)
	}
	fmt.Fprintf(os.Stderr, "kyber-disk-archive: verified %d entries, %d bytes\n", m.Totals.Entries, m.Totals.Bytes)
	return nil
}

// httpReaderAt reads a remote archive by byte range. archive/zip reads the
// central directory at the end and then each entry in order, so a small
// cache of large blocks turns those reads into a few requests per block.
type httpReaderAt struct {
	ctx   context.Context
	url   string
	token string
	size  int64

	mu     sync.Mutex
	blocks map[int64][]byte
	order  []int64
}

const (
	readBlock  = 4 << 20
	readBlocks = 4
)

func newHTTPReaderAt(ctx context.Context, url, token string) (*httpReaderAt, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength < 0 {
		return nil, fmt.Errorf("opening archive: HTTP %d", resp.StatusCode)
	}
	return &httpReaderAt{ctx: ctx, url: url, token: token, size: resp.ContentLength, blocks: map[int64][]byte{}}, nil
}

func (h *httpReaderAt) block(i int64) ([]byte, error) {
	h.mu.Lock()
	if b, ok := h.blocks[i]; ok {
		h.mu.Unlock()
		return b, nil
	}
	h.mu.Unlock()
	start := i * readBlock
	end := min(start+readBlock, h.size) - 1
	req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reading archive: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, end-start+2))
	if err != nil {
		return nil, fmt.Errorf("reading archive: %w", err)
	}
	if int64(len(b)) != end-start+1 {
		return nil, fmt.Errorf("reading archive: short range (%d of %d bytes)", len(b), end-start+1)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.blocks[i] = b
	h.order = append(h.order, i)
	if len(h.order) > readBlocks {
		delete(h.blocks, h.order[0])
		h.order = h.order[1:]
	}
	return b, nil
}

func (h *httpReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= h.size {
			return n, io.EOF
		}
		b, err := h.block(pos / readBlock)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], b[pos%readBlock:])
	}
	return n, nil
}

func runRestore(ctx context.Context, root string) error {
	url := os.Getenv("KYBER_ARCHIVE_SOURCE_URL")
	token := os.Getenv("KYBER_ARCHIVE_TOKEN")
	if url == "" || token == "" {
		return errors.New("KYBER_ARCHIVE_SOURCE_URL and KYBER_ARCHIVE_TOKEN are required")
	}
	// What to leave out is decided here, against the full manifest, so no
	// credential file or crontab escapes the rule because a summary list
	// was truncated. Both default to leaving the files out.
	skipCredentials := os.Getenv("KYBER_ARCHIVE_SKIP_CREDENTIALS") != "false"
	skipCrontabs := os.Getenv("KYBER_ARCHIVE_SKIP_CRONTABS") != "false"
	// Harness login files are always left out, whatever the options say.
	var loginFiles []string
	if v := os.Getenv("KYBER_ARCHIVE_LOGIN_FILES"); v != "" {
		loginFiles = strings.Split(v, ",")
	}
	skip := map[string]bool{}
	ra, err := newHTTPReaderAt(ctx, url, token)
	if err != nil {
		return err
	}
	// The archive must fit the volume it is restored into.
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", root, err)
	}
	available := int64(st.Bavail) * int64(st.Bsize)
	maxBytes, _ := strconv.ParseInt(os.Getenv("KYBER_ARCHIVE_MAX_BYTES"), 10, 64)
	if maxBytes <= 0 || available < maxBytes {
		maxBytes = available
	}
	maxEntries, _ := strconv.Atoi(os.Getenv("KYBER_ARCHIVE_MAX_ENTRIES"))
	allowed := []string{"lost+found"}
	m, err := diskarchive.Extract(ctx, ra, ra.size, root, diskarchive.ExtractOptions{
		Limits:            diskarchive.Limits{MaxBytes: maxBytes, MaxEntries: maxEntries},
		PreserveOwnership: true,
		Allowed:           allowed,
		Skip:              skip,
		SkipIf: func(e diskarchive.Entry) bool {
			return e.Type == diskarchive.EntryFile && diskarchive.IsLoginPath(e.Path, loginFiles) ||
				skipCredentials && diskarchive.IsSensitive(e) || skipCrontabs && diskarchive.IsAgentCrontab(e)
		},
	})
	if err != nil {
		return fmt.Errorf("restoring: %w", err)
	}
	if err := diskarchive.VerifyRestore(ctx, root, m, allowed, skip); err != nil {
		return fmt.Errorf("verifying restored volume: %w", err)
	}
	fmt.Fprintf(os.Stderr, "kyber-disk-archive: restored %d entries (%d left out), %d bytes\n", m.Totals.Entries, len(skip), m.Totals.Bytes)
	return nil
}
