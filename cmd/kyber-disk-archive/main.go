// Command kyber-disk-archive runs inside the short-lived pods Kyber creates
// for agent disk export (MAT-87) and restore (MAT-88). It never talks to the
// Kubernetes API: the control plane hands it one job's endpoint and a
// per-job token, and it can do nothing else with them.
//
//	kyber-disk-archive export --root /persist
//	    Walks the agent volume and streams a Kyber disk archive to
//	    $KYBER_ARCHIVE_UPLOAD_URL.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/matty-v/kyber/pkg/diskarchive"
)

// terminationLog is where Kubernetes reads a container's exit message. It is
// the only diagnostic channel back to the control plane; messages name paths
// and causes, never file contents.
const terminationLog = "/dev/termination-log"

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("usage: kyber-disk-archive export --root DIR"))
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
	var desc struct {
		Source diskarchive.Source  `json:"source"`
		Mounts []diskarchive.Mount `json:"mounts"`
	}
	if raw := os.Getenv("KYBER_ARCHIVE_DESCRIPTION"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &desc); err != nil {
			return fmt.Errorf("decoding archive description: %w", err)
		}
	}
	maxBytes, _ := strconv.ParseInt(os.Getenv("KYBER_ARCHIVE_MAX_BYTES"), 10, 64)

	pr, pw := io.Pipe()
	walkErr := make(chan error, 1)
	go func() {
		m, err := diskarchive.Write(ctx, root, pw, diskarchive.WriteOptions{
			Scan:   diskarchive.ScanOptions{Exclude: diskarchive.DefaultExclusions(), MaxBytes: maxBytes},
			Source: desc.Source,
			Mounts: desc.Mounts,
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
