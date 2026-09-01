package taskstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func registeredAuthorizationKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "github-app", "claude-oauth", "codex-oauth":
		return true
	default:
		return false
	}
}

func authorizationFlowID(interactionID string) string {
	return "authflow_" + strings.TrimPrefix(interactionID, "interaction_")
}

func loadConversation(ctx context.Context, q queryer, t *Task) error {
	rows, err := q.QueryContext(ctx, `SELECT sequence,role,kind,text_value,data,created_at FROM agent_task_messages WHERE task_id=$1 ORDER BY sequence`, t.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m Message
		var data sql.NullString
		if err = rows.Scan(&m.Sequence, &m.Role, &m.Kind, &m.Text, &data, &m.CreatedAt); err != nil {
			return err
		}
		if data.Valid {
			m.Data = json.RawMessage(data.String)
		}
		t.Messages = append(t.Messages, m)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	var i Interaction
	var opts, schema, response sql.NullString
	var answered sql.NullTime
	err = q.QueryRowContext(ctx, `SELECT id,attempt_id,type,status,question,options,schema,authorization_flow,response,created_at,expires_at,answered_at FROM agent_task_interactions WHERE task_id=$1 ORDER BY created_at DESC LIMIT 1`, t.ID).Scan(&i.ID, &i.AttemptID, &i.Type, &i.Status, &i.Question, &opts, &schema, &i.AuthorizationFlow, &response, &i.CreatedAt, &i.ExpiresAt, &answered)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if opts.Valid {
		_ = json.Unmarshal([]byte(opts.String), &i.Options)
	}
	if schema.Valid {
		i.Schema = json.RawMessage(schema.String)
	}
	if response.Valid {
		i.Response = json.RawMessage(response.String)
	}
	if answered.Valid {
		v := answered.Time.UTC()
		i.AnsweredAt = &v
	}
	t.Interaction = &i
	return nil
}

func validateInteractionRequest(l Limits, p RequestInteractionParams) error {
	if p.TaskID == "" || p.AttemptID == "" || p.InteractionID == "" || strings.TrimSpace(p.Question) == "" || len([]byte(p.Question)) > l.MaxInteractionQuestionBytes || len(p.Options) > l.MaxInteractionOptions {
		return ErrInvalid
	}
	switch p.Type {
	case InteractionText, InteractionConfirm, InteractionJSON:
	case InteractionChoice:
		if len(p.Options) == 0 {
			return ErrInvalid
		}
	case InteractionAuthorization:
		if !registeredAuthorizationKind(p.AuthorizationFlow) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, o := range p.Options {
		if o.ID == "" || o.Label == "" || len(o.ID) > 256 || len(o.Label) > 1024 || seen[o.ID] {
			return ErrInvalid
		}
		seen[o.ID] = true
	}
	if len(p.Schema) > 0 && !json.Valid(p.Schema) {
		return ErrInvalid
	}
	return nil
}

func (s *PostgresStore) RequestInteraction(ctx context.Context, p RequestInteractionParams) (*Task, error) {
	if err := validateInteractionRequest(s.limits, p); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var state State
	var createdBy, currentAttempt, status string
	var deadline time.Time
	err = tx.QueryRowContext(ctx, `SELECT t.state,t.created_by,t.deadline_at,d.attempt_token,d.status FROM agent_tasks t JOIN agent_task_dispatches d ON d.task_id=t.id WHERE t.id=$1 AND t.agent_namespace=$2 AND t.agent_name=$3 FOR UPDATE OF t,d`, p.TaskID, p.Agent.Namespace, p.Agent.Name).Scan(&state, &createdBy, &deadline, &currentAttempt, &status)
	_ = createdBy
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if state != StateDispatched || currentAttempt != p.AttemptID || status != "delivered" {
		return nil, ErrInvalidAttempt
	}
	var count, messages int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_task_interactions WHERE task_id=$1`, p.TaskID).Scan(&count); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_task_messages WHERE task_id=$1`, p.TaskID).Scan(&messages); err != nil {
		return nil, err
	}
	if count >= s.limits.MaxInteractions || messages+1 >= s.limits.MaxMessages {
		return nil, ErrInteractionLimit
	}
	expiry := p.ExpiresIn
	if expiry <= 0 {
		expiry = s.limits.DefaultInteractionExpiry
	}
	if expiry > s.limits.MaxInteractionExpiry {
		expiry = s.limits.MaxInteractionExpiry
	}
	expires := time.Now().UTC().Add(expiry)
	if expires.After(deadline) {
		expires = deadline
	}
	if p.Type == InteractionAuthorization {
		flowID := authorizationFlowID(p.InteractionID)
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_authorization_flows(id,task_id,interaction_id,created_by,connection_kind,status,created_at,expires_at) VALUES($1,$2,$3,$4,$5,'pending',clock_timestamp(),$6)`, flowID, p.TaskID, p.InteractionID, createdBy, strings.TrimSpace(p.AuthorizationFlow), expires); err != nil {
			return nil, ErrConflict
		}
		p.AuthorizationFlow = flowID
	}
	var options, schema any
	if len(p.Options) > 0 {
		b, _ := json.Marshal(p.Options)
		options = string(b)
	}
	if len(p.Schema) > 0 {
		schema = string(p.Schema)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_task_interactions(id,task_id,attempt_id,type,status,question,options,schema,authorization_flow,created_at,expires_at) VALUES($1,$2,$3,$4,'paused',$5,$6,$7,$8,clock_timestamp(),$9)`, p.InteractionID, p.TaskID, p.AttemptID, p.Type, p.Question, options, schema, p.AuthorizationFlow, expires)
	if err != nil {
		return nil, ErrConflict
	}
	kind, nextState := "input_request", StateInputRequired
	if p.Type == InteractionAuthorization {
		kind, nextState = "authorization_request", StateAuthRequired
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_messages(task_id,sequence,role,kind,text_value,created_at) SELECT $1,COALESCE(max(sequence),0)+1,'agent',$2,$3,clock_timestamp() FROM agent_task_messages WHERE task_id=$1`, p.TaskID, kind, p.Question); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET state=$2,version=version+1,updated_at=clock_timestamp() WHERE id=$1`, p.TaskID, nextState); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, p.Agent, p.TaskID)
}

func (s *PostgresStore) RespondInteraction(ctx context.Context, p RespondInteractionParams) (*InteractionResult, error) {
	if p.RespondedBy == "" || p.InteractionID == "" || len(p.Response) == 0 || len(p.Response) > s.limits.MaxInteractionResponseBytes || !json.Valid(p.Response) || len(p.IdempotencyKey) > HardMaxIdempotencyBytes {
		return nil, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var owner, tenantID, ownerPrincipalID, agentResourceID string
	var state State
	if err = tx.QueryRowContext(ctx, `SELECT created_by,tenant_id,owner_principal_id,agent_resource_id,state FROM agent_tasks WHERE id=$1 AND agent_namespace=$2 AND agent_name=$3 FOR UPDATE`, p.TaskID, p.Agent.Namespace, p.Agent.Name).Scan(&owner, &tenantID, &ownerPrincipalID, &agentResourceID, &state); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if p.Authorization.Valid() && (tenantID != p.Authorization.TenantID || ownerPrincipalID != p.Authorization.PrincipalID || agentResourceID != p.Authorization.AgentResourceID) {
		return nil, ErrNotFound
	}
	if !p.Authorization.Valid() && owner != p.RespondedBy {
		return nil, ErrNotFound
	}
	if p.IdempotencyKey != "" {
		var hash string
		err = tx.QueryRowContext(ctx, `SELECT request_hash FROM agent_task_interaction_idempotency WHERE responded_by=$1 AND task_id=$2 AND interaction_id=$3 AND idempotency_key=$4`, p.RespondedBy, p.TaskID, p.InteractionID, p.IdempotencyKey).Scan(&hash)
		if err == nil {
			if hash != p.RequestHash {
				return nil, ErrIdempotencyConflict
			}
			if err = tx.Commit(); err != nil {
				return nil, err
			}
			t, e := s.Get(ctx, p.Agent, p.TaskID)
			return &InteractionResult{Task: t, Replay: true}, e
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	var i Interaction
	var opts, schema sql.NullString
	var expires time.Time
	err = tx.QueryRowContext(ctx, `SELECT type,status,options,schema,authorization_flow,expires_at FROM agent_task_interactions WHERE id=$1 AND task_id=$2 FOR UPDATE`, p.InteractionID, p.TaskID).Scan(&i.Type, &i.Status, &opts, &schema, &i.AuthorizationFlow, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if i.Status != InteractionPaused {
		return nil, ErrInteractionNotReady
	}
	if !expires.After(time.Now().UTC()) {
		code := FailureInputTimeout
		if i.Type == InteractionAuthorization {
			code = FailureAuthTimeout
		}
		if _, err = tx.ExecContext(ctx, `UPDATE agent_task_interactions SET status='expired' WHERE id=$1`, p.InteractionID); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET state='failed',failure_code=$2,version=version+1,updated_at=clock_timestamp(),completed_at=clock_timestamp() WHERE id=$1 AND state IN ('input_required','auth_required')`, p.TaskID, code); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE agent_task_dispatches SET status='closed',last_error_code=$2,updated_at=clock_timestamp() WHERE task_id=$1`, p.TaskID, code); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrInteractionExpired
	}
	if opts.Valid {
		_ = json.Unmarshal([]byte(opts.String), &i.Options)
	}
	if schema.Valid {
		i.Schema = json.RawMessage(schema.String)
	}
	if i.Type == InteractionAuthorization {
		var supplied struct {
			AuthorizationFlowID string `json:"authorizationFlowId"`
		}
		if json.Unmarshal(p.Response, &supplied) != nil || supplied.AuthorizationFlowID != i.AuthorizationFlow {
			return nil, ErrAuthorizationFlow
		}
		var reference string
		err = tx.QueryRowContext(ctx, `SELECT connection_reference FROM agent_task_authorization_flows WHERE id=$1 AND task_id=$2 AND interaction_id=$3 AND created_by=$4 AND status='completed' AND expires_at>clock_timestamp() FOR UPDATE`, supplied.AuthorizationFlowID, p.TaskID, p.InteractionID, p.RespondedBy).Scan(&reference)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAuthorizationFlow
		}
		if err != nil {
			return nil, err
		}
		p.Response, _ = json.Marshal(map[string]string{"reference": reference})
	}
	if err = validateInteractionResponse(&i, p.Response); err != nil {
		return nil, err
	}
	kind := "input_response"
	if i.Type == InteractionAuthorization {
		kind = "authorization_completed"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_task_interactions SET status='answered',response=$2,answered_at=clock_timestamp() WHERE id=$1`, p.InteractionID, string(p.Response)); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `WITH n AS (SELECT COALESCE(max(sequence),0) s FROM agent_task_messages WHERE task_id=$1) INSERT INTO agent_task_messages(task_id,sequence,role,kind,data,created_at) SELECT $1,s+1,'caller',$2,$3,clock_timestamp() FROM n UNION ALL SELECT $1,s+2,'platform','continuation_instruction',to_jsonb('Continue the task using the caller response above.'::text),clock_timestamp() FROM n`, p.TaskID, kind, string(p.Response)); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET state='queued',version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND state IN ('input_required','auth_required')`, p.TaskID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE agent_task_dispatches SET status='pending',attempts=0,next_attempt_at=clock_timestamp(),attempt_token='',receipt_id='',receipt_runtime='',receipt_session_id='',receipt_turn_id='',lease_owner=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE task_id=$1 AND status='delivered'`, p.TaskID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrConflict
	}
	if p.IdempotencyKey != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_interaction_idempotency(responded_by,task_id,interaction_id,idempotency_key,request_hash) VALUES($1,$2,$3,$4,$5)`, p.RespondedBy, p.TaskID, p.InteractionID, p.IdempotencyKey, p.RequestHash); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	t, err := s.Get(ctx, p.Agent, p.TaskID)
	return &InteractionResult{Task: t}, err
}

func (s *PostgresStore) CompleteAuthorizationFlow(ctx context.Context, p CompleteAuthorizationFlowParams) error {
	if p.FlowID == "" || p.TaskID == "" || p.InteractionID == "" || p.CreatedBy == "" || strings.TrimSpace(p.ConnectionReference) == "" || len(p.ConnectionReference) > HardMaxInteractionResponseBytes {
		return ErrInvalid
	}
	res, err := s.db.ExecContext(ctx, `UPDATE agent_task_authorization_flows SET status='completed',connection_reference=$5,completed_at=clock_timestamp() WHERE id=$1 AND task_id=$2 AND interaction_id=$3 AND created_by=$4 AND status='pending' AND expires_at>clock_timestamp()`, p.FlowID, p.TaskID, p.InteractionID, p.CreatedBy, strings.TrimSpace(p.ConnectionReference))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrAuthorizationFlow
	}
	return nil
}
