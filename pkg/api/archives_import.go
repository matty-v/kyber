package api

// archives_import.go — create a new agent from a disk archive (MAT-88). An
// import restores a completed export or an operator upload into a fresh PVC
// for a new Agent that is held (kyber.io/archive-hold) until the restore has
// been verified. The source agent and its volume are never touched.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/archivestore"
	agentctrl "github.com/matty-v/kyber/pkg/controllers/agent"
	"github.com/matty-v/kyber/pkg/diskarchive"
)

// importBootTimeout bounds how long a restored agent may take to reach
// Running or NeedsAuth before the job reports that it did not boot.
const importBootTimeout = 15 * time.Minute

// importCreateGrace is how long after an import is queued the worker waits
// for the handler to record the agent it created, and tolerates the cached
// client not showing that agent yet, before treating it as missing.
const importCreateGrace = 2 * time.Minute

// --- uploads ---

// StartUpload stages an operator-supplied archive. The body is streamed and
// sealed into the store; verification runs in the worker.
func (a *ArchiveService) StartUpload(ctx context.Context, requestedBy string) (*archivejob.Job, error) {
	now := a.now()
	deadline := now.Add(a.limits().JobTimeout)
	dataKey, err := archivestore.NewDataKey()
	if err != nil {
		return nil, err
	}
	wrapped, err := archivestore.WrapKey(a.SigningKey, dataKey)
	if err != nil {
		return nil, err
	}
	j := &archivejob.Job{
		ID: archivejob.NewID(), Kind: archivejob.KindUpload, State: archivejob.StateRunning,
		Step: archivejob.StepReceiving, Message: "Receiving the archive",
		CreatedAt: now, UpdatedAt: now, StartedAt: &now, Deadline: &deadline,
		RequestedBy: requestedBy, WrappedKey: wrapped,
	}
	j.ObjectKey = "uploads/" + j.ID + "/archive.sealed"
	if err := a.Jobs.Create(ctx, j, a.limits().MaxConcurrentJobs); err != nil {
		return nil, err
	}
	return j, nil
}

func (a *ArchiveService) advanceUpload(ctx context.Context, j *archivejob.Job) error {
	switch {
	case j.State == archivejob.StateCompleted:
		return a.expire(ctx, j)
	case j.CancelRequested:
		return a.finish(ctx, j, archivejob.StateCanceled, "canceled by operator")
	case j.Deadline != nil && a.now().After(*j.Deadline):
		return a.fail(ctx, j, "upload exceeded its time limit")
	case a.uploadIdle(j):
		return a.fail(ctx, j, "upload stopped: no part arrived for "+uploadPartIdleTimeout.String())
	case j.Step == archivejob.StepAssembling:
		return a.assembleUpload(ctx, j)
	case j.Step == archivejob.StepVerifying:
		return a.verifyStored(ctx, j, "Ready to import")
	}
	return nil // still receiving; the request handlers own this step
}

// --- imports ---

type agentImportSource struct {
	ExportID string `json:"exportId,omitempty"`
	UploadID string `json:"uploadId,omitempty"`
}

type agentImportRequest struct {
	Source agentImportSource  `json:"source"`
	Agent  CreateAgentRequest `json:"agent"`
	// KeepCredentialFiles restores files that look like credentials instead
	// of leaving them out. Off by default: a restored copy must not act as the
	// source with the source's logins.
	KeepCredentialFiles bool `json:"keepCredentialFiles,omitempty"`
	// KeepCrontabs restores crontabs the agent installed itself. Off by
	// default, so the same work does not run on both agents.
	KeepCrontabs bool `json:"keepCrontabs,omitempty"`
}

// importSource resolves and checks the archive an import reads.
func (a *ArchiveService) importSource(ctx context.Context, src agentImportSource) (*archivejob.Job, error) {
	id, kind := src.ExportID, archivejob.KindExport
	if src.UploadID != "" {
		id, kind = src.UploadID, archivejob.KindUpload
	}
	if id == "" || src.ExportID != "" && src.UploadID != "" {
		return nil, errors.New("source must name exactly one of exportId or uploadId")
	}
	j, err := a.Jobs.Get(ctx, id)
	if err != nil || j.Kind != kind {
		return nil, fmt.Errorf("%s %q not found", kind, id)
	}
	if j.State != archivejob.StateCompleted || j.Summary == nil {
		return nil, fmt.Errorf("%s %q is not a completed, verified archive", kind, id)
	}
	if j.ExpiresAt != nil && !a.now().Before(*j.ExpiresAt) {
		return nil, fmt.Errorf("%s %q has expired", kind, id)
	}
	if err := diskarchive.CheckVersion(j.Summary.FormatVersion); err != nil {
		return nil, err
	}
	return j, nil
}

// requiredDiskBytes is the smallest volume that fits an archive: its file
// bytes plus room for filesystem metadata and the agent's first writes.
func requiredDiskBytes(s *archivejob.Summary) int64 {
	b := s.Totals.Bytes
	return b + b/10 + 1<<30
}

// cutoverChecklist lists what the operator must reconcile by hand while the
// source and the new agent coexist. Nothing on it is activated automatically.
func cutoverChecklist(src *archivejob.Job, req agentImportRequest) []string {
	var out []string
	cfg := src.Summary.Source.Config
	sourceName := src.Summary.Source.Agent
	if jobs, ok := cfg["jobs"].([]any); ok && len(jobs) > 0 {
		names := make([]string, 0, len(jobs))
		for _, j := range jobs {
			if m, ok := j.(map[string]any); ok {
				names = append(names, fmt.Sprint(m["name"]))
			}
		}
		out = append(out, fmt.Sprintf("Scheduled jobs on %s were not copied (%s). Add them to the new agent once %s no longer runs them.",
			sourceName, strings.Join(names, ", "), sourceName))
	}
	if ch, ok := cfg["channels"].(map[string]any); ok {
		for _, name := range []string{"telegram", "slack", "discord"} {
			if on, _ := ch[name].(bool); on {
				out = append(out, fmt.Sprintf("%s had %s enabled. Enable it on the new agent only after disabling it on %s, or both agents will answer.",
					sourceName, channelTitle[name], sourceName))
			}
		}
	}
	if b, ok := cfg["inboundBindings"].([]any); ok && len(b) > 0 {
		out = append(out, fmt.Sprintf("%s has %d inbound webhook binding(s). They were not copied; move the sender to the new agent's URL when you cut over.", sourceName, len(b)))
	}
	if repo, _ := cfg["identityRepo"].(string); repo != "" {
		note := "The identity repo checkout from " + repo + " is restored on disk."
		if req.Agent.IdentityRepo.Repo == repo {
			note += " Both agents now use it; stop one before either pushes."
		}
		out = append(out, note)
	}
	if n := len(src.Summary.Cron); n > 0 {
		if req.KeepCrontabs {
			out = append(out, fmt.Sprintf("%d crontab(s) %s installed itself were restored and will run on both agents: %s. Remove them from one.",
				n, sourceName, strings.Join(src.Summary.Cron, ", ")))
		} else {
			out = append(out, fmt.Sprintf("%d crontab(s) %s installed itself were not restored (%s). Reinstall them once %s no longer runs them.",
				n, sourceName, strings.Join(src.Summary.Cron, ", "), sourceName))
		}
	}
	if n := len(src.Summary.Sensitive); n > 0 {
		if req.KeepCredentialFiles {
			out = append(out, "Files that look like credentials were restored as they were: "+strings.Join(src.Summary.Sensitive, ", ")+". Both agents now hold them; rotate any they should not share.")
		} else {
			out = append(out, "Files that look like credentials were not restored ("+strings.Join(src.Summary.Sensitive, ", ")+"). Authorize the new agent and give it its own credentials.")
		}
	}
	if src.Summary.Source.Runtime != "" && src.Summary.Source.Runtime != req.Agent.Runtime {
		out = append(out, fmt.Sprintf("The archive came from a %s agent and the new agent runs %s. Its sessions will not resume.", src.Summary.Source.Runtime, req.Agent.Runtime))
	}
	return out
}

// startImport validates an import, creates the held Agent through the normal
// create path (so every create-time check applies unchanged), and queues the
// restore. It writes the HTTP response itself.
func (s *Server) startImport(w http.ResponseWriter, r *http.Request) {
	a := s.Archives
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req agentImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	name := req.Agent.Name
	if !isValidName(name) {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "agent.name must be lowercase alphanumeric + hyphens, 1-63 chars", "agent.name")
		return
	}
	caller := callerFrom(r.Context())
	if caller == nil || !caller.AgentResources.Has(s.Namespace+"/"+name) {
		writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}
	src, err := a.importSource(r.Context(), req.Source)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, diskarchive.ErrUnsupportedVersion) {
			code = http.StatusUnprocessableEntity
		}
		writeJSONErrorWithField(w, code, "invalid_source", err.Error(), "source")
		return
	}
	// A caller may import an export only if it may read the exported agent;
	// an upload is not tied to an agent and needs an installation-wide grant.
	if src.Kind == archivejob.KindUpload && !caller.AgentResources.all ||
		src.Kind == archivejob.KindExport && !caller.AgentResources.Has(s.Namespace+"/"+src.Agent) {
		writeJSONErrorWithField(w, http.StatusNotFound, "invalid_source", "source not found", "source")
		return
	}

	// Default the destination from the archive where the request leaves it
	// open; everything else goes through the ordinary create validation.
	if req.Agent.Runtime == "" {
		req.Agent.Runtime = src.Summary.Source.Runtime
	}
	need := requiredDiskBytes(src.Summary)
	if req.Agent.Resources.Disk == "" {
		// The source's own disk size where it is known, so the copy keeps the
		// room the source had rather than starting nearly full.
		size := max(roundUpGi(need), roundUpGi(src.Summary.Source.Disk.RequestBytes))
		req.Agent.Resources.Disk = resource.NewQuantity(size, resource.BinarySI).String()
	}
	disk, err := resource.ParseQuantity(req.Agent.Resources.Disk)
	if err != nil {
		writeJSONErrorWithField(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid disk quantity", "agent.resources.disk")
		return
	}
	if disk.Value() < need {
		writeJSONErrorWithField(w, http.StatusBadRequest, "insufficient_capacity",
			fmt.Sprintf("the archive needs a disk of at least %s", resource.NewQuantity(roundUpGi(need), resource.BinarySI).String()), "agent.resources.disk")
		return
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := s.K8sClient.Get(r.Context(), types.NamespacedName{Name: agentctrl.PVCName(name), Namespace: s.Namespace}, pvc); err == nil {
		writeJSONError(w, http.StatusConflict, "conflict", "a volume named "+agentctrl.PVCName(name)+" already exists; an import never reuses one")
		return
	} else if !k8serrors.IsNotFound(err) {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to check the destination volume")
		return
	}

	if mode := src.Summary.Source.PersistenceMode; mode != "" && a.PersistenceMode != "" && mode != a.PersistenceMode {
		writeJSONErrorWithField(w, http.StatusUnprocessableEntity, "incompatible_archive",
			fmt.Sprintf("the archive's disk uses %s persistence and this installation uses %s", mode, a.PersistenceMode), "source")
		return
	}

	// What the restore will leave out. The restore pod applies the same rule
	// to the full manifest; this list (from the summary) is for display.
	var skipped []string
	for _, p := range src.Summary.Sensitive {
		if !req.KeepCredentialFiles {
			skipped = append(skipped, p)
		}
	}
	for _, p := range src.Summary.Cron {
		if !req.KeepCrontabs {
			skipped = append(skipped, p)
		}
	}

	// Reserve the job before creating anything, so capacity and "one import
	// per name" are refused with nothing to undo, and so a crash after the
	// create leaves a job that finds and cleans up the agent.
	now := a.now()
	deadline := now.Add(a.limits().JobTimeout)
	j := &archivejob.Job{
		ID: archivejob.NewID(), Kind: archivejob.KindImport, Agent: name, State: archivejob.StateQueued,
		Message: "Creating the agent", CreatedAt: now, UpdatedAt: now, Deadline: &deadline,
		RequestedBy: caller.Name, SourceJobID: src.ID, Machine: req.Agent.Machine,
		Skip: skipped, Summary: src.Summary, HoldApplied: true, BytesTotal: src.PlainSize,
		KeepCredentialFiles: req.KeepCredentialFiles, KeepCrontabs: req.KeepCrontabs,
		Cutover: cutoverChecklist(src, req),
	}
	if err := a.Jobs.Create(r.Context(), j, a.limits().MaxConcurrentJobs); err != nil {
		switch {
		case errors.Is(err, archivejob.ErrConcurrency):
			w.Header().Set("Retry-After", "60")
			writeJSONError(w, http.StatusTooManyRequests, "archive_capacity", "the installation is already running its maximum number of archive jobs")
		case errors.Is(err, archivejob.ErrActiveJob):
			writeJSONError(w, http.StatusConflict, "import_in_progress", "an import into this name is already running")
		default:
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to queue the restore")
		}
		return
	}
	// abandon fails the job and removes whatever the create made, using the
	// UID and Secrets recorded in this handler even if saving them failed.
	abandon := func(reason string) {
		ctx := context.WithoutCancel(r.Context())
		cur, err := a.Jobs.Get(ctx, j.ID)
		if err != nil {
			cur = j
		}
		cur.AgentUID, cur.CreatedSecrets = j.AgentUID, j.CreatedSecrets
		if err := a.finish(ctx, cur, archivejob.StateFailed, reason); err != nil {
			slog.Warn("abandoning import failed; the worker will retry", "job", j.ID, "error", err)
		}
	}

	// Create the Agent through the normal handler, held from birth.
	hold := &importHold{jobID: j.ID}
	body, _ := json.Marshal(req.Agent)
	sub := r.Clone(withArchiveHold(r.Context(), hold))
	sub.Body = io.NopCloser(strings.NewReader(string(body)))
	sub.ContentLength = int64(len(body))
	rec := &captureWriter{header: http.Header{}}
	s.createAgent(rec, sub)
	if rec.status != http.StatusCreated {
		abandon("the agent could not be created")
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.status)
		_, _ = io.WriteString(w, rec.body.String())
		return
	}

	// Record what was created. The UID and Secret names come from the create
	// itself, not a read-back through the (possibly stale) cache.
	uid, secrets := string(hold.uid), hold.secrets
	j.AgentUID, j.CreatedSecrets, j.Message = uid, secrets, "Queued"
	err = a.Jobs.Update(r.Context(), j)
	// The worker may have adopted this agent by its hold in the meantime (it
	// cannot tell a slow handler from one that stopped). That is the same
	// agent, so record onto the current row instead of discarding a good
	// create over a version conflict.
	for attempt := 0; errors.Is(err, archivejob.ErrConflict) && attempt < 5; attempt++ {
		cur, gerr := a.Jobs.Get(r.Context(), j.ID)
		if gerr != nil || cur.State.Terminal() || cur.Finishing != "" || cur.CancelRequested || cur.AgentUID != "" && cur.AgentUID != uid {
			break
		}
		cur.AgentUID, cur.CreatedSecrets = uid, secrets
		if cur.State == archivejob.StateQueued {
			cur.Message = "Queued"
		}
		j = cur
		err = a.Jobs.Update(r.Context(), j)
	}
	if err != nil {
		j.AgentUID, j.CreatedSecrets = uid, secrets
		abandon("recording the new agent failed")
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to queue the restore; the new agent was removed")
		return
	}
	var created AgentResponse
	_ = json.Unmarshal([]byte(rec.body.String()), &created)
	writeJSON(w, http.StatusAccepted, map[string]any{"import": viewArchiveJob(j), "agent": created})
}

var channelTitle = map[string]string{"telegram": "Telegram", "slack": "Slack", "discord": "Discord"}

func roundUpGi(b int64) int64 {
	const gi = 1 << 30
	return (b + gi - 1) / gi * gi
}

type archiveHoldKey struct{}

// importHold marks a create request as an import, so createAgent births the
// Agent with the hold annotation and the controller never boots it from an
// empty or half-restored volume. createAgent fills in what it created.
type importHold struct {
	jobID   string
	uid     types.UID
	secrets []string
}

func withArchiveHold(ctx context.Context, h *importHold) context.Context {
	return context.WithValue(ctx, archiveHoldKey{}, h)
}

func archiveHoldFrom(ctx context.Context) *importHold {
	h, _ := ctx.Value(archiveHoldKey{}).(*importHold)
	return h
}

// captureWriter records a handler's response so startImport can reuse
// createAgent and relay its validation errors verbatim.
type captureWriter struct {
	header http.Header
	status int
	body   strings.Builder
}

func (c *captureWriter) Header() http.Header { return c.header }
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.WriteString(string(b))
}
func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

// advanceImport drives a restore: volume, restore pod, verification in the
// pod, release, and the new agent's first boot.
func (a *ArchiveService) advanceImport(ctx context.Context, j *archivejob.Job) error {
	if j.CancelRequested && !j.Restored {
		return a.finish(ctx, j, archivejob.StateCanceled, "canceled by operator")
	}
	if j.Deadline != nil && a.now().After(*j.Deadline) {
		if j.Restored {
			return a.fail(ctx, j, "the volume was restored but the agent did not boot within the job's time limit; it has been kept for inspection")
		}
		return a.fail(ctx, j, "restore exceeded its time limit")
	}
	agent := &kyberv1.Agent{}
	err := a.Client.Get(ctx, types.NamespacedName{Name: j.Agent, Namespace: a.Namespace}, agent)
	if err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("reading agent: %w", err)
	}
	if j.AgentUID == "" {
		// The control plane stopped between creating the agent and recording
		// it. The hold annotation, which carries this job's ID, identifies it.
		if err == nil && agent.Annotations[kyberv1.AnnotationArchiveHold] == j.ID {
			j.AgentUID = string(agent.UID)
			// Recover the Secrets the create made, so abandoning this import
			// still removes them. The name was free when the job reserved it,
			// so every Secret the API labels for it came from this create.
			secrets, err := a.createdSecretsFor(ctx, j.Agent)
			if err != nil {
				return err
			}
			j.CreatedSecrets = secrets
			return a.Jobs.Update(ctx, j)
		}
		if a.now().Sub(j.CreatedAt) > importCreateGrace {
			return a.fail(ctx, j, "the new agent was never created")
		}
		return nil
	}
	if k8serrors.IsNotFound(err) || string(agent.UID) != j.AgentUID {
		// Reads come from the informer cache, which can trail the create by
		// a moment. Failing on that would discard the new agent's Secrets
		// while the cache still hides the agent itself, stranding it held.
		if a.now().Sub(j.CreatedAt) < importCreateGrace {
			return nil
		}
		return a.fail(ctx, j, "the new agent was deleted during the restore")
	}
	switch {
	case j.State == archivejob.StateQueued:
		return a.beginRestore(ctx, j, agent)
	case j.Step == archivejob.StepRestoring:
		return a.watchRestorePod(ctx, j, agent)
	case j.Step == archivejob.StepStarting:
		return a.watchFirstBoot(ctx, j, agent)
	}
	return a.fail(ctx, j, "restore is in an unknown step "+string(j.Step))
}

func (a *ArchiveService) beginRestore(ctx context.Context, j *archivejob.Job, agent *kyberv1.Agent) error {
	src, err := a.Jobs.Get(ctx, j.SourceJobID)
	if err != nil || src.State != archivejob.StateCompleted {
		return a.fail(ctx, j, "the source archive is no longer available")
	}
	node, err := a.agentNode(ctx, agent)
	if err != nil {
		// A machine still starting up is not a failure yet; the deadline is.
		return nil
	}
	pvc := agentctrl.BuildPVC(agent, a.AgentStorageClass)
	pvc.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(agent, kyberv1.GroupVersion.WithKind("Agent"))}
	if err := a.Client.Create(ctx, pvc); err != nil {
		existing := &corev1.PersistentVolumeClaim{}
		if k8serrors.IsAlreadyExists(err) && a.Client.Get(ctx, client.ObjectKeyFromObject(pvc), existing) == nil && metav1.IsControlledBy(existing, agent) {
			// Our own claim from an interrupted earlier attempt.
		} else {
			return a.fail(ctx, j, "creating the new agent's volume: "+err.Error())
		}
	}
	now := a.now()
	j.State = archivejob.StateRunning
	j.Step = archivejob.StepRestoring
	j.StartedAt = &now
	j.NodeName = node
	j.Message = "Restoring the disk"
	if err := a.Jobs.Update(ctx, j); err != nil {
		return err
	}
	a.event(j, corev1.EventTypeNormal, "DiskRestoreStarted", "restoring from "+string(src.Kind)+" "+src.ID)
	return a.createRestorePod(ctx, j, agent)
}

// createRestorePod issues the job's token and starts the restore pod. It is
// safe to repeat: a restart before the pod existed simply runs it again, and
// the pod refuses a volume that is not empty.
func (a *ArchiveService) createRestorePod(ctx context.Context, j *archivejob.Job, agent *kyberv1.Agent) error {
	token, err := a.issuePodToken(ctx, j, agent)
	if err != nil {
		return err
	}
	j.TokenHash = hashToken(token)
	if err := a.Jobs.Update(ctx, j); err != nil {
		return err
	}
	env := []corev1.EnvVar{
		{Name: "KYBER_ARCHIVE_SOURCE_URL", Value: strings.TrimRight(a.InternalURL, "/") + "/internal/archive-jobs/" + j.ID + "/archive"},
		{Name: "KYBER_ARCHIVE_SKIP_CREDENTIALS", Value: strconv.FormatBool(!j.KeepCredentialFiles)},
		{Name: "KYBER_ARCHIVE_SKIP_CRONTABS", Value: strconv.FormatBool(!j.KeepCrontabs)},
		{Name: "KYBER_ARCHIVE_MAX_BYTES", Value: fmt.Sprint(a.limits().MaxArchiveBytes)},
		{Name: "KYBER_ARCHIVE_MAX_ENTRIES", Value: fmt.Sprint(a.limits().MaxEntries)},
	}
	// Restoring ownership, modes and setgid bits on files the pod did not
	// create needs these, and nothing else.
	caps := []corev1.Capability{"CHOWN", "FOWNER", "DAC_OVERRIDE", "FSETID"}
	pod := a.archivePod(j, agent, j.NodeName, "restore", env, false, caps)
	if err := a.Client.Create(ctx, pod); err != nil && !k8serrors.IsAlreadyExists(err) {
		return a.fail(ctx, j, "creating restore pod: "+err.Error())
	}
	j.RestoreStarted = true
	return a.Jobs.Update(ctx, j)
}

func (a *ArchiveService) watchRestorePod(ctx context.Context, j *archivejob.Job, agent *kyberv1.Agent) error {
	pod := &corev1.Pod{}
	err := a.Client.Get(ctx, types.NamespacedName{Name: archivePodName(j), Namespace: a.Namespace}, pod)
	if k8serrors.IsNotFound(err) {
		if !j.RestoreStarted {
			return a.createRestorePod(ctx, j, agent)
		}
		if j.UpdatedAt.Add(2 * time.Minute).Before(a.now()) {
			return a.fail(ctx, j, "restore pod disappeared before it finished")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading restore pod: %w", err)
	}
	switch pod.Status.Phase {
	case corev1.PodFailed:
		return a.fail(ctx, j, "restore pod failed: "+archivePodDiagnostic(pod))
	case corev1.PodSucceeded:
	default:
		if stuck, why := a.archivePodStuck(pod); stuck {
			return a.fail(ctx, j, "restore pod could not start: "+why)
		}
		return nil
	}
	// The pod re-scanned the volume against the manifest before exiting 0.
	// Record that first: from here on the agent is whole and is never
	// discarded. Releasing it happens in the next step, idempotently.
	j.Restored = true
	j.Step = archivejob.StepStarting
	j.Message = "Starting the new agent"
	j.BytesDone = j.BytesTotal
	if err := a.Jobs.Update(ctx, j); err != nil {
		return err
	}
	a.event(j, corev1.EventTypeNormal, "DiskRestoreVerified", "volume restored and verified; starting the agent")
	return nil
}

// watchFirstBoot releases the verified agent and completes the job once it
// has passed its normal startup checks. Running and NeedsAuth both count:
// NeedsAuth is expected when credentials were not restored.
func (a *ArchiveService) watchFirstBoot(ctx context.Context, j *archivejob.Job, agent *kyberv1.Agent) error {
	if j.HoldApplied {
		if err := a.cleanupPod(ctx, j); err != nil {
			return err
		}
		if err := a.releaseAgent(ctx, j); err != nil {
			return err
		}
		j.WrappedKey = nil
		return a.Jobs.Update(ctx, j)
	}
	switch agent.Status.Phase {
	case kyberv1.AgentPhaseRunning, kyberv1.AgentPhaseNeedsAuth:
		now := a.now()
		j.State = archivejob.StateCompleted
		j.Step = ""
		j.Message = "Restored; agent is " + string(agent.Status.Phase)
		j.CompletedAt = &now
		a.event(j, corev1.EventTypeNormal, "DiskRestoreCompleted", "agent reached "+string(agent.Status.Phase))
		return a.Jobs.Update(ctx, j)
	case kyberv1.AgentPhaseFailed, kyberv1.AgentPhaseBrokenRuntime, kyberv1.AgentPhaseMemoryExhausted, kyberv1.AgentPhaseDiskExhausted:
		return a.fail(ctx, j, "the volume was restored but the agent did not boot (phase "+string(agent.Status.Phase)+"); it has been kept for inspection")
	}
	if j.UpdatedAt.Add(importBootTimeout).Before(a.now()) {
		return a.fail(ctx, j, "the volume was restored but the agent did not boot within 15 minutes; it has been kept for inspection")
	}
	return nil
}

// createdSecretsFor lists the Secrets the create path labels for an agent.
func (a *ArchiveService) createdSecretsFor(ctx context.Context, agent string) ([]string, error) {
	var list corev1.SecretList
	if err := a.Client.List(ctx, &list, client.InNamespace(a.Namespace), client.MatchingLabels{
		"app.kubernetes.io/managed-by": "kyber-api",
		"kyber.io/agent":               agent,
	}); err != nil {
		return nil, fmt.Errorf("listing the new agent's secrets: %w", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, s := range list.Items {
		names = append(names, s.Name)
	}
	return names, nil
}

// discardImportDestination removes the new Agent, its volume and the Secrets
// the create made, unless the restore already completed, in which case the
// agent is whole and kept (only its hold is released). The source is never
// touched.
func (a *ArchiveService) discardImportDestination(ctx context.Context, j *archivejob.Job) error {
	if j.Kind != archivejob.KindImport || j.Agent == "" {
		return nil
	}
	if j.Restored {
		return a.releaseAgent(ctx, j)
	}
	if j.AgentUID == "" {
		return nil // nothing was created
	}
	agent := &kyberv1.Agent{}
	err := a.Client.Get(ctx, types.NamespacedName{Name: j.Agent, Namespace: a.Namespace}, agent)
	if err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("reading agent: %w", err)
	}
	if err == nil && string(agent.UID) == j.AgentUID {
		if err := a.Client.Delete(ctx, agent); err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Errorf("deleting partially restored agent: %w", err)
		}
		// The finalizer deletes the volume too; deleting it here as well
		// means a retry never finds a half-restored claim in its way.
		pvc := &corev1.PersistentVolumeClaim{}
		if err := a.Client.Get(ctx, types.NamespacedName{Name: agentctrl.PVCName(j.Agent), Namespace: a.Namespace}, pvc); err == nil && metav1.IsControlledBy(pvc, agent) {
			if err := a.Client.Delete(ctx, pvc); err != nil && !k8serrors.IsNotFound(err) {
				return fmt.Errorf("deleting partially restored volume: %w", err)
			}
		}
	}
	// The create's Secrets have no owner, and a just-created agent may be
	// deleted before the controller ever added the finalizer that sweeps them.
	for _, name := range j.CreatedSecrets {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.Namespace}}
		if err := a.Client.Delete(ctx, sec); err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Errorf("deleting the new agent's secret %s: %w", name, err)
		}
	}
	j.HoldApplied = false
	return nil
}

// serveRestoreReads answers the restore pod's ranged reads of the source
// archive's plaintext.
func (a *ArchiveService) serveRestoreReads(w http.ResponseWriter, r *http.Request, j *archivejob.Job) {
	src, err := a.Jobs.Get(r.Context(), j.SourceJobID)
	if err != nil || src.State != archivejob.StateCompleted {
		http.Error(w, "source unavailable", http.StatusGone)
		return
	}
	a.serveArchiveReads(w, r, src)
}

// referencedSources returns the archives active imports are reading, which
// retention must not delete underneath them.
func referencedSources(active []*archivejob.Job) map[string]bool {
	out := map[string]bool{}
	for _, j := range active {
		if j.Kind == archivejob.KindImport && !j.State.Terminal() {
			out[j.SourceJobID] = true
		}
	}
	return out
}

// --- routes ---

// handleArchiveUploads serves /api/v1/archive-uploads[/{id}].
func (s *Server) handleArchiveUploads(w http.ResponseWriter, r *http.Request) {
	if !s.requireInstallationArchiveAccess(w, r) {
		return
	}
	a := s.Archives
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/v1/archive-uploads"), "/")
	id, action, _ := strings.Cut(rest, "/")
	switch {
	case id == "" && r.Method == http.MethodPost && strings.HasPrefix(r.Header.Get("Content-Type"), "application/json"):
		s.startPartUpload(w, r)
	case id == "" && r.Method == http.MethodPost:
		s.receiveUpload(w, r)
	case id != "" && strings.HasPrefix(action, "parts/") && r.Method == http.MethodPut:
		s.receivePart(w, r, id, strings.TrimPrefix(action, "parts/"))
	case id != "" && action == "complete" && r.Method == http.MethodPost:
		s.completeUpload(w, r, id)
	case id != "" && action == "" && r.Method == http.MethodGet:
		j, err := a.Jobs.Get(r.Context(), id)
		if err != nil || j.Kind != archivejob.KindUpload {
			writeJSONError(w, http.StatusNotFound, "not_found", "upload not found")
			return
		}
		writeJSON(w, http.StatusOK, viewArchiveJob(j))
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

// receiveUpload streams a ZIP body into the store. The public server's short
// read timeout is lifted for this request and bounded by the job deadline.
func (s *Server) receiveUpload(w http.ResponseWriter, r *http.Request) {
	a := s.Archives
	caller := callerFrom(r.Context())
	j, err := a.StartUpload(r.Context(), caller.Name)
	if errors.Is(err, archivejob.ErrConcurrency) {
		w.Header().Set("Retry-After", "60")
		writeJSONError(w, http.StatusTooManyRequests, "archive_capacity", "the installation is already running its maximum number of archive jobs")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to start the upload")
		return
	}
	deadline := time.Now().Add(a.limits().JobTimeout)
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	a.trackUpload(j.ID, cancel)
	defer a.untrackUpload(j.ID)
	plain, stored, err := a.storeSealed(ctx, j, r.Body, nil)
	bg := context.WithoutCancel(ctx)
	if err != nil {
		reason := "upload failed: " + err.Error()
		status := http.StatusBadRequest
		if errors.Is(err, errArchiveTooLarge) {
			status, reason = http.StatusRequestEntityTooLarge, errArchiveTooLarge.Error()
		}
		if cur, gerr := a.Jobs.Get(bg, j.ID); gerr == nil && !cur.State.Terminal() {
			_ = a.fail(bg, cur, reason)
		} else {
			_ = a.Store.Delete(bg, j.ObjectKey)
		}
		writeJSONError(w, status, "upload_failed", reason)
		return
	}
	// Record the upload only if the job is still waiting for it: the worker
	// may have failed it (deadline, cancel) while the body was streaming.
	for attempt := 0; attempt < 5; attempt++ {
		cur, gerr := a.Jobs.Get(bg, j.ID)
		if gerr != nil || cur.State != archivejob.StateRunning || cur.Finishing != "" || cur.CancelRequested || cur.Step != archivejob.StepReceiving {
			break
		}
		cur.Uploaded, cur.PlainSize, cur.SealedSize, cur.BytesDone = true, plain, stored, plain
		cur.Step, cur.Message = archivejob.StepVerifying, "Verifying the archive"
		err = a.Jobs.Update(bg, cur)
		if err == nil {
			writeJSON(w, http.StatusAccepted, viewArchiveJob(cur))
			return
		}
		if !errors.Is(err, archivejob.ErrConflict) {
			break
		}
	}
	_ = a.Store.Delete(bg, j.ObjectKey)
	writeJSONError(w, http.StatusConflict, "upload_changed", "the upload was canceled or timed out while it was received; retry")
}

// handleAgentImports serves /api/v1/agent-imports[/{id}[/cancel]].
func (s *Server) handleAgentImports(w http.ResponseWriter, r *http.Request) {
	if !s.requireScope(w, r, "", "disk-import", ScopeArchivesAdmin) {
		return
	}
	a := s.Archives
	if ok, reason := a.Available(); !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "archives_unavailable", reason)
		return
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/v1/agent-imports"), "/")
	id, action, _ := strings.Cut(rest, "/")
	if id == "" {
		switch r.Method {
		case http.MethodPost:
			s.startImport(w, r)
		case http.MethodGet:
			jobs, err := a.Jobs.List(r.Context(), archivejob.KindImport, r.URL.Query().Get("agent"), 20)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to list imports")
				return
			}
			// For one agent, only the imports that created the agent now
			// holding that name: a failed import into the name, or an earlier
			// agent of the same name, is not this agent's restore.
			liveUID := ""
			if name := r.URL.Query().Get("agent"); name != "" {
				live := &kyberv1.Agent{}
				if err := s.K8sClient.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.Namespace}, live); err == nil {
					liveUID = string(live.UID)
				}
			}
			caller := callerFrom(r.Context())
			out := []archiveJobView{}
			for _, j := range jobs {
				if liveUID != "" && j.AgentUID != liveUID {
					continue
				}
				if caller != nil && caller.AgentResources.Has(s.Namespace+"/"+j.Agent) {
					out = append(out, viewArchiveJob(j))
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"imports": out})
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
		return
	}
	j, err := a.Jobs.Get(r.Context(), id)
	caller := callerFrom(r.Context())
	if err != nil || j.Kind != archivejob.KindImport || caller == nil || !caller.AgentResources.Has(s.Namespace+"/"+j.Agent) {
		writeJSONError(w, http.StatusNotFound, "not_found", "import not found")
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, viewArchiveJob(j))
	case action == "cancel" && r.Method == http.MethodPost:
		if j.State.Terminal() || j.Restored {
			writeJSONError(w, http.StatusConflict, "import_finished", "the restore has already finished")
			return
		}
		if err := a.RequestCancel(r.Context(), j); err != nil {
			writeJSONError(w, http.StatusConflict, "import_changed", "the import changed; retry")
			return
		}
		writeJSON(w, http.StatusAccepted, viewArchiveJob(j))
	default:
		writeJSONError(w, http.StatusNotFound, "not_found", "unknown import action")
	}
}

// handleArchives serves GET /api/v1/archives: the completed exports and
// uploads the caller may import from, newest first.
func (s *Server) handleArchives(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if !s.requireScope(w, r, "", "disk-archives", ScopeArchivesAdmin) {
		return
	}
	a := s.Archives
	if ok, reason := a.Available(); !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "archives_unavailable", reason)
		return
	}
	caller := callerFrom(r.Context())
	var out []archiveJobView
	now := a.now()
	for _, kind := range []archivejob.Kind{archivejob.KindExport, archivejob.KindUpload} {
		if kind == archivejob.KindUpload && (caller == nil || !caller.AgentResources.all) {
			continue
		}
		jobs, err := a.Jobs.List(r.Context(), kind, "", 100)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to list archives")
			return
		}
		for _, j := range jobs {
			if j.State != archivejob.StateCompleted || j.ExpiresAt != nil && !now.Before(*j.ExpiresAt) {
				continue
			}
			if kind == archivejob.KindExport && (caller == nil || !caller.AgentResources.Has(s.Namespace+"/"+j.Agent)) {
				continue
			}
			out = append(out, viewArchiveJob(j))
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.After(out[k].CreatedAt) })
	if out == nil {
		out = []archiveJobView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"archives": out})
}

// requireInstallationArchiveAccess gates operations not tied to one agent
// (uploads): archives:admin and an installation-wide resource grant.
func (s *Server) requireInstallationArchiveAccess(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireScope(w, r, "", "disk-upload", ScopeArchivesAdmin) {
		return false
	}
	if ok, reason := s.Archives.Available(); !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "archives_unavailable", reason)
		return false
	}
	if c := callerFrom(r.Context()); c == nil || !c.AgentResources.all {
		writeJSONError(w, http.StatusForbidden, "forbidden", "uploading an archive needs an installation-wide agent grant")
		return false
	}
	return true
}
