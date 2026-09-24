package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/capabilities"
	"github.com/matty-v/kyber/pkg/diskarchive"
	"github.com/matty-v/kyber/pkg/taskobject"
)

// errStaleAgent is a cached read that still shows another agent by the name.
var errStaleAgent = errors.New("the cache does not show the new agent yet")

// importApplyBackoff spans the few seconds a cache-backed client can lag a
// create.
var importApplyBackoff = wait.Backoff{Steps: 8, Duration: 50 * time.Millisecond, Factor: 2, Jitter: 0.1}

const (
	applyPaused      = "paused"
	applyDisabled    = "disabled"
	applySkip        = "skip"
	applyCopyIfLocal = "copy-if-local"
)

// importApply chooses what an import carries from the archive to the new
// agent beyond its disk. The zero value means the defaults: apply the
// configuration, restore jobs paused and bindings disabled so nothing fires
// on both agents, and copy user-secret values when the source agent is on
// this installation.
type importApply struct {
	Config   *bool  `json:"config,omitempty"`
	Jobs     string `json:"jobs,omitempty"`
	Bindings string `json:"bindings,omitempty"`
	Secrets  string `json:"secrets,omitempty"`
}

func (a importApply) resolved() (importApply, error) {
	if a.Config == nil {
		on := true
		a.Config = &on
	}
	if a.Jobs == "" {
		a.Jobs = applyPaused
	}
	if a.Bindings == "" {
		a.Bindings = applyDisabled
	}
	if a.Secrets == "" {
		a.Secrets = applyCopyIfLocal
	}
	switch {
	case a.Jobs != applyPaused && a.Jobs != applySkip:
		return a, fmt.Errorf("apply.jobs must be %q or %q", applyPaused, applySkip)
	case a.Bindings != applyDisabled && a.Bindings != applySkip:
		return a, fmt.Errorf("apply.bindings must be %q or %q", applyDisabled, applySkip)
	case a.Secrets != applyCopyIfLocal && a.Secrets != applySkip:
		return a, fmt.Errorf("apply.secrets must be %q or %q", applyCopyIfLocal, applySkip)
	}
	return a, nil
}

// importPlan is what an import will carry, decided before the agent is
// created so the cutover checklist can be written up front.
type importPlan struct {
	apply importApply
	cfg   *diskarchive.Config
	// sameRuntime is false when the new agent runs a different harness than
	// the source; runtime-scoped settings (model) do not carry across.
	sameRuntime bool
	// missingPeerSecrets names A2A peers left off because the Secret their
	// credential references does not exist on this installation.
	missingPeerSecrets []string
	// sourceSecrets is set when user-secret values will be copied from a
	// source agent on this installation.
	sourceSecrets *kyberv1.Agent
	// identityRepoUnlinked is set when the archive's identity repo could not
	// be linked because this installation has no GitHub App.
	identityRepoUnlinked bool
	// failed collects anything the post-create apply could not carry.
	failed []string
	// createdSecrets are the Secrets the apply made, for the import's cleanup
	// if the restore is abandoned.
	createdSecrets []string
}

func (p *importPlan) configOn() bool { return p.cfg != nil && *p.apply.Config }

// planImport resolves the import's apply options against the archive and this
// installation.
func (s *Server) planImport(ctx context.Context, src *archivejob.Job, req agentImportRequest) (*importPlan, error) {
	apply, err := req.Apply.resolved()
	if err != nil {
		return nil, err
	}
	p := &importPlan{apply: apply, cfg: src.Summary.Source.Config}
	p.sameRuntime = src.Summary.Source.Runtime == "" || req.Agent.Runtime == "" || src.Summary.Source.Runtime == req.Agent.Runtime
	if !p.configOn() {
		return p, nil
	}
	if apply.Secrets == applyCopyIfLocal && len(p.cfg.UserSecrets) > 0 {
		srcAgent := &kyberv1.Agent{}
		err := s.K8sClient.Get(ctx, types.NamespacedName{Name: src.Summary.Source.Agent, Namespace: s.Namespace}, srcAgent)
		if err == nil && (src.Summary.Source.UID == "" || string(srcAgent.UID) == src.Summary.Source.UID) {
			p.sourceSecrets = srcAgent
		} else if err != nil && !k8serrors.IsNotFound(err) {
			return nil, fmt.Errorf("checking the source agent: %w", err)
		}
	}
	return p, nil
}

// mergeArchiveConfig fills create-request fields the operator left open from
// the archive. Explicit request values win; a boolean the archive has on
// stays on (the create request cannot tell "off" from "unset").
func (s *Server) mergeArchiveConfig(ctx context.Context, p *importPlan, req *CreateAgentRequest) error {
	if !p.configOn() {
		return nil
	}
	c := p.cfg
	if req.Model == "" && p.sameRuntime {
		req.Model = c.Model
	}
	if req.StartupPrompt == "" {
		req.StartupPrompt = c.StartupPrompt
	}
	req.SessionResume = req.SessionResume || c.SessionResume
	req.RequestReplyEnabled = req.RequestReplyEnabled || c.RequestReplyEnabled
	if req.Resources.CPU == "" {
		req.Resources.CPU = c.Resources.CPU
	}
	if req.Resources.Memory == "" {
		req.Resources.Memory = c.Resources.Memory
	}
	if req.Identity.SoulDescription == "" {
		req.Identity.SoulDescription = c.SoulDescription
	}
	if req.IdentityRepo.Repo == "" && req.IdentityRepo.Template == "" && c.IdentityRepo != "" {
		// Link the repo only where this installation can; the checkout is on
		// the restored disk either way.
		if slices.Contains(s.identityRepoCapability().SupportedModes, IdentityRepoModeExisting) {
			req.IdentityRepo.Repo = c.IdentityRepo
		} else {
			p.identityRepoUnlinked = true
		}
	}
	if len(req.A2APeers) == 0 && len(c.A2APeers) > 0 {
		var peers []kyberv1.AgentA2APeer
		if err := json.Unmarshal(c.A2APeers, &peers); err != nil {
			return fmt.Errorf("reading the archive's A2A peers: %w", err)
		}
		for _, peer := range peers {
			ref := peer.Credential.ExistingSecret
			if ref != "" {
				err := s.K8sClient.Get(ctx, types.NamespacedName{Name: ref, Namespace: s.Namespace}, &corev1.Secret{})
				if k8serrors.IsNotFound(err) {
					p.missingPeerSecrets = append(p.missingPeerSecrets, fmt.Sprintf("%s (needs Secret %s)", peer.Name, ref))
					continue
				} else if err != nil {
					return fmt.Errorf("checking A2A peer Secret %s: %w", ref, err)
				}
			}
			req.A2APeers = append(req.A2APeers, peer)
		}
	}
	return nil
}

// applyArchiveToAgent carries the settings the create request cannot express
// onto the held agent: jobs (paused), inbound bindings (disabled, with fresh
// signing secrets), profile and avatar, public capabilities and user-secret
// values. The agent does not start until its restore completes, so none of
// this can act before cutover. Anything that cannot be carried is recorded on
// the plan for the checklist rather than failing a good restore.
func (s *Server) applyArchiveToAgent(ctx context.Context, name string, uid types.UID, p *importPlan) {
	if !p.configOn() {
		return
	}
	c := p.cfg
	var bindings []kyberv1.AgentInboundBinding
	if p.apply.Bindings == applyDisabled && len(c.InboundBindingSpecs) > 0 {
		var specs []kyberv1.AgentInboundBinding
		if err := json.Unmarshal(c.InboundBindingSpecs, &specs); err != nil {
			p.failed = append(p.failed, "inbound bindings: the archive's definitions could not be read")
		}
		for _, b := range specs {
			secretName, _, err := s.createBindingSecret(ctx, name, b.Name)
			if err != nil {
				p.failed = append(p.failed, fmt.Sprintf("inbound binding %s: %v", b.Name, err))
				continue
			}
			p.createdSecrets = append(p.createdSecrets, secretName)
			b.ExistingSecret, b.Disabled = secretName, true
			bindings = append(bindings, b)
		}
	}
	var caps *kyberv1.AgentPublicCapabilities
	if len(c.PublicCapabilities) > 0 {
		decl := &kyberv1.AgentPublicCapabilities{}
		if err := json.Unmarshal(c.PublicCapabilities, decl); err != nil {
			p.failed = append(p.failed, "public capabilities: the archive's declaration could not be read")
		} else if _, _, err := capabilities.NormalizeAndValidate(decl); err != nil {
			p.failed = append(p.failed, "public capabilities: "+err.Error())
		} else {
			caps = decl
		}
	}
	var avatarKey string
	if c.Profile != nil && len(c.Profile.Avatar) > 0 {
		if s.TaskObjectStore == nil {
			p.failed = append(p.failed, "profile avatar: this installation has no object store for avatars")
		} else {
			key := "agent-avatars/" + name
			err := s.TaskObjectStore.Put(ctx, key, bytes.NewReader(c.Profile.Avatar), int64(len(c.Profile.Avatar)),
				taskobject.PutOptions{Filename: name + "-avatar", ContentType: c.Profile.AvatarContentType})
			if err != nil {
				p.failed = append(p.failed, "profile avatar: "+err.Error())
			} else {
				avatarKey = key
			}
		}
	}

	agent := &kyberv1.Agent{}
	// The client may be cache-backed and not show the just-created agent yet.
	notYet := func(err error) bool {
		return k8serrors.IsConflict(err) || k8serrors.IsNotFound(err) || errors.Is(err, errStaleAgent)
	}
	err := retry.OnError(importApplyBackoff, notYet, func() error {
		if err := s.K8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: s.Namespace}, agent); err != nil {
			return err
		}
		if agent.UID != uid {
			return errStaleAgent
		}
		if p.apply.Jobs == applyPaused {
			agent.Spec.Jobs = nil
			for _, jb := range c.Jobs {
				agent.Spec.Jobs = append(agent.Spec.Jobs, kyberv1.AgentJob{
					Name: jb.Name, Schedule: jb.Schedule, Prompt: jb.Prompt,
					Exclusive: jb.Exclusive, ClearContextAfter: jb.ClearContextAfter, Paused: true,
				})
			}
		}
		agent.Spec.InboundBindings = bindings
		agent.Spec.PublicCapabilities = caps
		if c.Profile != nil {
			agent.Spec.Profile.Alias, agent.Spec.Profile.Description = c.Profile.Alias, c.Profile.Description
			if avatarKey != "" {
				agent.Spec.Profile.AvatarKey = avatarKey
				agent.Spec.Profile.AvatarContentType = strings.ToLower(strings.Split(c.Profile.AvatarContentType, ";")[0])
			}
		}
		return s.K8sClient.Update(ctx, agent)
	})
	if err != nil {
		p.failed = append(p.failed, "jobs, bindings, profile and capabilities could not be saved: "+err.Error())
		return
	}
	if p.sourceSecrets != nil {
		if err := s.copyUserSecrets(ctx, p, p.sourceSecrets.Name, agent); err != nil {
			p.failed = append(p.failed, "user secrets: "+err.Error())
			p.sourceSecrets = nil
		}
	}
}

// copyUserSecrets copies user-secret values between agents on this
// installation. The values never leave the cluster.
func (s *Server) copyUserSecrets(ctx context.Context, p *importPlan, source string, dst *kyberv1.Agent) error {
	for _, kind := range []userSecretKind{userSecretKindKV, userSecretKindFile} {
		src := &corev1.Secret{}
		if err := s.K8sClient.Get(ctx, types.NamespacedName{Name: userSecretsSecretName(source, kind), Namespace: s.Namespace}, src); err != nil {
			if k8serrors.IsNotFound(err) {
				continue
			}
			return err
		}
		name := userSecretsSecretName(dst.Name, kind)
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := &corev1.Secret{}
			err := s.K8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: s.Namespace}, cur)
			if k8serrors.IsNotFound(err) {
				cur = &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: name, Namespace: s.Namespace,
						Labels: map[string]string{
							"app.kubernetes.io/managed-by": "kyber-controller",
							"kyber.io/agent":               dst.Name,
							"kyber.io/secret-kind":         "user-secrets",
						},
						OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(dst, kyberv1.GroupVersion.WithKind("Agent"))},
					},
					Type: corev1.SecretTypeOpaque,
				}
				cur.Data, cur.Annotations = src.Data, map[string]string{}
				if v, ok := src.Annotations[userSecretsMetadataAnnotation]; ok {
					cur.Annotations[userSecretsMetadataAnnotation] = v
				}
				if err := s.K8sClient.Create(ctx, cur); err != nil {
					return err
				}
				p.createdSecrets = append(p.createdSecrets, name)
				return nil
			} else if err != nil {
				return err
			}
			cur.Data = src.Data
			if cur.Annotations == nil {
				cur.Annotations = map[string]string{}
			}
			if v, ok := src.Annotations[userSecretsMetadataAnnotation]; ok {
				cur.Annotations[userSecretsMetadataAnnotation] = v
			}
			return s.K8sClient.Update(ctx, cur)
		})
		if err != nil {
			return fmt.Errorf("copying %s: %w", name, err)
		}
	}
	return nil
}

// secretList formats user secrets for the checklist.
func secretList(secrets []diskarchive.ConfigSecret) string {
	parts := make([]string, 0, len(secrets))
	for _, sec := range secrets {
		parts = append(parts, fmt.Sprintf("%s (%s)", sec.Key, sec.Kind))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
