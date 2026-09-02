package taskstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

type idempotencyRecord struct{ hash, taskID string }
type cancelIdempotencyRecord struct {
	hash, taskID string
	applied      bool
}

// MemoryStore is a contract test double. Production public routes must use the
// PostgreSQL store and must never silently fall back to this implementation.
type MemoryStore struct {
	mu                 sync.Mutex
	limits             Limits
	now                func() time.Time
	tasks              map[string]*Task
	idempotency        map[string]idempotencyRecord
	updates            map[string]map[string]string
	cancelKeys         map[string]cancelIdempotencyRecord
	cancelAcks         map[string]string
	interactionKeys    map[string]idempotencyRecord
	authorizationFlows map[string]memoryAuthorizationFlow
}

func NewMemoryStore(limits Limits) (*MemoryStore, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &MemoryStore{limits: limits, now: time.Now, tasks: map[string]*Task{}, idempotency: map[string]idempotencyRecord{}, updates: map[string]map[string]string{}, cancelKeys: map[string]cancelIdempotencyRecord{}, cancelAcks: map[string]string{}, interactionKeys: map[string]idempotencyRecord{}, authorizationFlows: map[string]memoryAuthorizationFlow{}}, nil
}

type memoryAuthorizationFlow struct {
	taskID, interactionID, createdBy, reference string
	complete                                    bool
}

func (s *MemoryStore) Create(_ context.Context, p CreateParams) (*CreateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCreate(s.limits, p); err != nil {
		return nil, err
	}
	auth := p.Authorization
	if !auth.Valid() {
		auth = AuthorizationContext{TenantID: "tenant_legacy", PrincipalID: p.CreatedBy, AgentResourceID: p.Agent.Namespace + "/" + p.Agent.Name}
	}
	key := auth.TenantID + "\x00" + auth.PrincipalID + "\x00" + auth.AgentResourceID + "\x00" + p.Agent.Namespace + "\x00" + p.Agent.Name + "\x00" + p.IdempotencyKey
	if p.IdempotencyKey != "" {
		if old, ok := s.idempotency[key]; ok {
			if old.hash != p.RequestHash {
				return nil, ErrIdempotencyConflict
			}
			t := s.tasks[old.taskID]
			if t == nil || t.TenantID != auth.TenantID || t.OwnerPrincipalID != auth.PrincipalID || t.AgentResourceID != auth.AgentResourceID {
				return nil, ErrNotFound
			}
			return &CreateResult{Task: cloneTask(t), Replay: true}, nil
		}
	}
	if _, ok := s.tasks[p.ID]; ok {
		return nil, ErrConflict
	}
	if len(s.tasks) >= s.limits.MaxRetained {
		return nil, ErrCapacity
	}
	outstanding := 0
	for _, t := range s.tasks {
		if t.AgentNamespace == p.Agent.Namespace && t.AgentName == p.Agent.Name && (t.State == StateQueued || t.State == StateDispatched || t.State == StateCanceling || t.State == StateInputRequired || t.State == StateAuthRequired) {
			outstanding++
		}
	}
	if outstanding >= s.limits.MaxOutstanding {
		return nil, ErrOutstandingLimit
	}
	now := s.now().UTC()
	deadline := p.DeadlineAt.UTC()
	if deadline.IsZero() {
		deadline = now.Add(s.limits.DefaultDeadline)
	}
	if !deadline.After(now) {
		return nil, ErrInvalid
	}
	if deadline.After(now.Add(s.limits.MaxDeadline)) {
		deadline = now.Add(s.limits.MaxDeadline)
	}
	t := &Task{ID: p.ID, AgentNamespace: p.Agent.Namespace, AgentName: p.Agent.Name, CreatedBy: p.CreatedBy, TenantID: auth.TenantID, OwnerPrincipalID: auth.PrincipalID, AgentResourceID: auth.AgentResourceID, Prompt: p.Prompt, Correlation: p.Correlation, State: StateQueued, Version: 1, CreatedAt: now, UpdatedAt: now, DeadlineAt: deadline, RetainUntil: deadline.Add(s.limits.Retention), Messages: []Message{{Sequence: 1, Role: "caller", Kind: "task_instruction", Text: p.Prompt, CreatedAt: now}}}
	s.tasks[t.ID] = t
	if p.IdempotencyKey != "" {
		s.idempotency[key] = idempotencyRecord{p.RequestHash, t.ID}
	}
	return &CreateResult{Task: cloneTask(t)}, nil
}

func validateCreate(l Limits, p CreateParams) error {
	if p.ID == "" || p.Agent.Namespace == "" || p.Agent.Name == "" || p.CreatedBy == "" || strings.TrimSpace(p.Prompt) == "" {
		return ErrInvalid
	}
	if len([]byte(p.Prompt)) > l.MaxPromptBytes {
		return ErrPromptTooLarge
	}
	if len([]byte(p.Correlation)) > l.MaxCorrelationBytes {
		return ErrCorrelationTooLarge
	}
	if len([]byte(p.IdempotencyKey)) > HardMaxIdempotencyBytes {
		return ErrIdempotencyTooLarge
	}
	if p.IdempotencyKey != "" && p.RequestHash == "" {
		return ErrInvalid
	}
	return nil
}

func (s *MemoryStore) Get(_ context.Context, a AgentRef, id string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || t.AgentNamespace != a.Namespace || t.AgentName != a.Name {
		return nil, ErrNotFound
	}
	return cloneTask(t), nil
}

func (s *MemoryStore) GetAuthorized(_ context.Context, a AgentRef, id string, auth AuthorizationContext) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || t.AgentNamespace != a.Namespace || t.AgentName != a.Name || !auth.Valid() || t.TenantID != auth.TenantID || t.OwnerPrincipalID != auth.PrincipalID || t.AgentResourceID != auth.AgentResourceID {
		return nil, ErrNotFound
	}
	return cloneTask(t), nil
}

func (s *MemoryStore) List(_ context.Context, p ListParams) (*Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := p.Limit
	if limit == 0 {
		limit = s.limits.DefaultListPage
	}
	if limit < 1 || limit > s.limits.MaxListPage {
		return nil, ErrInvalid
	}
	if p.State != "" && len(p.States) != 0 {
		return nil, ErrInvalid
	}
	var before time.Time
	var beforeID string
	var err error
	if p.Cursor != "" {
		before, beforeID, err = decodeCursor(p.Cursor, p)
		if err != nil {
			return nil, err
		}
	}
	items := make([]*Task, 0)
	total := 0
	states := make(map[State]bool, len(p.States))
	for _, state := range p.States {
		states[state] = true
	}
	for _, t := range s.tasks {
		if t.AgentNamespace != p.Agent.Namespace || t.AgentName != p.Agent.Name || (p.Authorization.Valid() && (t.TenantID != p.Authorization.TenantID || t.OwnerPrincipalID != p.Authorization.PrincipalID || t.AgentResourceID != p.Authorization.AgentResourceID)) || (p.State != "" && t.State != p.State) || (len(states) != 0 && !states[t.State]) || (p.Correlation != "" && t.Correlation != p.Correlation) || (!p.UpdatedAfter.IsZero() && !t.UpdatedAt.After(p.UpdatedAfter)) {
			continue
		}
		total++
		if !before.IsZero() && (t.CreatedAt.After(before) || (t.CreatedAt.Equal(before) && t.ID >= beforeID)) {
			continue
		}
		items = append(items, cloneTask(t))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID > items[j].ID
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	page := &Page{Total: total}
	if len(items) > limit {
		page.Tasks = items[:limit]
		page.NextCursor, err = encodeCursor(p, page.Tasks[len(page.Tasks)-1])
		if err != nil {
			return nil, err
		}
	} else {
		page.Tasks = items
	}
	return page, nil
}

func (s *MemoryStore) locked(a AgentRef, id string) (*Task, error) {
	t, ok := s.tasks[id]
	if !ok || t.AgentNamespace != a.Namespace || t.AgentName != a.Name {
		return nil, ErrNotFound
	}
	return t, nil
}

func (s *MemoryStore) MarkDispatched(_ context.Context, a AgentRef, id string, v int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(a, id)
	if err != nil {
		return err
	}
	if t.State == StateDispatched {
		return nil
	}
	if t.Version != v || t.State != StateQueued {
		return ErrConflict
	}
	t.State = StateDispatched
	t.Version++
	t.UpdatedAt = s.now().UTC()
	return nil
}
func (s *MemoryStore) Fail(_ context.Context, a AgentRef, id string, v int64, code FailureCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(a, id)
	if err != nil {
		return err
	}
	if t.State == StateFailed && t.FailureCode == code {
		return nil
	}
	if t.Version != v || (t.State != StateQueued && t.State != StateDispatched && t.State != StateCanceling) {
		return ErrConflict
	}
	now := s.now().UTC()
	t.State = StateFailed
	t.FailureCode = code
	t.Version++
	t.UpdatedAt = now
	t.CompletedAt = &now
	return nil
}
func (s *MemoryStore) Complete(_ context.Context, a AgentRef, id string, v int64, response string) error {
	if len([]byte(response)) > s.limits.MaxResponseBytes {
		return ErrResponseTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(a, id)
	if err != nil {
		return err
	}
	if t.State == StateCompleted && t.Response == response {
		return nil
	}
	if t.Version != v || (t.State != StateDispatched && t.State != StateCanceling) {
		return ErrConflict
	}
	now := s.now().UTC()
	t.State = StateCompleted
	t.Response = response
	if response != "" && len(t.Results) == 0 {
		sum := sha256.Sum256([]byte(t.ID))
		r := Result{ID: "result_" + hex.EncodeToString(sum[:16]), Name: "response", Parts: []ResultPart{{ID: "part_0", Kind: PartText, Text: response}}, CreatedAt: now}
		r.ContentDigest = resultDigest(r)
		t.Results = []Result{r}
	}
	t.Version++
	t.UpdatedAt = now
	t.CompletedAt = &now
	return nil
}

func (s *MemoryStore) RequestInteraction(_ context.Context, p RequestInteractionParams) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(p.Agent, p.TaskID)
	if err != nil {
		return nil, err
	}
	if t.State != StateDispatched || p.InteractionID == "" || p.AttemptID == "" || strings.TrimSpace(p.Question) == "" {
		return nil, ErrConflict
	}
	if t.Interaction != nil && (t.Interaction.Status == InteractionPaused || t.Interaction.Status == InteractionAnswered) {
		return nil, ErrConflict
	}
	if len([]byte(p.Question)) > s.limits.MaxInteractionQuestionBytes || len(p.Options) > s.limits.MaxInteractionOptions {
		return nil, ErrInvalid
	}
	count := 0
	for _, m := range t.Messages {
		if m.Kind == "input_request" || m.Kind == "authorization_request" {
			count++
		}
	}
	if count >= s.limits.MaxInteractions || len(t.Messages)+1 >= s.limits.MaxMessages {
		return nil, ErrInteractionLimit
	}
	if p.Type != InteractionText && p.Type != InteractionChoice && p.Type != InteractionConfirm && p.Type != InteractionJSON && p.Type != InteractionAuthorization {
		return nil, ErrInvalid
	}
	if p.Type == InteractionAuthorization && !registeredAuthorizationKind(p.AuthorizationFlow) {
		return nil, ErrInvalid
	}
	now := s.now().UTC()
	expiry := p.ExpiresIn
	if expiry <= 0 {
		expiry = s.limits.DefaultInteractionExpiry
	}
	if expiry > s.limits.MaxInteractionExpiry {
		expiry = s.limits.MaxInteractionExpiry
	}
	expires := now.Add(expiry)
	if expires.After(t.DeadlineAt) {
		expires = t.DeadlineAt
	}
	if p.Type == InteractionAuthorization {
		flowID := authorizationFlowID(p.InteractionID)
		s.authorizationFlows[flowID] = memoryAuthorizationFlow{taskID: p.TaskID, interactionID: p.InteractionID, createdBy: t.CreatedBy}
		p.AuthorizationFlow = flowID
	}
	i := &Interaction{ID: p.InteractionID, AttemptID: p.AttemptID, Type: p.Type, Status: InteractionPaused, Question: p.Question, Options: append([]InteractionOption(nil), p.Options...), Schema: append(json.RawMessage(nil), p.Schema...), AuthorizationFlow: p.AuthorizationFlow, CreatedAt: now, ExpiresAt: expires}
	t.Interaction = i
	state, kind := StateInputRequired, "input_request"
	if p.Type == InteractionAuthorization {
		state, kind = StateAuthRequired, "authorization_request"
	}
	t.State = state
	t.Version++
	t.UpdatedAt = now
	t.Messages = append(t.Messages, Message{Sequence: int64(len(t.Messages) + 1), Role: "agent", Kind: kind, Text: p.Question, CreatedAt: now})
	return cloneTask(t), nil
}

func (s *MemoryStore) RespondInteraction(_ context.Context, p RespondInteractionParams) (*InteractionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(p.Agent, p.TaskID)
	if err != nil {
		return nil, err
	}
	if p.Authorization.Valid() && (t.TenantID != p.Authorization.TenantID || t.OwnerPrincipalID != p.Authorization.PrincipalID || t.AgentResourceID != p.Authorization.AgentResourceID) {
		return nil, ErrNotFound
	}
	if !p.Authorization.Valid() && t.CreatedBy != p.RespondedBy {
		return nil, ErrNotFound
	}
	key := p.RespondedBy + "\x00" + p.TaskID + "\x00" + p.InteractionID + "\x00" + p.IdempotencyKey
	if p.IdempotencyKey != "" {
		if old, ok := s.interactionKeys[key]; ok {
			if old.hash != p.RequestHash {
				return nil, ErrIdempotencyConflict
			}
			return &InteractionResult{Task: cloneTask(t), Replay: true}, nil
		}
	}
	if t.Interaction == nil || t.Interaction.ID != p.InteractionID {
		return nil, ErrNotFound
	}
	if t.Interaction.Status != InteractionPaused {
		return nil, ErrInteractionNotReady
	}
	now := s.now().UTC()
	if !t.Interaction.ExpiresAt.After(now) {
		t.Interaction.Status = InteractionExpired
		t.State = StateFailed
		if t.Interaction.Type == InteractionAuthorization {
			t.FailureCode = FailureAuthTimeout
		} else {
			t.FailureCode = FailureInputTimeout
		}
		t.Version++
		t.UpdatedAt = now
		t.CompletedAt = &now
		return nil, ErrInteractionExpired
	}
	if len(p.Response) == 0 || len(p.Response) > s.limits.MaxInteractionResponseBytes || !json.Valid(p.Response) {
		return nil, ErrInvalid
	}
	if t.Interaction.Type == InteractionAuthorization {
		var supplied struct {
			AuthorizationFlowID string `json:"authorizationFlowId"`
		}
		if json.Unmarshal(p.Response, &supplied) != nil || supplied.AuthorizationFlowID != t.Interaction.AuthorizationFlow {
			return nil, ErrAuthorizationFlow
		}
		flow, ok := s.authorizationFlows[supplied.AuthorizationFlowID]
		if !ok || !flow.complete || flow.taskID != p.TaskID || flow.interactionID != p.InteractionID || flow.createdBy != p.RespondedBy {
			return nil, ErrAuthorizationFlow
		}
		p.Response, _ = json.Marshal(map[string]string{"reference": flow.reference})
	}
	if err := validateInteractionResponse(t.Interaction, p.Response); err != nil {
		return nil, err
	}
	t.Interaction.Response = append(json.RawMessage(nil), p.Response...)
	t.Interaction.Status = InteractionAnswered
	t.Interaction.AnsweredAt = &now
	kind := "input_response"
	if t.Interaction.Type == InteractionAuthorization {
		kind = "authorization_completed"
	}
	t.Messages = append(t.Messages, Message{Sequence: int64(len(t.Messages) + 1), Role: "caller", Kind: kind, Data: append(json.RawMessage(nil), p.Response...), CreatedAt: now}, Message{Sequence: int64(len(t.Messages) + 2), Role: "platform", Kind: "continuation_instruction", Text: "Continue the task using the caller response above.", CreatedAt: now})
	t.State = StateQueued
	t.Version++
	t.UpdatedAt = now
	if p.IdempotencyKey != "" {
		s.interactionKeys[key] = idempotencyRecord{hash: p.RequestHash, taskID: t.ID}
	}
	return &InteractionResult{Task: cloneTask(t)}, nil
}

func (s *MemoryStore) CompleteAuthorizationFlow(_ context.Context, p CompleteAuthorizationFlowParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	flow, ok := s.authorizationFlows[p.FlowID]
	if !ok || flow.complete || flow.taskID != p.TaskID || flow.interactionID != p.InteractionID || flow.createdBy != p.CreatedBy || strings.TrimSpace(p.ConnectionReference) == "" {
		return ErrAuthorizationFlow
	}
	flow.complete = true
	flow.reference = strings.TrimSpace(p.ConnectionReference)
	s.authorizationFlows[p.FlowID] = flow
	return nil
}

func validateInteractionResponse(i *Interaction, raw json.RawMessage) error {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ErrInvalid
	}
	switch i.Type {
	case InteractionText:
		if _, ok := v.(string); !ok {
			return ErrInvalid
		}
	case InteractionConfirm:
		if _, ok := v.(bool); !ok {
			return ErrInvalid
		}
	case InteractionChoice:
		s, ok := v.(string)
		if !ok {
			return ErrInvalid
		}
		found := false
		for _, o := range i.Options {
			if o.ID == s {
				found = true
			}
		}
		if !found {
			return ErrInvalid
		}
	case InteractionJSON:
		if err := validateBoundedJSONSchema(i.Schema, v); err != nil {
			return err
		}
	case InteractionAuthorization:
		m, ok := v.(map[string]any)
		if !ok {
			return ErrInvalid
		}
		ref, ok := m["reference"].(string)
		if !ok || strings.TrimSpace(ref) == "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// validateBoundedJSONSchema intentionally supports only the portable edge Kyber
// promises: root type, required object fields, and primitive property types.
// Richer schemas stay agent-domain data rather than becoming a hidden validator.
func validateBoundedJSONSchema(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	var schema struct {
		Type       string   `json:"type"`
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if json.Unmarshal(raw, &schema) != nil {
		return ErrInvalid
	}
	typeOK := func(typ string, x any) bool {
		switch typ {
		case "":
			return true
		case "object":
			_, ok := x.(map[string]any)
			return ok
		case "array":
			_, ok := x.([]any)
			return ok
		case "string":
			_, ok := x.(string)
			return ok
		case "boolean":
			_, ok := x.(bool)
			return ok
		case "number":
			_, ok := x.(float64)
			return ok
		case "integer":
			n, ok := x.(float64)
			return ok && n == float64(int64(n))
		case "null":
			return x == nil
		default:
			return false
		}
	}
	if !typeOK(schema.Type, v) {
		return ErrInvalid
	}
	if len(schema.Required) > 0 || len(schema.Properties) > 0 {
		obj, ok := v.(map[string]any)
		if !ok {
			return ErrInvalid
		}
		for _, name := range schema.Required {
			if _, ok = obj[name]; !ok {
				return ErrInvalid
			}
		}
		for name, p := range schema.Properties {
			if value, ok := obj[name]; ok && !typeOK(p.Type, value) {
				return ErrInvalid
			}
		}
	}
	return nil
}

func (s *MemoryStore) Reject(_ context.Context, a AgentRef, id, attemptID, reason string) (*Task, error) {
	if attemptID == "" || strings.TrimSpace(reason) == "" || len(reason) > s.limits.MaxResponseBytes {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(a, id)
	if err != nil {
		return nil, err
	}
	if t.State != StateDispatched {
		return nil, ErrConflict
	}
	now := s.now().UTC()
	t.State = StateRejected
	t.Response = reason
	t.Version++
	t.UpdatedAt = now
	t.CompletedAt = &now
	t.Messages = append(t.Messages, Message{Sequence: int64(len(t.Messages) + 1), Role: "agent", Kind: "terminal_summary", Text: reason, CreatedAt: now})
	return cloneTask(t), nil
}

func (s *MemoryStore) Cancel(_ context.Context, p CancelParams) (*CancelResult, error) {
	if p.TaskID == "" || p.RequestedBy == "" || len([]byte(p.IdempotencyKey)) > HardMaxIdempotencyBytes {
		return nil, ErrInvalid
	}
	if len([]byte(p.Reason)) > s.limits.MaxCancelReasonBytes {
		return nil, ErrCancelReasonTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(p.Agent, p.TaskID)
	if err != nil {
		return nil, err
	}
	if p.Authorization.Valid() && (t.TenantID != p.Authorization.TenantID || t.OwnerPrincipalID != p.Authorization.PrincipalID || t.AgentResourceID != p.Authorization.AgentResourceID) {
		return nil, ErrNotFound
	}
	if !p.Authorization.Valid() && t.CreatedBy != p.RequestedBy {
		return nil, ErrNotFound
	}
	key := p.RequestedBy + "\x00" + p.Agent.Namespace + "\x00" + p.Agent.Name + "\x00" + p.IdempotencyKey
	if p.IdempotencyKey != "" {
		if prior, ok := s.cancelKeys[key]; ok {
			if prior.hash != p.RequestHash {
				return nil, ErrIdempotencyConflict
			}
			return &CancelResult{Task: cloneTask(t), Applied: prior.applied, Replay: true}, nil
		}
	}
	if t.Cancellation != nil || t.State == StateCanceled || t.State == StateCanceling {
		if p.IdempotencyKey != "" {
			s.cancelKeys[key] = cancelIdempotencyRecord{hash: p.RequestHash, taskID: t.ID}
		}
		return &CancelResult{Task: cloneTask(t), Replay: true}, nil
	}
	if t.State == StateCompleted || t.State == StateFailed || t.State == StateRejected {
		if p.IdempotencyKey != "" {
			s.cancelKeys[key] = cancelIdempotencyRecord{hash: p.RequestHash, taskID: t.ID}
		}
		return &CancelResult{Task: cloneTask(t)}, nil
	}
	now := s.now().UTC()
	deadline := now.Add(s.limits.DefaultCancelDeadline)
	if deadline.After(t.DeadlineAt) {
		deadline = t.DeadlineAt
	}
	t.Cancellation = &Cancellation{RequestedAt: now, RequestedBy: p.RequestedBy, Reason: p.Reason, DeadlineAt: deadline, Status: "requested", Scope: "future_task_work"}
	if t.State == StateQueued || t.State == StateInputRequired || t.State == StateAuthRequired {
		t.State = StateCanceled
		t.CompletedAt = &now
	} else {
		t.State = StateCanceling
	}
	t.Version++
	t.UpdatedAt = now
	if p.IdempotencyKey != "" {
		s.cancelKeys[key] = cancelIdempotencyRecord{hash: p.RequestHash, taskID: t.ID, applied: true}
	}
	return &CancelResult{Task: cloneTask(t), Applied: true}, nil
}

func (s *MemoryStore) GetControl(_ context.Context, a AgentRef, id, attemptID string) (*TaskControl, error) {
	if attemptID == "" {
		return nil, ErrInvalidAttempt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(a, id)
	if err != nil {
		return nil, err
	}
	if t.Cancellation == nil {
		return &TaskControl{}, nil
	}
	return &TaskControl{CancelRequested: true, Reason: t.Cancellation.Reason, RequestedAt: t.Cancellation.RequestedAt}, nil
}

func (s *MemoryStore) AcknowledgeCancel(_ context.Context, a AgentRef, id, attemptID, acknowledgmentID, note string) (*Task, bool, error) {
	if attemptID == "" || acknowledgmentID == "" {
		return nil, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(a, id)
	if err != nil {
		return nil, false, err
	}
	if prior, ok := s.cancelAcks[id]; ok {
		if prior != acknowledgmentID {
			return nil, false, ErrConflict
		}
		return cloneTask(t), true, nil
	}
	if t.State != StateCanceling || t.Cancellation == nil {
		return nil, false, ErrConflict
	}
	now := s.now().UTC()
	t.State = StateCanceled
	t.Version++
	t.UpdatedAt = now
	t.CompletedAt = &now
	t.Cancellation.AcknowledgedAt = &now
	t.Cancellation.AckSource = "agent"
	t.Cancellation.Status = "acknowledged"
	s.cancelAcks[id] = acknowledgmentID
	return cloneTask(t), false, nil
}

func (s *MemoryStore) ReportProgress(_ context.Context, a AgentRef, id, attemptID string, u ProgressUpdate) (*Progress, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(a, id)
	if err != nil {
		return nil, false, err
	}
	if err := validateProgress(s.limits, attemptID, u); err != nil {
		return nil, false, err
	}
	fingerprint := progressDigest(u)
	if prior, ok := s.updates[id][u.UpdateID]; ok {
		if prior != fingerprint {
			return nil, false, ErrUpdateConflict
		}
		p := *t.Progress
		return &p, true, nil
	}
	if t.State != StateDispatched {
		return nil, false, ErrConflict
	}
	if len(s.updates[id]) >= s.limits.MaxProgressUpdates {
		return nil, false, ErrUpdateLimit
	}
	if s.updates[id] == nil {
		s.updates[id] = map[string]string{}
	}
	now := s.now().UTC()
	t.Progress = &Progress{Message: u.Message, Percent: cloneInt(u.Percent), UpdatedAt: now}
	t.Version++
	t.UpdatedAt = now
	s.updates[id][u.UpdateID] = fingerprint
	p := *t.Progress
	p.Percent = cloneInt(p.Percent)
	return &p, false, nil
}

func (s *MemoryStore) PublishResult(_ context.Context, a AgentRef, id, attemptID string, r Result) (*Result, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.locked(a, id)
	if err != nil {
		return nil, false, err
	}
	if err := validateResult(s.limits, attemptID, r); err != nil {
		return nil, false, err
	}
	digest := resultDigest(r)
	for i := range t.Results {
		if t.Results[i].ID == r.ID {
			if t.Results[i].ContentDigest != digest {
				return nil, false, ErrResultConflict
			}
			out := cloneResults(t.Results[i : i+1])[0]
			return &out, true, nil
		}
		if t.Results[i].Name == r.Name {
			return nil, false, ErrResultConflict
		}
	}
	if t.State != StateDispatched {
		return nil, false, ErrConflict
	}
	if len(t.Results) >= s.limits.MaxResults {
		return nil, false, ErrResultLimit
	}
	r.ContentDigest = digest
	r.CreatedAt = s.now().UTC()
	t.Results = append(t.Results, cloneResults([]Result{r})[0])
	t.Version++
	t.UpdatedAt = r.CreatedAt
	out := cloneResults([]Result{r})[0]
	return &out, false, nil
}

func cloneInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
func progressDigest(u ProgressUpdate) string {
	b, _ := json.Marshal(struct {
		Message string
		Percent *int
	}{u.Message, u.Percent})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func resultDigest(r Result) string {
	r.Parts = cloneResults([]Result{r})[0].Parts
	r.ContentDigest = ""
	r.CreatedAt = time.Time{}
	for i := range r.Parts {
		if r.Parts[i].File != nil {
			file := *r.Parts[i].File
			file.ObjectID = ""
			file.ScanStatus = ""
			r.Parts[i].File = &file
		}
	}
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func validateProgress(l Limits, attempt string, u ProgressUpdate) error {
	if attempt == "" {
		return ErrInvalidAttempt
	}
	if u.UpdateID == "" || strings.TrimSpace(u.Message) == "" {
		return ErrInvalid
	}
	if len([]byte(u.Message)) > l.MaxProgressBytes {
		return ErrProgressTooLarge
	}
	if u.Percent != nil && (*u.Percent < 0 || *u.Percent > 100) {
		return ErrInvalid
	}
	return nil
}
func validateResult(l Limits, attempt string, r Result) error {
	if attempt == "" {
		return ErrInvalidAttempt
	}
	if r.ID == "" || strings.TrimSpace(r.Name) == "" || len(r.Parts) == 0 {
		return ErrInvalid
	}
	if len([]byte(r.Name)) > l.MaxResultNameBytes || len([]byte(r.Description)) > l.MaxDescriptionBytes || len(r.Parts) > l.MaxResultParts {
		return ErrResultTooLarge
	}
	var files int64
	seen := map[string]bool{}
	for _, p := range r.Parts {
		if p.ID == "" || seen[p.ID] {
			return ErrInvalid
		}
		seen[p.ID] = true
		switch p.Kind {
		case PartText:
			if p.JSON != nil || p.File != nil || int64(len([]byte(p.Text))) > l.MaxTextPartBytes {
				return ErrResultTooLarge
			}
		case PartJSON:
			if p.Text != "" || p.File != nil || len(p.JSON) == 0 || !json.Valid(p.JSON) {
				return ErrInvalid
			}
			if int64(len(p.JSON)) > l.MaxJSONPartBytes {
				return ErrResultTooLarge
			}
		case PartFile:
			if p.Text != "" || p.JSON != nil || p.File == nil || p.File.ObjectID == "" || p.File.Filename == "" || p.File.MediaType == "" || p.File.SHA256 == "" || p.File.SizeBytes < 0 {
				return ErrInvalid
			}
			if len([]byte(p.File.Filename)) > l.MaxFilenameBytes || p.File.SizeBytes > l.MaxFileBytes {
				return ErrResultTooLarge
			}
			files += p.File.SizeBytes
		default:
			return ErrInvalid
		}
	}
	if files > l.MaxTaskFileBytes {
		return ErrResultTooLarge
	}
	return nil
}
