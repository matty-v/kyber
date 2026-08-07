// Command transcript-compact reclaims duplicate transcript objects (kyber#458
// Part B). The transcript-tailer used to re-ship a whole session on every
// sidecar restart, leaving overlapping cumulative NDJSON objects under each
// transcripts/<agent>/<date>/ prefix. This merges them to a deduped superset and
// (under --apply) deletes the redundant copies.
//
// DRY-RUN IS THE DEFAULT: with no flags it reports reclaimable bytes and changes
// nothing. --apply performs the destructive merge+delete and is gated on Matt's
// explicit go (it mutates durable audit data); run it only after the Part A
// source fix is deployed (so the duplicate set is no longer growing). See
// docs/operator/transcript-compaction.md.
//
// Backend + credentials come from the same KYBER_LOG_ARCHIVE_* env the control
// plane uses (same bucket; scope the creds to the log bucket / transcripts/).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	internalapi "github.com/matty-v/kyber/pkg/api"
)

func main() {
	apply := flag.Bool("apply", false, "perform the destructive merge+delete (default: dry-run, report only). Gated on Matt's go.")
	agent := flag.String("agent", "", "limit compaction to one agent's prefix (default: all agents under the lane)")
	flag.Parse()

	if err := run(*apply, *agent); err != nil {
		fmt.Fprintf(os.Stderr, "transcript-compact: %v\n", err)
		os.Exit(1)
	}
}

func run(apply bool, agent string) error {
	ctx := context.Background()
	store, err := buildStore(ctx)
	if err != nil {
		return err
	}
	if c, ok := store.(io.Closer); ok {
		defer c.Close()
	}

	mode := "DRY-RUN (no changes; pass --apply to merge+delete)"
	if apply {
		mode = "APPLY (merging + deleting redundant objects)"
	}
	fmt.Printf("transcript-compact: %s\n", mode)

	report, err := internalapi.CompactTranscripts(ctx, store, internalapi.CompactOptions{
		Agent: agent,
		Apply: apply,
	})
	if err != nil {
		return err
	}
	printReport(report)
	return nil
}

// buildStore selects the backend the same way the control plane does
// (KYBER_LOG_ARCHIVE_BACKEND gcs|s3, default gcs) and builds a write+delete-
// capable compaction store from the shared KYBER_LOG_ARCHIVE_* config.
func buildStore(ctx context.Context) (internalapi.CompactionStore, error) {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("KYBER_LOG_ARCHIVE_BACKEND")))
	if backend == "" {
		backend = "gcs"
	}
	bucket := os.Getenv("KYBER_LOG_ARCHIVE_BUCKET")

	switch backend {
	case "gcs":
		if bucket == "" {
			return nil, fmt.Errorf("KYBER_LOG_ARCHIVE_BUCKET unset (required for the gcs backend)")
		}
		return internalapi.NewGCSCompactionStore(ctx, bucket)
	case "s3":
		endpoint := os.Getenv("KYBER_LOG_ARCHIVE_ENDPOINT")
		region := os.Getenv("KYBER_LOG_ARCHIVE_REGION")
		accessKey := os.Getenv("KYBER_LOG_ARCHIVE_ACCESS_KEY")
		secretKey := os.Getenv("KYBER_LOG_ARCHIVE_SECRET_KEY")
		useTLS := !strings.EqualFold(os.Getenv("KYBER_LOG_ARCHIVE_USE_TLS"), "false")
		var missing []string
		if endpoint == "" {
			missing = append(missing, "KYBER_LOG_ARCHIVE_ENDPOINT")
		}
		if bucket == "" {
			missing = append(missing, "KYBER_LOG_ARCHIVE_BUCKET")
		}
		if accessKey == "" {
			missing = append(missing, "KYBER_LOG_ARCHIVE_ACCESS_KEY")
		}
		if secretKey == "" {
			missing = append(missing, "KYBER_LOG_ARCHIVE_SECRET_KEY")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("the s3 backend requires %s", strings.Join(missing, ", "))
		}
		return internalapi.NewS3CompactionStore(endpoint, bucket, region, accessKey, secretKey, useTLS)
	default:
		return nil, fmt.Errorf("KYBER_LOG_ARCHIVE_BACKEND=%q is not a recognized backend (want gcs|s3)", backend)
	}
}

func printReport(r internalapi.CompactionReport) {
	if len(r.Prefixes) == 0 {
		fmt.Println("No compactable prefixes found (no cross-object re-ship redundancy).")
		return
	}
	for _, p := range r.Prefixes {
		switch {
		case p.Skipped:
			fmt.Printf("  SKIP   %s — %s\n", p.Prefix, p.SkipReason)
		case p.Applied:
			note := ""
			if p.SkipReason != "" {
				note = " [" + p.SkipReason + "]"
			}
			fmt.Printf("  DONE   %s — %d objects → %d lines, reclaimed %s%s\n",
				p.Prefix, p.SourceObjects, p.MergedLines, humanBytes(p.ReclaimBytes), note)
		default:
			fmt.Printf("  WOULD  %s — %d objects → %d lines, reclaimable %s\n",
				p.Prefix, p.SourceObjects, p.MergedLines, humanBytes(p.ReclaimBytes))
		}
	}
	verb := "Reclaimable"
	if !r.DryRun {
		verb = "Reclaimed"
	}
	fmt.Printf("%s total: %s across %d prefix(es).\n", verb, humanBytes(r.TotalReclaim), len(r.Prefixes))
	if r.DryRun {
		fmt.Println("Dry-run only — nothing was changed. Re-run with --apply (gated on Matt's go) to reclaim.")
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
