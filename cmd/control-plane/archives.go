package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalapi "github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/archivestore"
)

// buildArchiveService assembles disk export/import (MAT-87/MAT-88) from the
// KYBER_ARCHIVE_* environment the chart renders. It always returns a
// service; an unusable configuration is reported through DisabledReason so
// /api/v1/config and the routes can say why.
//
// KYBER_ARCHIVE_STORE_BACKEND selects where sealed archives live:
//
//	builtin (default) – the chart's kyber-archive-store StatefulSet, at
//	                    KYBER_ARCHIVE_STORE_URL with KYBER_ARCHIVE_STORE_TOKEN
//	s3                – KYBER_ARCHIVE_S3_{ENDPOINT,BUCKET,REGION,ACCESS_KEY,
//	                    SECRET_KEY,USE_TLS}, each falling back to the matching
//	                    KYBER_LOG_ARCHIVE_* value
//	gcs               – KYBER_ARCHIVE_GCS_BUCKET (node ADC)
//	disabled          – feature off
func buildArchiveService(ctx context.Context, c client.Client, namespace string, jobs archivejob.Store,
	signingKey []byte, version string, recorder record.EventRecorder) *internalapi.ArchiveService {
	svc := &internalapi.ArchiveService{
		Client:          c,
		Namespace:       namespace,
		Jobs:            jobs,
		SigningKey:      signingKey,
		ToolImage:       os.Getenv("KYBER_ARCHIVE_TOOL_IMAGE"),
		InternalURL:     os.Getenv("KYBER_CONTROL_PLANE_INTERNAL_URL"),
		KyberVersion:    version,
		PersistenceMode: firstNonEmpty(strings.ToLower(os.Getenv("KYBER_AGENT_PERSISTENCE_MODE")), "rootfs"),
		Recorder:        recorder,
		ImagePullSecrets: func() []string {
			var out []string
			for _, n := range strings.Split(os.Getenv("KYBER_IMAGE_PULL_SECRETS"), ",") {
				if n = strings.TrimSpace(n); n != "" {
					out = append(out, n)
				}
			}
			return out
		}(),
		Limits: internalapi.ArchiveLimits{
			MaxArchiveBytes:   envInt64("KYBER_ARCHIVE_MAX_BYTES"),
			PauseTimeout:      envDuration("KYBER_ARCHIVE_PAUSE_TIMEOUT"),
			JobTimeout:        envDuration("KYBER_ARCHIVE_JOB_TIMEOUT"),
			Retention:         envDuration("KYBER_ARCHIVE_RETENTION"),
			MaxConcurrentJobs: int(envInt64("KYBER_ARCHIVE_MAX_CONCURRENT_JOBS")),
			MaxEntries:        int(envInt64("KYBER_ARCHIVE_MAX_ENTRIES")),
		},
	}
	switch {
	case jobs == nil:
		svc.DisabledReason = "disk archives need PostgreSQL (KYBER_POSTGRES_URL) to keep job state"
		return svc
	case len(signingKey) == 0:
		svc.DisabledReason = "disk archives need the internal signing key (KYBER_INTERNAL_SIGNING_KEY)"
		return svc
	case svc.ToolImage == "":
		svc.DisabledReason = "KYBER_ARCHIVE_TOOL_IMAGE is unset"
		return svc
	}
	backend := strings.ToLower(firstNonEmpty(os.Getenv("KYBER_ARCHIVE_STORE_BACKEND"), "builtin"))
	switch backend {
	case "disabled":
		svc.DisabledReason = "disk archives are disabled on this installation (agentArchives.enabled=false)"
	case "builtin":
		url, token := os.Getenv("KYBER_ARCHIVE_STORE_URL"), os.Getenv("KYBER_ARCHIVE_STORE_TOKEN")
		if url == "" || token == "" {
			svc.DisabledReason = "the built-in archive store needs KYBER_ARCHIVE_STORE_URL and KYBER_ARCHIVE_STORE_TOKEN"
			return svc
		}
		svc.Store = &archivestore.HTTPStore{BaseURL: url, Token: token}
	case "s3":
		archiveEnv := func(k string) string {
			return firstNonEmpty(os.Getenv("KYBER_ARCHIVE_S3_"+k), os.Getenv("KYBER_LOG_ARCHIVE_"+k))
		}
		store, err := archivestore.NewS3Store(archiveEnv("ENDPOINT"), archiveEnv("BUCKET"),
			firstNonEmpty(os.Getenv("KYBER_ARCHIVE_PREFIX"), "kyber-agent-archives/"),
			archiveEnv("REGION"), archiveEnv("ACCESS_KEY"), archiveEnv("SECRET_KEY"),
			!strings.EqualFold(archiveEnv("USE_TLS"), "false"))
		if err != nil {
			svc.DisabledReason = "the S3 archive store is misconfigured: " + err.Error()
			return svc
		}
		svc.Store = store
	case "gcs":
		store, err := archivestore.NewGCSStore(ctx, firstNonEmpty(os.Getenv("KYBER_ARCHIVE_GCS_BUCKET"), os.Getenv("KYBER_LOG_ARCHIVE_BUCKET")),
			firstNonEmpty(os.Getenv("KYBER_ARCHIVE_PREFIX"), "kyber-agent-archives/"))
		if err != nil {
			svc.DisabledReason = "the GCS archive store is misconfigured: " + err.Error()
			return svc
		}
		svc.Store = store
	default:
		svc.DisabledReason = "KYBER_ARCHIVE_STORE_BACKEND=" + backend + " is not one of builtin, s3, gcs, disabled"
	}
	return svc
}

func envInt64(k string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv(k)), 10, 64)
	return v
}

func envDuration(k string) time.Duration {
	d, _ := time.ParseDuration(strings.TrimSpace(os.Getenv(k)))
	return d
}
