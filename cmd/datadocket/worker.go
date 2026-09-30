package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"datadocket/internal/analyze"
	"datadocket/internal/store"
)

type reportPayload struct {
	Summary    string              `json:"summary"`
	Validation analyze.Validation  `json:"validation"`
	Comparison *analyze.Comparison `json:"comparison,omitempty"`
}

type permanentError struct{ cause error }

func (e permanentError) Error() string { return e.cause.Error() }
func (e permanentError) Unwrap() error { return e.cause }

func runWorker(ctx context.Context, st *store.Store, uploadDir, workerID string) error {
	const lease = 30 * time.Second
	for ctx.Err() == nil {
		job, err := st.ClaimNextJob(ctx, workerID, lease)
		if errors.Is(err, store.ErrNoJob) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
				continue
			}
		}
		if err != nil {
			log.Printf("claim job: %v", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
				continue
			}
		}
		log.Printf("worker %s processing job %d attempt %d", workerID, job.ID, job.Attempts)
		jobCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-jobCtx.Done():
					return
				case <-ticker.C:
					if e := st.RenewLease(jobCtx, job.ID, job.ClaimToken, lease); e != nil {
						log.Printf("renew job %d lease: %v", job.ID, e)
						return
					}
				}
			}
		}()
		payload, decision, processErr := processJob(jobCtx, st, uploadDir, job)
		cancel()
		<-done
		if processErr != nil {
			var permanent permanentError
			if e := st.RetryOrFailJob(context.Background(), job.ID, job.ClaimToken, processErr.Error(), !errors.As(processErr, &permanent)); e != nil && !errors.Is(e, store.ErrStaleClaim) {
				log.Printf("record job %d error: %v", job.ID, e)
			}
			continue
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if err := st.CompleteJob(context.Background(), job.ID, job.ClaimToken, decision, encoded); err != nil && !errors.Is(err, store.ErrStaleClaim) {
			log.Printf("complete job %d: %v", job.ID, err)
		}
	}
	return ctx.Err()
}

func processJob(ctx context.Context, st *store.Store, uploadDir string, job store.Job) (reportPayload, string, error) {
	var payload reportPayload
	project, err := st.GetProject(ctx, job.ProjectID)
	if err != nil {
		return payload, "", err
	}
	var cfg analyze.Config
	if err := json.Unmarshal(project.Rules, &cfg); err != nil {
		return payload, "", permanentError{fmt.Errorf("stored project rules are invalid: %w", err)}
	}
	file, err := st.GetFile(ctx, job.FileID)
	if err != nil {
		return payload, "", err
	}
	path, err := storedFilePath(uploadDir, file.StoragePath)
	if err != nil {
		return payload, "", err
	}
	payload.Validation, err = analyze.Analyze(path, cfg)
	if err != nil {
		return payload, "", fmt.Errorf("read upload: %w", err)
	}
	if !payload.Validation.Pass {
		payload.Summary = fmt.Sprintf("Blocked: %d validation finding(s) need attention.", len(payload.Validation.Findings))
		if file.Kind == "candidate" {
			skipped := analyze.Compare(analyze.Validation{}, payload.Validation, cfg)
			payload.Comparison = &skipped
		}
		return payload, "block", nil
	}
	if file.Kind == "baseline" {
		payload.Summary = "Pass: baseline meets the configured CSV validation rules."
		return payload, "pass", nil
	}
	if job.BaselineFileID == nil {
		return payload, "", permanentError{fmt.Errorf("candidate job has no approved baseline snapshot")}
	}
	baseFile, err := st.GetFile(ctx, *job.BaselineFileID)
	if err != nil {
		return payload, "", err
	}
	basePath, err := storedFilePath(uploadDir, baseFile.StoragePath)
	if err != nil {
		return payload, "", err
	}
	base, err := analyze.Analyze(basePath, cfg)
	if err != nil {
		return payload, "", fmt.Errorf("read approved baseline: %w", err)
	}
	comparison := analyze.Compare(base, payload.Validation, cfg)
	payload.Comparison = &comparison
	if !base.Pass {
		payload.Summary = "Blocked: the approved baseline no longer validates; upload and approve a new baseline."
		return payload, "block", nil
	}
	if !comparison.Pass {
		payload.Summary = "Blocked: candidate exceeds a comparison threshold or introduces a new target label."
		return payload, "block", nil
	}
	payload.Summary = "Pass: candidate validates and remains within all baseline comparison thresholds."
	return payload, "pass", nil
}

func storedFilePath(root, name string) (string, error) {
	if name == "" || name != filepath.Base(name) || filepath.IsAbs(name) || name == "." || name == ".." {
		return "", permanentError{fmt.Errorf("unsafe stored file path")}
	}
	return filepath.Join(root, name), nil
}
