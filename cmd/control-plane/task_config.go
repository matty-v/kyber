package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/matty-v/kyber/pkg/taskstore"
)

func loadTaskLimits() (taskstore.Limits, error) {
	l := taskstore.DefaultLimits()
	ints := []struct {
		name string
		dst  *int
	}{{"KYBER_TASKS_MAX_PROMPT_BYTES", &l.MaxPromptBytes}, {"KYBER_TASKS_MAX_CORRELATION_BYTES", &l.MaxCorrelationBytes}, {"KYBER_TASKS_MAX_RESPONSE_BYTES", &l.MaxResponseBytes}, {"KYBER_TASKS_MAX_OUTSTANDING", &l.MaxOutstanding}, {"KYBER_TASKS_MAX_RETAINED", &l.MaxRetained}, {"KYBER_TASKS_MAX_DISPATCH_ATTEMPTS", &l.MaxDispatchAttempts}, {"KYBER_TASKS_MAX_PROGRESS_BYTES", &l.MaxProgressBytes}, {"KYBER_TASKS_MAX_PROGRESS_UPDATES", &l.MaxProgressUpdates}, {"KYBER_TASKS_MAX_RESULTS", &l.MaxResults}, {"KYBER_TASKS_MAX_RESULT_PARTS", &l.MaxResultParts}, {"KYBER_TASKS_MAX_RESULT_NAME_BYTES", &l.MaxResultNameBytes}, {"KYBER_TASKS_MAX_DESCRIPTION_BYTES", &l.MaxDescriptionBytes}, {"KYBER_TASKS_MAX_FILENAME_BYTES", &l.MaxFilenameBytes}}
	for _, f := range ints {
		if raw := os.Getenv(f.name); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v <= 0 {
				return l, fmt.Errorf("%s must be a positive integer", f.name)
			}
			*f.dst = v
		}
	}
	int64s := []struct {
		name string
		dst  *int64
	}{{"KYBER_TASKS_MAX_TEXT_PART_BYTES", &l.MaxTextPartBytes}, {"KYBER_TASKS_MAX_JSON_PART_BYTES", &l.MaxJSONPartBytes}, {"KYBER_TASKS_MAX_FILE_BYTES", &l.MaxFileBytes}, {"KYBER_TASKS_MAX_TASK_FILE_BYTES", &l.MaxTaskFileBytes}}
	for _, f := range int64s {
		if raw := os.Getenv(f.name); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || v <= 0 {
				return l, fmt.Errorf("%s must be a positive integer", f.name)
			}
			*f.dst = v
		}
	}
	durations := []struct {
		name string
		dst  *time.Duration
		unit time.Duration
	}{{"KYBER_TASKS_DEFAULT_DEADLINE_HOURS", &l.DefaultDeadline, time.Hour}, {"KYBER_TASKS_MAX_DEADLINE_HOURS", &l.MaxDeadline, time.Hour}, {"KYBER_TASKS_RETENTION_HOURS", &l.Retention, time.Hour}}
	for _, f := range durations {
		if raw := os.Getenv(f.name); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v <= 0 {
				return l, fmt.Errorf("%s must be a positive integer", f.name)
			}
			*f.dst = time.Duration(v) * f.unit
		}
	}
	if err := l.Validate(); err != nil {
		return l, fmt.Errorf("durable task limits: %w", err)
	}
	return l, nil
}
