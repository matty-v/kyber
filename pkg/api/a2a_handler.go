package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/matty-v/kyber/pkg/taskstore"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

// a2aTaskHandler is deliberately only a translator. Kyber's native task store
// remains the owner of persistence, authorization, execution and replay.
type a2aTaskHandler struct {
	server *Server
	agent  string
}

func (h *a2aTaskHandler) ref() taskstore.AgentRef {
	return taskstore.AgentRef{Namespace: h.server.Namespace, Name: h.agent}
}

func (h *a2aTaskHandler) authorize(ctx context.Context, scope Scope) (taskstore.AuthorizationContext, error) {
	caller := callerFrom(ctx)
	if caller == nil || caller.PrincipalID == "" || caller.TenantID == "" {
		return taskstore.AuthorizationContext{}, a2a.ErrUnauthenticated
	}
	if !caller.Scopes.Has(scope) || !caller.AgentResources.Has(h.server.Namespace+"/"+h.agent) {
		return taskstore.AuthorizationContext{}, a2a.ErrUnauthorized
	}
	return taskstore.AuthorizationContext{TenantID: caller.TenantID, PrincipalID: caller.PrincipalID, AgentResourceID: h.server.Namespace + "/" + h.agent}, nil
}

func tenantMatches(ctx context.Context, tenant string) bool {
	caller := callerFrom(ctx)
	return tenant == "" || (caller != nil && tenant == caller.TenantID)
}

func (h *a2aTaskHandler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	if !tenantMatches(ctx, req.Tenant) {
		return nil, a2a.ErrTaskNotFound
	}
	auth, err := h.authorize(ctx, ScopeTasksRead)
	if err != nil {
		return nil, err
	}
	t, err := h.server.TaskStore.GetAuthorized(ctx, h.ref(), string(req.ID), auth)
	if err != nil {
		return nil, a2aStoreError(err)
	}
	return nativeA2ATask(t, req.HistoryLength, true), nil
}

func (h *a2aTaskHandler) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	if !tenantMatches(ctx, req.Tenant) {
		return nil, a2a.ErrUnauthorized
	}
	auth, err := h.authorize(ctx, ScopeTasksList)
	if err != nil {
		return nil, err
	}
	state, err := a2aNativeState(req.Status)
	if err != nil {
		return nil, err
	}
	page, err := h.server.TaskStore.List(ctx, taskstore.ListParams{Agent: h.ref(), Authorization: auth, State: state, Limit: req.PageSize, Cursor: req.PageToken})
	if err != nil {
		return nil, a2aStoreError(err)
	}
	out := &a2a.ListTasksResponse{Tasks: make([]*a2a.Task, 0, len(page.Tasks)), PageSize: req.PageSize, NextPageToken: page.NextCursor}
	for _, t := range page.Tasks {
		if req.ContextID != "" && t.Correlation != req.ContextID {
			continue
		}
		out.Tasks = append(out.Tasks, nativeA2ATask(t, req.HistoryLength, req.IncludeArtifacts))
	}
	out.TotalSize = len(out.Tasks)
	return out, nil
}

func (h *a2aTaskHandler) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	if !tenantMatches(ctx, req.Tenant) {
		return nil, a2a.ErrTaskNotFound
	}
	if len(req.Metadata) != 0 {
		return nil, a2a.ErrInvalidParams
	}
	auth, err := h.authorize(ctx, ScopeTasksCancel)
	if err != nil {
		return nil, err
	}
	caller := callerFrom(ctx)
	hash := sha256.Sum256([]byte(req.ID))
	result, err := h.server.TaskStore.Cancel(ctx, taskstore.CancelParams{Agent: h.ref(), TaskID: string(req.ID), RequestedBy: caller.PrincipalID, IdempotencyKey: "a2a-cancel-" + string(req.ID), RequestHash: hex.EncodeToString(hash[:]), Authorization: auth})
	if err != nil {
		if errors.Is(err, taskstore.ErrConflict) {
			return nil, a2a.ErrTaskNotCancelable
		}
		return nil, a2aStoreError(err)
	}
	return nativeA2ATask(result.Task, nil, true), nil
}

func (h *a2aTaskHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	t, err := h.send(ctx, req)
	if err != nil {
		return nil, err
	}
	if req.Config != nil && req.Config.ReturnImmediately {
		return nativeA2ATask(t, req.Config.HistoryLength, true), nil
	}
	caller := callerFrom(ctx)
	auth := taskstore.AuthorizationContext{TenantID: caller.TenantID, PrincipalID: caller.PrincipalID, AgentResourceID: h.server.Namespace + "/" + h.agent}
	for !terminalOrPaused(t.State) {
		select {
		case <-ctx.Done():
			return nativeA2ATask(t, historyLength(req), true), nil
		case <-time.After(200 * time.Millisecond):
		}
		t, err = h.server.TaskStore.GetAuthorized(ctx, h.ref(), t.ID, auth)
		if err != nil {
			return nil, a2aStoreError(err)
		}
	}
	return nativeA2ATask(t, historyLength(req), true), nil
}

func (h *a2aTaskHandler) send(ctx context.Context, req *a2a.SendMessageRequest) (*taskstore.Task, error) {
	if req == nil || req.Message == nil || req.Message.Role != a2a.MessageRoleUser || req.Message.ID == "" || len(req.Metadata) != 0 || len(req.Message.Metadata) != 0 || len(req.Message.Extensions) != 0 || len(req.Message.ReferenceTasks) != 0 {
		return nil, a2a.ErrInvalidParams
	}
	if !tenantMatches(ctx, req.Tenant) {
		return nil, a2a.ErrUnauthorized
	}
	if req.Config != nil && req.Config.PushConfig != nil {
		return nil, a2a.ErrPushNotificationNotSupported
	}
	if err := h.ensureAgentAcceptsTasks(ctx); err != nil {
		return nil, err
	}
	prompt, err := a2aPrompt(req.Message.Parts)
	if err != nil {
		return nil, err
	}
	if req.Message.TaskID != "" {
		return h.continueTask(ctx, req, prompt)
	}
	auth, err := h.authorize(ctx, ScopeTasksCreate)
	if err != nil {
		return nil, err
	}
	contextID := req.Message.ContextID
	if contextID == "" {
		contextID = a2a.NewContextID()
	}
	hash := sha256.Sum256([]byte(prompt + "\x00" + contextID))
	caller := callerFrom(ctx)
	for i := 0; i < 3; i++ {
		id, idErr := newAgentTaskID()
		if idErr != nil {
			return nil, a2a.ErrInternalError
		}
		result, createErr := h.server.TaskStore.Create(ctx, taskstore.CreateParams{ID: id, Agent: h.ref(), CreatedBy: caller.PrincipalID, Authorization: auth, Prompt: prompt, Correlation: contextID, IdempotencyKey: req.Message.ID, RequestHash: hex.EncodeToString(hash[:])})
		if errors.Is(createErr, taskstore.ErrConflict) {
			continue
		}
		if createErr != nil {
			return nil, a2aStoreError(createErr)
		}
		return result.Task, nil
	}
	return nil, a2a.ErrInternalError
}

func (h *a2aTaskHandler) ensureAgentAcceptsTasks(ctx context.Context) error {
	if h.server.K8sClient == nil {
		return a2a.ErrInternalError
	}
	agent := &kyberv1.Agent{}
	err := h.server.K8sClient.Get(ctx, types.NamespacedName{Namespace: h.server.Namespace, Name: h.agent}, agent)
	if k8serrors.IsNotFound(err) {
		return a2a.ErrTaskNotFound
	}
	if err != nil {
		return a2a.ErrInternalError
	}
	if !agent.Spec.RequestReplyEnabled {
		return a2a.NewError(a2a.ErrUnsupportedOperation, "agent does not accept durable tasks")
	}
	return nil
}

func (h *a2aTaskHandler) continueTask(ctx context.Context, req *a2a.SendMessageRequest, prompt string) (*taskstore.Task, error) {
	auth, err := h.authorize(ctx, ScopeTasksContinue)
	if err != nil {
		return nil, err
	}
	t, err := h.server.TaskStore.GetAuthorized(ctx, h.ref(), string(req.Message.TaskID), auth)
	if err != nil {
		return nil, a2aStoreError(err)
	}
	if t.Interaction == nil || t.Interaction.Status != taskstore.InteractionPaused {
		return nil, a2a.ErrInvalidParams
	}
	response, _ := json.Marshal(prompt)
	hash := sha256.Sum256(response)
	result, err := h.server.TaskStore.RespondInteraction(ctx, taskstore.RespondInteractionParams{Agent: h.ref(), TaskID: t.ID, InteractionID: t.Interaction.ID, RespondedBy: callerFrom(ctx).PrincipalID, IdempotencyKey: req.Message.ID, RequestHash: hex.EncodeToString(hash[:]), Authorization: auth, Response: response})
	if err != nil {
		return nil, a2aStoreError(err)
	}
	return result.Task, nil
}

func (h *a2aTaskHandler) SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	if !tenantMatches(ctx, req.Tenant) {
		return func(yield func(a2a.Event, error) bool) { yield(nil, a2a.ErrTaskNotFound) }
	}
	return h.streamTask(ctx, string(req.ID), nil)
}

func (h *a2aTaskHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		t, err := h.send(ctx, req)
		if err != nil {
			yield(nil, err)
			return
		}
		for event, streamErr := range h.streamTask(ctx, t.ID, historyLength(req)) {
			if !yield(event, streamErr) {
				return
			}
		}
	}
}

func (h *a2aTaskHandler) streamTask(ctx context.Context, id string, history *int) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		auth, err := h.authorize(ctx, ScopeTaskEventsRead)
		if err != nil {
			yield(nil, err)
			return
		}
		events, ok := h.server.TaskStore.(taskstore.EventStore)
		if !ok {
			yield(nil, a2a.ErrUnsupportedOperation)
			return
		}
		t, sequence, err := events.EventSnapshot(ctx, h.ref(), id, auth)
		if err != nil {
			yield(nil, a2aStoreError(err))
			return
		}
		if !yield(nativeA2ATask(t, history, true), nil) || terminalOrPaused(t.State) {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
			page, readErr := events.ReadEvents(ctx, taskstore.EventReadParams{Agent: h.ref(), TaskID: id, Authorization: auth, AfterSequence: sequence, Limit: 100, Resume: true})
			if readErr != nil {
				yield(nil, a2aStoreError(readErr))
				return
			}
			for _, nativeEvent := range page.Events {
				sequence = nativeEvent.Sequence
				latest, getErr := h.server.TaskStore.GetAuthorized(ctx, h.ref(), id, auth)
				if getErr != nil {
					yield(nil, a2aStoreError(getErr))
					return
				}
				var event a2a.Event = &a2a.TaskStatusUpdateEvent{TaskID: a2a.TaskID(id), ContextID: latest.Correlation, Status: nativeA2AStatus(latest)}
				if nativeEvent.Type == taskstore.EventTaskResultAdded && len(latest.Results) > 0 {
					event = &a2a.TaskArtifactUpdateEvent{TaskID: a2a.TaskID(id), ContextID: latest.Correlation, Artifact: nativeA2AArtifact(latest, latest.Results[len(latest.Results)-1]), LastChunk: true}
				}
				if !yield(event, nil) {
					return
				}
				t = latest
			}
			if page.Terminal || terminalOrPaused(t.State) {
				return
			}
		}
	}
}

func (h *a2aTaskHandler) GetTaskPushConfig(context.Context, *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}
func (h *a2aTaskHandler) ListTaskPushConfigs(context.Context, *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}
func (h *a2aTaskHandler) CreateTaskPushConfig(context.Context, *a2a.PushConfig) (*a2a.PushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}
func (h *a2aTaskHandler) DeleteTaskPushConfig(context.Context, *a2a.DeleteTaskPushConfigRequest) error {
	return a2a.ErrPushNotificationNotSupported
}
func (h *a2aTaskHandler) GetExtendedAgentCard(context.Context, *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	return nil, a2a.ErrExtendedCardNotConfigured
}

func historyLength(req *a2a.SendMessageRequest) *int {
	if req.Config == nil {
		return nil
	}
	return req.Config.HistoryLength
}

func a2aPrompt(parts a2a.ContentParts) (string, error) {
	if len(parts) == 0 {
		return "", a2a.ErrInvalidParams
	}
	var out []string
	for _, part := range parts {
		if part == nil || len(part.Metadata) != 0 {
			return "", a2a.ErrInvalidParams
		}
		switch value := part.Content.(type) {
		case a2a.Text:
			out = append(out, string(value))
		case a2a.Data:
			encoded, err := json.Marshal(value.Value)
			if err != nil {
				return "", a2a.ErrInvalidParams
			}
			out = append(out, string(encoded))
		default:
			return "", a2a.ErrUnsupportedContentType
		}
	}
	prompt := strings.TrimSpace(strings.Join(out, "\n"))
	if prompt == "" || len(prompt) > taskstore.HardMaxPromptBytes {
		return "", a2a.ErrInvalidParams
	}
	return prompt, nil
}

func nativeA2ATask(t *taskstore.Task, historyLength *int, includeArtifacts bool) *a2a.Task {
	out := &a2a.Task{ID: a2a.TaskID(t.ID), ContextID: t.Correlation, Status: nativeA2AStatus(t)}
	if out.ContextID == "" {
		out.ContextID = t.ID
	}
	if includeArtifacts {
		for _, result := range t.Results {
			out.Artifacts = append(out.Artifacts, nativeA2AArtifact(t, result))
		}
	}
	start := 0
	if historyLength == nil && len(t.Messages) > 20 {
		start = len(t.Messages) - 20
	} else if historyLength != nil {
		if *historyLength <= 0 {
			return out
		}
		if len(t.Messages) > *historyLength {
			start = len(t.Messages) - *historyLength
		}
	}
	for _, msg := range t.Messages[start:] {
		role := a2a.MessageRoleAgent
		if msg.Role == "caller" {
			role = a2a.MessageRoleUser
		}
		parts := a2a.ContentParts{}
		if msg.Text != "" {
			parts = append(parts, a2a.NewTextPart(msg.Text))
		} else if len(msg.Data) > 0 {
			var data any
			if json.Unmarshal(msg.Data, &data) == nil {
				parts = append(parts, a2a.NewDataPart(data))
			}
		}
		out.History = append(out.History, &a2a.Message{ID: fmt.Sprintf("%s-%d", t.ID, msg.Sequence), TaskID: a2a.TaskID(t.ID), ContextID: out.ContextID, Role: role, Parts: parts})
	}
	return out
}

func nativeA2AStatus(t *taskstore.Task) a2a.TaskStatus {
	timestamp := t.UpdatedAt
	status := a2a.TaskStatus{State: nativeA2AState(t.State), Timestamp: &timestamp}
	text := ""
	if t.Progress != nil {
		text = t.Progress.Message
	}
	if t.Interaction != nil && (t.State == taskstore.StateInputRequired || t.State == taskstore.StateAuthRequired) {
		text = t.Interaction.Question
	}
	if t.Response != "" {
		text = t.Response
	}
	contextID := t.Correlation
	if contextID == "" {
		contextID = t.ID
	}
	if text != "" {
		status.Message = &a2a.Message{ID: fmt.Sprintf("%s-status-%d", t.ID, t.Version), TaskID: a2a.TaskID(t.ID), ContextID: contextID, Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{a2a.NewTextPart(text)}}
	}
	return status
}

func nativeA2AArtifact(task *taskstore.Task, result taskstore.Result) *a2a.Artifact {
	out := &a2a.Artifact{ID: a2a.ArtifactID(result.ID), Name: result.Name, Description: result.Description}
	for ordinal, part := range result.Parts {
		switch part.Kind {
		case taskstore.PartText:
			out.Parts = append(out.Parts, a2a.NewTextPart(part.Text))
		case taskstore.PartJSON:
			var data any
			if json.Unmarshal(part.JSON, &data) == nil {
				out.Parts = append(out.Parts, a2a.NewDataPart(data))
			}
		case taskstore.PartFile:
			if part.File != nil {
				out.Parts = append(out.Parts, &a2a.Part{Content: a2a.URL(fmt.Sprintf("/api/v1/agents/%s/tasks/%s/results/%s/parts/%d/content", task.AgentName, task.ID, result.ID, ordinal)), Filename: part.File.Filename, MediaType: part.File.MediaType})
			}
		}
	}
	return out
}

func nativeA2AState(state taskstore.State) a2a.TaskState {
	switch state {
	case taskstore.StateQueued:
		return a2a.TaskStateSubmitted
	case taskstore.StateDispatched, taskstore.StateCanceling:
		return a2a.TaskStateWorking
	case taskstore.StateInputRequired:
		return a2a.TaskStateInputRequired
	case taskstore.StateAuthRequired:
		return a2a.TaskStateAuthRequired
	case taskstore.StateCanceled:
		return a2a.TaskStateCanceled
	case taskstore.StateCompleted:
		return a2a.TaskStateCompleted
	case taskstore.StateFailed:
		return a2a.TaskStateFailed
	case taskstore.StateRejected:
		return a2a.TaskStateRejected
	default:
		return a2a.TaskStateUnspecified
	}
}

func a2aNativeState(state a2a.TaskState) (taskstore.State, error) {
	switch state {
	case a2a.TaskStateUnspecified:
		return "", nil
	case a2a.TaskStateSubmitted:
		return taskstore.StateQueued, nil
	case a2a.TaskStateWorking:
		return taskstore.StateDispatched, nil
	case a2a.TaskStateInputRequired:
		return taskstore.StateInputRequired, nil
	case a2a.TaskStateAuthRequired:
		return taskstore.StateAuthRequired, nil
	case a2a.TaskStateCanceled:
		return taskstore.StateCanceled, nil
	case a2a.TaskStateCompleted:
		return taskstore.StateCompleted, nil
	case a2a.TaskStateFailed:
		return taskstore.StateFailed, nil
	case a2a.TaskStateRejected:
		return taskstore.StateRejected, nil
	default:
		return "", a2a.ErrInvalidParams
	}
}

func terminalOrPaused(state taskstore.State) bool {
	return state == taskstore.StateCompleted || state == taskstore.StateFailed || state == taskstore.StateRejected || state == taskstore.StateCanceled || state == taskstore.StateInputRequired || state == taskstore.StateAuthRequired
}
func a2aStoreError(err error) error {
	switch {
	case errors.Is(err, taskstore.ErrNotFound):
		return a2a.ErrTaskNotFound
	case errors.Is(err, taskstore.ErrInvalid), errors.Is(err, taskstore.ErrInvalidCursor), errors.Is(err, taskstore.ErrIdempotencyConflict), errors.Is(err, taskstore.ErrInteractionNotReady), errors.Is(err, taskstore.ErrInteractionExpired):
		return a2a.ErrInvalidParams
	case errors.Is(err, taskstore.ErrPromptTooLarge), errors.Is(err, taskstore.ErrCorrelationTooLarge), errors.Is(err, taskstore.ErrResponseTooLarge), errors.Is(err, taskstore.ErrIdempotencyTooLarge):
		return a2a.ErrInvalidParams
	default:
		return a2a.ErrInternalError
	}
}
