package api

// archives.go — the control-plane side of agent disk export (MAT-87): job
// creation, the worker that drives each job through its steps, and the pod
// that reads the agent's volume. The public routes are in routes_archives.go
// and the pod's upload endpoint in internal_archives.go. The design is
// docs/design/2026-09-22-agent-disk-export-import.md.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/archivestore"
	agentctrl "github.com/matty-v/kyber/pkg/controllers/agent"
	"github.com/matty-v/kyber/pkg/diskarchive"
	pkgruntimes "github.com/matty-v/kyber/pkg/runtimes"
)

// ArchiveLimits bound disk archive jobs. Zero values take the defaults below.
type ArchiveLimits struct {
	// MaxArchiveBytes bounds one archive's plaintext size.
	MaxArchiveBytes int64
	// PauseTimeout bounds how long an export waits for the agent pod to stop.
	PauseTimeout time.Duration
	// JobTimeout bounds a job's whole running phase, including the pause.
	JobTimeout time.Duration
	// Retention is how long a completed archive stays downloadable.
	Retention time.Duration
	// MaxConcurrentJobs bounds active jobs installation-wide.
	MaxConcurrentJobs int
	// LinkTTL is the lifetime of a download link.
	LinkTTL time.Duration
}

func (l ArchiveLimits) withDefaults() ArchiveLimits {
	if l.MaxArchiveBytes <= 0 {
		l.MaxArchiveBytes = 200 << 30
	}
	if l.PauseTimeout <= 0 {
		l.PauseTimeout = 5 * time.Minute
	}
	if l.JobTimeout <= 0 {
		l.JobTimeout = 2 * time.Hour
	}
	if l.Retention <= 0 {
		l.Retention = 72 * time.Hour
	}
	if l.MaxConcurrentJobs <= 0 {
		l.MaxConcurrentJobs = 2
	}
	if l.LinkTTL <= 0 {
		l.LinkTTL = 10 * time.Minute
	}
	return l
}

// ArchiveService owns disk archive jobs. A nil service, or one with a
// non-empty DisabledReason, reports the feature unavailable.
type ArchiveService struct {
	Client    client.Client
	Namespace string
	Store     archivestore.Store
	Jobs      archivejob.Store
	// SigningKey is the installation's internal signing key. It derives the
	// key-encryption key for sealed archives and signs download links.
	SigningKey []byte
	// ToolImage runs kyber-disk-archive in export and restore pods.
	ToolImage string
	// InternalURL is the control plane's internal API base URL, reachable
	// from agent-labelled pods.
	InternalURL     string
	KyberVersion    string
	PersistenceMode string
	Limits          ArchiveLimits
	Recorder        record.EventRecorder
	DisabledReason  string

	// Now is overridable in tests.
	Now func() time.Time

	// uploads tracks in-flight uploads so a cancel can abort them.
	uploadsMu sync.Mutex
	uploads   map[string]context.CancelFunc
}

func (a *ArchiveService) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func (a *ArchiveService) limits() ArchiveLimits { return a.Limits.withDefaults() }

// Available reports whether jobs can run, and why not.
func (a *ArchiveService) Available() (bool, string) {
	switch {
	case a == nil:
		return false, "disk archives are not configured on this installation"
	case a.DisabledReason != "":
		return false, a.DisabledReason
	case a.Store == nil || a.Jobs == nil || len(a.SigningKey) == 0 || a.ToolImage == "":
		return false, "disk archives are missing storage, a job store, the signing key or the tool image"
	}
	return true, ""
}

var (
	errArchivePhase = errors.New("agent is not in a phase that can be exported")
	errArchiveHeld  = errors.New("agent volume is already held by another archive job")
)

// exportablePhases are the stable phases an export may start from. Transient
// phases are refused so the job never races a lifecycle transition.
var exportablePhases = map[kyberv1.AgentPhase]bool{
	kyberv1.AgentPhaseRunning:         true,
	kyberv1.AgentPhaseStopped:         true,
	kyberv1.AgentPhaseFailed:          true,
	kyberv1.AgentPhaseNeedsAuth:       true,
	kyberv1.AgentPhaseBrokenRuntime:   true,
	kyberv1.AgentPhaseDiskExhausted:   true,
	kyberv1.AgentPhaseMemoryExhausted: true,
}

// StartExport validates the agent and queues an export job.
func (a *ArchiveService) StartExport(ctx context.Context, agentName, requestedBy string) (*archivejob.Job, error) {
	agent := &kyberv1.Agent{}
	if err := a.Client.Get(ctx, types.NamespacedName{Name: agentName, Namespace: a.Namespace}, agent); err != nil {
		return nil, err
	}
	if !exportablePhases[agent.Status.Phase] {
		return nil, fmt.Errorf("%w (current phase: %s)", errArchivePhase, agent.Status.Phase)
	}
	if agent.Annotations[kyberv1.AnnotationArchiveHold] != "" {
		return nil, errArchiveHeld
	}
	now := a.now()
	j := &archivejob.Job{
		ID:          archivejob.NewID(),
		Kind:        archivejob.KindExport,
		Agent:       agentName,
		State:       archivejob.StateQueued,
		Message:     "Queued",
		CreatedAt:   now,
		UpdatedAt:   now,
		RequestedBy: requestedBy,
		AgentUID:    string(agent.UID),
	}
	j.ObjectKey = "exports/" + j.ID + "/archive.sealed"
	if err := a.Jobs.Create(ctx, j, a.limits().MaxConcurrentJobs); err != nil {
		return nil, err
	}
	return j, nil
}

// RequestCancel marks a job for cancellation; the worker does the cleanup.
func (a *ArchiveService) RequestCancel(ctx context.Context, j *archivejob.Job) error {
	if j.State.Terminal() {
		return nil
	}
	j.CancelRequested = true
	if err := a.Jobs.Update(ctx, j); err != nil {
		return err
	}
	a.abortUpload(j.ID)
	return nil
}

// --- worker ---

// Start drives jobs until ctx ends. It is a leader-gated manager Runnable:
// exactly one replica drives jobs, and every step is idempotent against the
// persisted job, so a new leader resumes where the last one stopped.
func (a *ArchiveService) Start(ctx context.Context) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		a.Tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// NeedLeaderElection keeps the worker on the leader only.
func (a *ArchiveService) NeedLeaderElection() bool { return true }

// Tick advances every active job by at most one step.
func (a *ArchiveService) Tick(ctx context.Context) {
	if ok, _ := a.Available(); !ok {
		return
	}
	jobs, err := a.Jobs.ListActive(ctx, a.now())
	if err != nil {
		slog.Warn("disk archives: listing active jobs failed", "error", err)
		return
	}
	for _, j := range jobs {
		var err error
		switch j.Kind {
		case archivejob.KindExport:
			err = a.advanceExport(ctx, j)
		case archivejob.KindImport:
			err = a.advanceImport(ctx, j)
		}
		if err != nil && !errors.Is(err, archivejob.ErrConflict) {
			slog.Warn("disk archives: job step failed", "job", j.ID, "kind", j.Kind, "agent", j.Agent, "error", err)
		}
	}
}

// fail records a terminal failure after releasing everything the job holds.
func (a *ArchiveService) fail(ctx context.Context, j *archivejob.Job, reason string) error {
	return a.finish(ctx, j, archivejob.StateFailed, reason)
}

func (a *ArchiveService) finish(ctx context.Context, j *archivejob.Job, state archivejob.State, reason string) error {
	a.abortUpload(j.ID)
	if err := a.cleanupPod(ctx, j); err != nil {
		return err
	}
	if j.Kind == archivejob.KindExport {
		if err := a.releaseAgent(ctx, j); err != nil {
			return err
		}
	} else if err := a.discardImportDestination(ctx, j); err != nil {
		return err
	}
	if j.ObjectKey != "" && state != archivejob.StateCompleted && !(j.Kind == archivejob.KindImport && j.SourceJobID != "") {
		if err := a.Store.Delete(ctx, j.ObjectKey); err != nil {
			return fmt.Errorf("deleting archive: %w", err)
		}
	}
	now := a.now()
	j.State = state
	j.CompletedAt = &now
	j.Error = boundedRepairOutput(reason)
	switch state {
	case archivejob.StateFailed:
		j.Message = "Failed"
	case archivejob.StateCanceled:
		j.Message = "Canceled"
	}
	j.WrappedKey = nil
	a.event(j, corev1.EventTypeWarning, "DiskArchive"+strings.ToUpper(string(state[:1]))+string(state[1:]), reason)
	return a.Jobs.Update(ctx, j)
}

func (a *ArchiveService) event(j *archivejob.Job, typ, reason, msg string) {
	if a.Recorder == nil {
		return
	}
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: j.Agent, Namespace: a.Namespace, UID: types.UID(j.AgentUID)}}
	a.Recorder.Eventf(agent, typ, reason, "%s job %s: %s", j.Kind, j.ID, boundedRepairOutput(msg))
}

func (a *ArchiveService) advanceExport(ctx context.Context, j *archivejob.Job) error {
	now := a.now()
	if j.State == archivejob.StateCompleted {
		// Retention lapsed (ListActive returns only those).
		if err := a.Store.Delete(ctx, j.ObjectKey); err != nil {
			return fmt.Errorf("deleting expired archive: %w", err)
		}
		j.State = archivejob.StateExpired
		j.Message = "Expired"
		j.WrappedKey = nil
		return a.Jobs.Update(ctx, j)
	}
	if j.CancelRequested {
		return a.finish(ctx, j, archivejob.StateCanceled, "canceled by operator")
	}
	if j.Deadline != nil && now.After(*j.Deadline) {
		return a.fail(ctx, j, "export exceeded its time limit")
	}

	agent := &kyberv1.Agent{}
	err := a.Client.Get(ctx, types.NamespacedName{Name: j.Agent, Namespace: a.Namespace}, agent)
	if k8serrors.IsNotFound(err) || err == nil && j.AgentUID != "" && string(agent.UID) != j.AgentUID {
		return a.fail(ctx, j, "agent was deleted during the export")
	}
	if err != nil {
		return fmt.Errorf("reading agent: %w", err)
	}

	switch {
	case j.State == archivejob.StateQueued:
		return a.beginExport(ctx, j, agent)
	case j.Step == archivejob.StepPausing:
		return a.waitForPause(ctx, j, agent)
	case j.Step == archivejob.StepArchiving:
		return a.watchExportPod(ctx, j)
	case j.Step == archivejob.StepVerifying:
		return a.verifyExport(ctx, j)
	}
	return a.fail(ctx, j, "export is in an unknown step "+string(j.Step))
}

// beginExport applies the hold and, if the agent has a pod, stops it. Both
// are one optimistic-lock patch, so a concurrent lifecycle change makes the
// job retry rather than overwrite it.
func (a *ArchiveService) beginExport(ctx context.Context, j *archivejob.Job, agent *kyberv1.Agent) error {
	if !exportablePhases[agent.Status.Phase] {
		return a.fail(ctx, j, fmt.Sprintf("agent moved to %s before the export started", agent.Status.Phase))
	}
	if hold := agent.Annotations[kyberv1.AnnotationArchiveHold]; hold != "" && hold != j.ID {
		return a.fail(ctx, j, "agent volume is held by another archive job")
	}
	now := a.now()
	deadline := now.Add(a.limits().JobTimeout)
	j.State = archivejob.StateRunning
	j.Step = archivejob.StepPausing
	j.StartedAt = &now
	j.Deadline = &deadline
	j.PriorDesiredPhase = string(agent.Spec.DesiredPhase)
	j.Message = "Pausing the agent"
	// Persist the intent before touching the agent: if this write loses a
	// race nothing has changed yet; if the patch below fails the job is in a
	// state whose release path knows exactly what to undo.
	j.HoldApplied = true
	j.PausedByJob = agent.Spec.DesiredPhase != kyberv1.AgentPhaseStopped
	// Inventory the mounts while the agent's own pod (if any) still exists.
	spec, err := a.podSpecForInventory(ctx, agent)
	if err != nil {
		return a.fail(ctx, j, "describing the agent's mounts: "+err.Error())
	}
	j.Mounts = mountInventory(spec, "agent-"+agent.Name+"-pv")
	if err := a.Jobs.Update(ctx, j); err != nil {
		return err
	}
	before := agent.DeepCopy()
	if agent.Annotations == nil {
		agent.Annotations = map[string]string{}
	}
	agent.Annotations[kyberv1.AnnotationArchiveHold] = j.ID
	if j.PausedByJob {
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
	}
	if err := a.Client.Patch(ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		if k8serrors.IsConflict(err) {
			return nil // next tick re-reads the agent and retries
		}
		return fmt.Errorf("pausing agent: %w", err)
	}
	a.event(j, corev1.EventTypeNormal, "DiskExportStarted", "agent paused for export")
	return nil
}

// waitForPause advances once no agent pod exists, within PauseTimeout.
func (a *ArchiveService) waitForPause(ctx context.Context, j *archivejob.Job, agent *kyberv1.Agent) error {
	if agent.Annotations[kyberv1.AnnotationArchiveHold] != j.ID {
		// The hold patch lost a race; beginExport's retry path.
		return a.reapplyHold(ctx, j, agent)
	}
	pod := &corev1.Pod{}
	err := a.Client.Get(ctx, types.NamespacedName{Name: "agent-" + agent.Name, Namespace: a.Namespace}, pod)
	if err == nil {
		if j.StartedAt != nil && a.now().Sub(*j.StartedAt) > a.limits().PauseTimeout {
			return a.fail(ctx, j, "agent pod did not stop within the pause limit")
		}
		return nil
	}
	if !k8serrors.IsNotFound(err) {
		return fmt.Errorf("reading agent pod: %w", err)
	}
	node, err := a.agentNode(ctx, agent)
	if err != nil {
		return a.fail(ctx, j, err.Error())
	}
	token, err := a.issuePodToken(ctx, j, agent)
	if err != nil {
		return err
	}
	dataKey, err := archivestore.NewDataKey()
	if err != nil {
		return err
	}
	wrapped, err := archivestore.WrapKey(a.SigningKey, dataKey)
	if err != nil {
		return err
	}
	source, err := a.describeSource(ctx, agent)
	if err != nil {
		return err
	}
	j.NodeName = node
	j.WrappedKey = wrapped
	j.TokenHash = hashToken(token)
	j.Step = archivejob.StepArchiving
	j.Message = "Archiving the disk"
	if err := a.Jobs.Update(ctx, j); err != nil {
		return err
	}
	pod, err = a.exportPod(j, agent, node, source, j.Mounts)
	if err != nil {
		return a.fail(ctx, j, err.Error())
	}
	if err := a.Client.Create(ctx, pod); err != nil && !k8serrors.IsAlreadyExists(err) {
		return a.fail(ctx, j, "creating export pod: "+err.Error())
	}
	return nil
}

func (a *ArchiveService) reapplyHold(ctx context.Context, j *archivejob.Job, agent *kyberv1.Agent) error {
	if hold := agent.Annotations[kyberv1.AnnotationArchiveHold]; hold != "" && hold != j.ID {
		return a.fail(ctx, j, "agent volume is held by another archive job")
	}
	before := agent.DeepCopy()
	if agent.Annotations == nil {
		agent.Annotations = map[string]string{}
	}
	agent.Annotations[kyberv1.AnnotationArchiveHold] = j.ID
	if j.PausedByJob {
		agent.Spec.DesiredPhase = kyberv1.AgentPhaseStopped
	}
	if err := a.Client.Patch(ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil && !k8serrors.IsConflict(err) {
		return fmt.Errorf("pausing agent: %w", err)
	}
	return nil
}

// watchExportPod waits for the pod's upload to land and the pod to exit.
func (a *ArchiveService) watchExportPod(ctx context.Context, j *archivejob.Job) error {
	pod := &corev1.Pod{}
	err := a.Client.Get(ctx, types.NamespacedName{Name: archivePodName(j), Namespace: a.Namespace}, pod)
	if k8serrors.IsNotFound(err) {
		if j.Uploaded {
			return a.afterUpload(ctx, j)
		}
		// Created moments ago and not yet in the cache, or deleted underneath
		// us. The job deadline bounds the first case.
		if j.UpdatedAt.Add(2 * time.Minute).Before(a.now()) {
			return a.fail(ctx, j, "export pod disappeared before its upload completed")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading export pod: %w", err)
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		if !j.Uploaded {
			return a.fail(ctx, j, "export pod exited without a completed upload")
		}
		return a.afterUpload(ctx, j)
	case corev1.PodFailed:
		return a.fail(ctx, j, "export pod failed: "+archivePodDiagnostic(pod))
	}
	return nil
}

// afterUpload releases the agent as soon as its bytes are safely stored;
// verification reads the stored copy and does not need the volume.
func (a *ArchiveService) afterUpload(ctx context.Context, j *archivejob.Job) error {
	if err := a.cleanupPod(ctx, j); err != nil {
		return err
	}
	if err := a.releaseAgent(ctx, j); err != nil {
		return err
	}
	j.Step = archivejob.StepVerifying
	j.Message = "Verifying the archive"
	return a.Jobs.Update(ctx, j)
}

func (a *ArchiveService) verifyExport(ctx context.Context, j *archivejob.Job) error {
	r, err := a.openArchive(ctx, j)
	if err != nil {
		return a.fail(ctx, j, "opening stored archive: "+err.Error())
	}
	m, err := diskarchive.Verify(r, r.Size(), diskarchive.Limits{MaxBytes: a.limits().MaxArchiveBytes})
	if err != nil {
		return a.fail(ctx, j, "archive failed verification: "+err.Error())
	}
	now := a.now()
	expires := now.Add(a.limits().Retention)
	j.Summary = archivejob.SummaryOf(m)
	j.State = archivejob.StateCompleted
	j.Step = ""
	j.Message = "Ready to download"
	j.CompletedAt = &now
	j.ExpiresAt = &expires
	j.BytesDone = j.PlainSize
	a.event(j, corev1.EventTypeNormal, "DiskExportCompleted", fmt.Sprintf("%d entries, %d bytes", m.Totals.Entries, j.PlainSize))
	return a.Jobs.Update(ctx, j)
}

// openArchive returns a plaintext reader over a job's sealed object.
func (a *ArchiveService) openArchive(ctx context.Context, j *archivejob.Job) (*archivestore.SealedReader, error) {
	if len(j.WrappedKey) == 0 {
		return nil, errors.New("archive key is no longer available")
	}
	key, err := archivestore.UnwrapKey(a.SigningKey, j.WrappedKey)
	if err != nil {
		return nil, err
	}
	return archivestore.OpenSealed(ctx, a.Store, j.ObjectKey, key)
}

// releaseAgent removes the job's hold and, if the job stopped the agent,
// restores the operator's prior intent. An operator who changed
// desiredPhase during the export keeps their newer intent.
func (a *ArchiveService) releaseAgent(ctx context.Context, j *archivejob.Job) error {
	if !j.HoldApplied {
		return nil
	}
	agent := &kyberv1.Agent{}
	err := a.Client.Get(ctx, types.NamespacedName{Name: j.Agent, Namespace: a.Namespace}, agent)
	if err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("reading agent: %w", err)
	}
	if err == nil && (j.AgentUID == "" || string(agent.UID) == j.AgentUID) {
		before := agent.DeepCopy()
		changed := false
		if agent.Annotations[kyberv1.AnnotationArchiveHold] == j.ID {
			delete(agent.Annotations, kyberv1.AnnotationArchiveHold)
			changed = true
		}
		if j.PausedByJob && agent.Spec.DesiredPhase == kyberv1.AgentPhaseStopped && j.PriorDesiredPhase != "" {
			agent.Spec.DesiredPhase = kyberv1.AgentPhase(j.PriorDesiredPhase)
			changed = true
		}
		if changed {
			if err := a.Client.Patch(ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return fmt.Errorf("releasing agent: %w", err)
			}
		}
	}
	j.HoldApplied = false
	j.PausedByJob = false
	return nil
}

func (a *ArchiveService) cleanupPod(ctx context.Context, j *archivejob.Job) error {
	for _, obj := range []client.Object{
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: archivePodName(j), Namespace: a.Namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: archiveTokenSecretName(j), Namespace: a.Namespace}},
	} {
		grace := int64(0)
		if err := a.Client.Delete(ctx, obj, &client.DeleteOptions{GracePeriodSeconds: &grace}); err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Errorf("deleting %s: %w", obj.GetName(), err)
		}
	}
	return nil
}

// agentNode resolves the node the agent's volume can be mounted on: its
// Machine's node. Same-node placement is what makes node-local storage work.
func (a *ArchiveService) agentNode(ctx context.Context, agent *kyberv1.Agent) (string, error) {
	machine := &kyberv1.Machine{}
	if err := a.Client.Get(ctx, types.NamespacedName{Name: agent.Spec.Machine, Namespace: a.Namespace}, machine); err != nil {
		return "", fmt.Errorf("resolving the agent's machine: %w", err)
	}
	if machine.Status.NodeName == "" {
		return "", errors.New("the agent's machine has no node; start the machine and retry")
	}
	return machine.Status.NodeName, nil
}

// issuePodToken creates the per-job Secret the archive pod authenticates
// with. Only its hash is stored in the job.
func (a *ArchiveService) issuePodToken(ctx context.Context, j *archivejob.Job, owner *kyberv1.Agent) (string, error) {
	raw, err := archivestore.NewDataKey()
	if err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      archiveTokenSecretName(j),
			Namespace: a.Namespace,
			Labels:    archivePodLabels(j),
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"token": token},
	}
	if owner != nil && owner.UID != "" {
		secret.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, kyberv1.GroupVersion.WithKind("Agent"))}
	}
	_ = a.Client.Delete(ctx, secret)
	if err := a.Client.Create(ctx, secret); err != nil {
		return "", fmt.Errorf("creating archive token secret: %w", err)
	}
	return token, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// checkPodToken compares a presented token with the job's stored hash.
func checkPodToken(j *archivejob.Job, presented string) bool {
	if j.TokenHash == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashToken(presented)), []byte(j.TokenHash)) == 1
}

// describeSource collects the manifest's source identity and the agent pod's
// mount inventory. Only non-secret settings are copied into Config.
func (a *ArchiveService) describeSource(ctx context.Context, agent *kyberv1.Agent) (diskarchive.Source, error) {
	spec := agent.Spec
	src := diskarchive.Source{
		Agent:           agent.Name,
		UID:             string(agent.UID),
		Runtime:         spec.Runtime,
		RuntimeVersion:  firstNonEmptyString(agent.Status.Runtime.InstalledVersion, spec.RuntimeVersion),
		Model:           spec.Model,
		Machine:         spec.Machine,
		KyberVersion:    a.KyberVersion,
		PersistenceMode: a.PersistenceMode,
		Config:          archiveConfig(agent),
	}
	pvc := &corev1.PersistentVolumeClaim{}
	pvcName := "agent-" + agent.Name + "-pv"
	if err := a.Client.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: a.Namespace}, pvc); err != nil {
		return src, fmt.Errorf("reading agent volume claim: %w", err)
	}
	src.Disk = diskarchive.Disk{PVC: pvcName}
	if pvc.Spec.StorageClassName != nil {
		src.Disk.StorageClass = *pvc.Spec.StorageClassName
	}
	if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		src.Disk.CapacityBytes = q.Value()
	}
	if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		src.Disk.RequestBytes = q.Value()
	}
	for _, m := range pvc.Spec.AccessModes {
		src.Disk.AccessModes = append(src.Disk.AccessModes, string(m))
	}
	if pvc.Spec.VolumeMode != nil {
		src.Disk.VolumeMode = string(*pvc.Spec.VolumeMode)
	}
	return src, nil
}

// mountInventory states, for every filesystem an agent pod mounts, whether
// the archive contains it, so nothing outside the agent volume is ever
// implied to be backed up. It reads the pod spec itself: the live pod when
// one existed at export time, otherwise the spec the controller would build.
func mountInventory(spec corev1.PodSpec, pvcName string) []diskarchive.Mount {
	kinds := map[string]string{}
	archived := map[string]bool{}
	reasons := map[string]string{}
	for _, v := range spec.Volumes {
		src := v.VolumeSource
		switch {
		case src.PersistentVolumeClaim != nil && src.PersistentVolumeClaim.ClaimName == pvcName:
			kinds[v.Name] = "persistentVolumeClaim " + pvcName
			archived[v.Name] = true
			reasons[v.Name] = "the agent's persistent disk: durable root, home, identity checkout, skills, local work and sessions"
		case src.PersistentVolumeClaim != nil:
			kinds[v.Name] = "persistentVolumeClaim " + src.PersistentVolumeClaim.ClaimName
			reasons[v.Name] = "platform-managed volume; the new agent's controller recreates it"
		case src.Secret != nil:
			kinds[v.Name] = "secret"
			reasons[v.Name] = "Kubernetes Secrets are never exported; re-create or re-authorize them on the new agent"
		case src.Projected != nil:
			kinds[v.Name] = "projected"
			reasons[v.Name] = "platform-rendered credentials or configuration; regenerated for the new agent"
		case src.ConfigMap != nil:
			kinds[v.Name] = "configMap"
			reasons[v.Name] = "platform-rendered configuration; regenerated from the new agent's spec"
		case src.EmptyDir != nil:
			kinds[v.Name] = "emptyDir"
			reasons[v.Name] = "ephemeral by design; discarded on every pod restart"
		case src.HostPath != nil:
			kinds[v.Name] = "hostPath"
			reasons[v.Name] = "node-provided path; belongs to the node, not the agent"
		default:
			kinds[v.Name] = "other"
			reasons[v.Name] = "not agent disk state"
		}
	}
	seen := map[string]bool{}
	out := []diskarchive.Mount{{Path: "/", Kind: "container image", Archived: false,
		Reason: "image-provided and rebuilt from the runtime image; with rootfs persistence the agent's own root lives under /persist/agentroot and is archived"}}
	containers := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
	for _, c := range containers {
		for _, m := range c.VolumeMounts {
			key := m.MountPath + "\x00" + m.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, diskarchive.Mount{
				Path:     m.MountPath,
				Kind:     kinds[m.Name] + " (" + c.Name + ")",
				Archived: archived[m.Name] && m.SubPath == "",
				Reason:   reasons[m.Name],
			})
		}
	}
	return out
}

// podSpecForInventory returns the live agent pod's spec, or the spec the
// controller would build for a stopped agent.
func (a *ArchiveService) podSpecForInventory(ctx context.Context, agent *kyberv1.Agent) (corev1.PodSpec, error) {
	pod := &corev1.Pod{}
	err := a.Client.Get(ctx, types.NamespacedName{Name: "agent-" + agent.Name, Namespace: a.Namespace}, pod)
	if err == nil {
		return pod.Spec, nil
	}
	if !k8serrors.IsNotFound(err) {
		return corev1.PodSpec{}, fmt.Errorf("reading agent pod: %w", err)
	}
	rt, ok := pkgruntimes.Get(agent.Spec.Runtime)
	if !ok {
		return corev1.PodSpec{}, fmt.Errorf("unknown runtime %q", agent.Spec.Runtime)
	}
	return agentctrl.BuildPodSpec(agent, rt.Adapter(), "")
}

// archiveConfig is the non-secret subset of the spec a create-from-export
// flow can offer as defaults. Secret names, key material, peers and inference
// endpoints are deliberately absent.
func archiveConfig(agent *kyberv1.Agent) map[string]any {
	s := agent.Spec
	jobs := make([]map[string]any, 0, len(s.Jobs))
	for _, jb := range s.Jobs {
		jobs = append(jobs, map[string]any{
			"name": jb.Name, "schedule": jb.Schedule, "prompt": jb.Prompt,
			"exclusive": jb.Exclusive, "clearContextAfter": jb.ClearContextAfter,
		})
	}
	bindings := make([]string, 0, len(s.InboundBindings))
	for _, b := range s.InboundBindings {
		bindings = append(bindings, b.Name)
	}
	return map[string]any{
		"runtime":             s.Runtime,
		"runtimeVersion":      s.RuntimeVersion,
		"model":               s.Model,
		"machine":             s.Machine,
		"resources":           map[string]string{"cpu": s.Resources.CPU.String(), "memory": s.Resources.Memory.String(), "disk": s.Resources.Disk.String()},
		"startupPrompt":       s.StartupPrompt,
		"sessionResume":       s.SessionResume,
		"requestReplyEnabled": s.RequestReplyEnabled,
		"identityRepo":        s.IdentityRepo.Repo,
		"authType":            string(s.Secrets.AuthType),
		"channels": map[string]bool{
			"telegram": s.Secrets.TelegramEnabled,
			"slack":    s.Secrets.SlackEnabled,
			"discord":  s.Secrets.DiscordEnabled,
		},
		"jobs":            jobs,
		"inboundBindings": bindings,
	}
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func archivePodLabels(j *archivejob.Job) map[string]string {
	return map[string]string{
		"app.kubernetes.io/part-of":   "kyber",
		"app.kubernetes.io/component": "disk-archive",
		// kyber.io/agent places the pod under the agent network policies:
		// no ingress, egress only to DNS, the control plane and the internet.
		"kyber.io/agent":       j.Agent,
		"kyber.io/archive-job": j.ID,
	}
}

func archivePodName(j *archivejob.Job) string {
	return "disk-" + string(j.Kind) + "-" + j.ID
}

func archiveTokenSecretName(j *archivejob.Job) string {
	return archivePodName(j) + "-token"
}

// exportPod builds the pod that archives the agent volume. It mounts the
// volume read-only, has no service-account token, and can do nothing but
// read the disk and upload to its own job's endpoint.
func (a *ArchiveService) exportPod(j *archivejob.Job, agent *kyberv1.Agent, node string, src diskarchive.Source, mounts []diskarchive.Mount) (*corev1.Pod, error) {
	desc, err := json.Marshal(struct {
		Source diskarchive.Source  `json:"source"`
		Mounts []diskarchive.Mount `json:"mounts"`
	}{src, mounts})
	if err != nil {
		return nil, err
	}
	env := []corev1.EnvVar{
		{Name: "KYBER_ARCHIVE_UPLOAD_URL", Value: strings.TrimRight(a.InternalURL, "/") + "/internal/archive-jobs/" + j.ID + "/upload"},
		{Name: "KYBER_ARCHIVE_DESCRIPTION", Value: string(desc)},
		{Name: "KYBER_ARCHIVE_MAX_BYTES", Value: fmt.Sprint(a.limits().MaxArchiveBytes)},
	}
	caps := []corev1.Capability{"DAC_READ_SEARCH"}
	return a.archivePod(j, agent, node, "export", env, true, caps), nil
}

func (a *ArchiveService) archivePod(j *archivejob.Job, owner *kyberv1.Agent, node, mode string, env []corev1.EnvVar, readOnly bool, caps []corev1.Capability) *corev1.Pod {
	deadline := int64(a.limits().JobTimeout.Seconds())
	automount := false
	root := int64(0)
	readOnlyRoot := true
	noEscalation := false
	env = append(env, corev1.EnvVar{Name: "KYBER_ARCHIVE_TOKEN", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: archiveTokenSecretName(j)},
			Key:                  "token",
		},
	}})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      archivePodName(j),
			Namespace: a.Namespace,
			Labels:    archivePodLabels(j),
		},
		Spec: corev1.PodSpec{
			NodeName:                     node,
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        &deadline,
			AutomountServiceAccountToken: &automount,
			Volumes: []corev1.Volume{{
				Name: "persist",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "agent-" + j.Agent + "-pv",
					ReadOnly:  readOnly,
				}},
			}},
			Containers: []corev1.Container{{
				Name:    "archive",
				Image:   a.ToolImage,
				Command: []string{"/usr/local/bin/kyber-disk-archive", mode, "--root", "/persist"},
				Env:     env,
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                &root,
					ReadOnlyRootFilesystem:   &readOnlyRoot,
					AllowPrivilegeEscalation: &noEscalation,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: caps},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "persist", MountPath: "/persist", ReadOnly: readOnly}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
				},
			}},
		},
	}
	if owner != nil && owner.UID != "" {
		pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, kyberv1.GroupVersion.WithKind("Agent"))}
	}
	return pod
}

func archivePodDiagnostic(pod *corev1.Pod) string {
	for _, st := range pod.Status.ContainerStatuses {
		if t := st.State.Terminated; t != nil {
			// The termination message is written by kyber-disk-archive and is a
			// bounded, content-free description (it never includes file data).
			if t.Message != "" {
				return fmt.Sprintf("exit %d: %s", t.ExitCode, boundedRepairOutput(t.Message))
			}
			return fmt.Sprintf("exit %d (%s)", t.ExitCode, t.Reason)
		}
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}
	return "pod terminated without a diagnostic"
}

// --- uploads ---

func (a *ArchiveService) trackUpload(id string, cancel context.CancelFunc) {
	a.uploadsMu.Lock()
	defer a.uploadsMu.Unlock()
	if a.uploads == nil {
		a.uploads = map[string]context.CancelFunc{}
	}
	a.uploads[id] = cancel
}

func (a *ArchiveService) untrackUpload(id string) {
	a.uploadsMu.Lock()
	defer a.uploadsMu.Unlock()
	delete(a.uploads, id)
}

func (a *ArchiveService) abortUpload(id string) {
	a.uploadsMu.Lock()
	defer a.uploadsMu.Unlock()
	if cancel := a.uploads[id]; cancel != nil {
		cancel()
	}
}

// errArchiveTooLarge is returned when an upload exceeds MaxArchiveBytes.
var errArchiveTooLarge = errors.New("archive exceeds the installation's size limit")

// storeSealed seals body into the job's object. It returns the plaintext and
// stored sizes. The plaintext is counted as it streams so an oversized
// archive is cut off rather than stored.
func (a *ArchiveService) storeSealed(ctx context.Context, j *archivejob.Job, body io.Reader, progress func(int64)) (int64, int64, error) {
	key, err := archivestore.UnwrapKey(a.SigningKey, j.WrappedKey)
	if err != nil {
		return 0, 0, err
	}
	max := a.limits().MaxArchiveBytes
	pr, pw := io.Pipe()
	var plainN atomic.Int64
	go func() {
		var plain int64
		sw, err := archivestore.NewSealWriter(pw, key)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		buf := make([]byte, 256<<10)
		for {
			n, rerr := body.Read(buf)
			if n > 0 {
				plain += int64(n)
				plainN.Store(plain)
				if plain > max {
					pw.CloseWithError(errArchiveTooLarge)
					return
				}
				if _, werr := sw.Write(buf[:n]); werr != nil {
					pw.CloseWithError(werr)
					return
				}
				if progress != nil {
					progress(plain)
				}
			}
			if rerr == io.EOF {
				pw.CloseWithError(sw.Close())
				return
			}
			if rerr != nil {
				pw.CloseWithError(rerr)
				return
			}
		}
	}()
	stored, err := a.Store.Put(ctx, j.ObjectKey, pr)
	pr.CloseWithError(err)
	if err != nil {
		if errors.Is(err, errArchiveTooLarge) || plainN.Load() > max {
			return 0, 0, errArchiveTooLarge
		}
		return 0, 0, err
	}
	return plainN.Load(), stored, nil
}
