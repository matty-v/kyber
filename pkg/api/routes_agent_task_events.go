package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/matty-v/kyber/pkg/taskstore"
)

const (
	taskEventReplayPage = 100
	taskEventPoll       = time.Second
	taskEventHeartbeat  = 20 * time.Second
	taskEventMaxAge     = 5 * time.Minute
	taskEventWriteLimit = 10 * time.Second
)

func (s *Server) streamAgentTaskEvents(w http.ResponseWriter, r *http.Request, agentName, taskID string) {
	events, ok := s.TaskStore.(taskstore.EventStore)
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "task_events_unavailable", errTaskEventStreamUnavailable.Error())
		return
	}
	if accept := r.Header.Get("Accept"); accept != "" && !acceptsTaskEventStream(accept) {
		writeJSONError(w, http.StatusNotAcceptable, "event_stream_required", "Accept must include text/event-stream")
		return
	}
	auth, ok := s.taskAuthorizationContext(w, r, agentName)
	if !ok {
		return
	}
	release, retryAfter, ok := s.acquireTaskEventStream(r, taskID)
	if !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
		writeJSONError(w, http.StatusTooManyRequests, "task_event_capacity", "task event stream capacity exceeded")
		return
	}
	defer release()
	headerCursor := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	queryCursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if headerCursor != "" && queryCursor != "" && headerCursor != queryCursor {
		writeJSONError(w, http.StatusBadRequest, "invalid_cursor", "cursor values disagree")
		return
	}
	cursor := headerCursor
	if cursor == "" {
		cursor = queryCursor
	}
	last := int64(0)
	var err error
	if cursor != "" {
		last, err = s.decodeTaskEventCursor(r, agentName, taskID, cursor)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_cursor", "invalid event cursor")
			return
		}
	}
	a := taskstore.AgentRef{Namespace: s.Namespace, Name: agentName}
	initial, err := events.ReadEvents(r.Context(), taskstore.EventReadParams{Agent: a, TaskID: taskID, Authorization: auth, AfterSequence: last, Limit: taskEventReplayPage, Resume: cursor != ""})
	if errors.Is(err, taskstore.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "not_found", "task not found")
		return
	}
	if errors.Is(err, taskstore.ErrEventCursorExpired) {
		current, _ := s.encodeTaskEventCursor(r, agentName, taskID, initialHighWater(initial))
		writeJSON(w, http.StatusGone, map[string]any{"code": "event_cursor_expired", "taskId": taskID, "snapshotRequired": true, "currentCursor": current})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "task_store_error", "failed to read task events")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming_unavailable", "streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "private, no-store, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	_, _ = fmt.Fprint(w, ": kyber-task-events-v1\n\n")
	flusher.Flush()

	poll := time.NewTicker(taskEventPoll)
	heartbeat := time.NewTicker(taskEventHeartbeat)
	maxAge := time.NewTimer(taskEventMaxAge)
	defer poll.Stop()
	defer heartbeat.Stop()
	defer maxAge.Stop()
	page := initial
	for {
		for _, event := range page.Events {
			encoded, marshalErr := json.Marshal(event)
			if marshalErr != nil {
				return
			}
			id, cursorErr := s.encodeTaskEventCursor(r, agentName, taskID, event.Sequence)
			if cursorErr != nil {
				return
			}
			controller := http.NewResponseController(w)
			_ = controller.SetWriteDeadline(time.Now().Add(taskEventWriteLimit))
			if _, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", id, event.Type, encoded); err != nil {
				return
			}
			flusher.Flush()
			_ = controller.SetWriteDeadline(time.Time{})
			last = event.Sequence
		}
		if len(page.Events) == taskEventReplayPage {
			page, err = events.ReadEvents(r.Context(), taskstore.EventReadParams{Agent: a, TaskID: taskID, Authorization: auth, AfterSequence: last, Limit: taskEventReplayPage, Resume: true})
			if err != nil {
				return
			}
			continue
		}
		if page.Terminal && last >= page.HighWater {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-maxAge.C:
			return
		case <-heartbeat.C:
			if !s.taskEventCallerStillAuthorized(r, agentName) {
				return
			}
			if _, err = fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
			page, err = events.ReadEvents(r.Context(), taskstore.EventReadParams{Agent: a, TaskID: taskID, Authorization: auth, AfterSequence: last, Limit: taskEventReplayPage, Resume: true})
			if err != nil {
				return
			}
		}
	}
}

func acceptsTaskEventStream(value string) bool {
	for _, item := range strings.Split(value, ",") {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err == nil && (mediaType == "text/event-stream" || mediaType == "*/*") {
			return true
		}
	}
	return false
}

func (s *Server) acquireTaskEventStream(r *http.Request, taskID string) (func(), int, bool) {
	caller := callerFrom(r.Context())
	if caller == nil {
		return func() {}, 1, false
	}
	now := time.Now()
	user := caller.TenantID + "\x00" + caller.PrincipalID
	task := user + "\x00" + taskID
	s.taskEventMu.Lock()
	defer s.taskEventMu.Unlock()
	if s.taskEventByUser == nil {
		s.taskEventByUser = map[string]int{}
		s.taskEventByTask = map[string]int{}
		s.taskEventByTeam = map[string]int{}
		s.taskEventRecent = map[string][]time.Time{}
	}
	recent := s.taskEventRecent[user][:0]
	for _, at := range s.taskEventRecent[user] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	s.taskEventRecent[user] = recent
	if s.taskEventTotal >= 100 || s.taskEventByTeam[caller.TenantID] >= 50 || s.taskEventByUser[user] >= 10 || s.taskEventByTask[task] >= 4 || len(recent) >= 30 {
		return func() {}, 5, false
	}
	s.taskEventTotal++
	s.taskEventByTeam[caller.TenantID]++
	s.taskEventByUser[user]++
	s.taskEventByTask[task]++
	s.taskEventRecent[user] = append(recent, now)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.taskEventMu.Lock()
			defer s.taskEventMu.Unlock()
			s.taskEventTotal--
			s.taskEventByTeam[caller.TenantID]--
			s.taskEventByUser[user]--
			s.taskEventByTask[task]--
		})
	}, 0, true
}

func (s *Server) taskEventCallerStillAuthorized(r *http.Request, agentName string) bool {
	before := callerFrom(r.Context())
	if before == nil || s.auth == nil {
		return false
	}
	after, err := s.auth.Authenticate(r)
	return err == nil && after != nil && before.PrincipalID == after.PrincipalID && before.TenantID == after.TenantID && before.CredentialID == after.CredentialID && before.CredentialGeneration == after.CredentialGeneration && (after.Scopes.Has(ScopeTaskEventsRead) || after.Scopes.Has(ScopeRequestsRead)) && after.AgentResources.Has(s.Namespace+"/"+agentName)
}

func initialHighWater(page *taskstore.EventPage) int64 {
	if page == nil {
		return 0
	}
	return page.HighWater
}
