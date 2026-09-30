package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

//go:embed schema.sql
var schemaFS embed.FS

var (
	ErrNotFound           = errors.New("not found")
	ErrConflict           = errors.New("conflict")
	ErrNoApprovedBaseline = errors.New("approve a passing baseline before uploading a candidate")
	ErrNoJob              = errors.New("no job available")
	ErrStaleClaim         = errors.New("job claim is no longer current")
)

type Store struct{ DB *sql.DB }

func New(db *sql.DB) *Store { return &Store{DB: db} }

type Project struct {
	ID                     int64           `json:"id"`
	Name                   string          `json:"name"`
	Rules                  json.RawMessage `json:"rules"`
	ApprovedBaselineFileID *int64          `json:"approved_baseline_file_id,omitempty"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
}
type File struct {
	ID           int64     `json:"id"`
	ProjectID    int64     `json:"project_id"`
	Kind         string    `json:"kind"`
	OriginalName string    `json:"original_name"`
	StoragePath  string    `json:"storage_path"`
	SizeBytes    int64     `json:"size_bytes"`
	CreatedAt    time.Time `json:"created_at"`
}
type Job struct {
	ID             int64      `json:"id"`
	ProjectID      int64      `json:"project_id"`
	FileID         int64      `json:"file_id"`
	BaselineFileID *int64     `json:"baseline_file_id,omitempty"`
	State          string     `json:"state"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	ClaimToken     string     `json:"-"`
	ClaimedBy      string     `json:"claimed_by,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	AvailableAt    time.Time  `json:"available_at"`
	ErrorMessage   string     `json:"error_message,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}
type Report struct {
	ID        int64           `json:"id"`
	JobID     int64           `json:"job_id"`
	Decision  string          `json:"decision"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

func (s *Store) Migrate(ctx context.Context) error {
	sqlText, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock(973441)`); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(973441)`)
	_, err = conn.ExecContext(ctx, string(sqlText))
	return err
}

func projectScan(row interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var approved sql.NullInt64
	err := row.Scan(&p.ID, &p.Name, &p.Rules, &approved, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if approved.Valid {
		p.ApprovedBaselineFileID = &approved.Int64
	}
	return p, err
}
func (s *Store) CreateProject(ctx context.Context, name string, rules json.RawMessage) (Project, error) {
	p, err := projectScan(s.DB.QueryRowContext(ctx, `INSERT INTO projects(name,rules) VALUES($1,$2) RETURNING id,name,rules,approved_baseline_file_id,created_at,updated_at`, name, rules))
	if err != nil && isUnique(err) {
		return p, fmt.Errorf("%w: a project with this name already exists", ErrConflict)
	}
	return p, err
}
func (s *Store) GetProject(ctx context.Context, id int64) (Project, error) {
	return projectScan(s.DB.QueryRowContext(ctx, `SELECT id,name,rules,approved_baseline_file_id,created_at,updated_at FROM projects WHERE id=$1`, id))
}
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name,rules,approved_baseline_file_id,created_at,updated_at FROM projects ORDER BY created_at DESC,id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		p, e := projectScan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func fileScan(row interface{ Scan(...any) error }) (File, error) {
	var f File
	err := row.Scan(&f.ID, &f.ProjectID, &f.Kind, &f.OriginalName, &f.StoragePath, &f.SizeBytes, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}
func (s *Store) GetFile(ctx context.Context, id int64) (File, error) {
	return fileScan(s.DB.QueryRowContext(ctx, `SELECT id,project_id,kind,original_name,storage_path,size_bytes,created_at FROM files WHERE id=$1`, id))
}
func (s *Store) CreateUploadJob(ctx context.Context, f File, maxAttempts int) (File, Job, error) {
	var j Job
	if maxAttempts < 1 {
		return f, j, fmt.Errorf("max attempts must be positive")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return f, j, err
	}
	defer tx.Rollback()
	var approved sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT approved_baseline_file_id FROM projects WHERE id=$1 FOR SHARE`, f.ProjectID).Scan(&approved)
	if errors.Is(err, sql.ErrNoRows) {
		return f, j, ErrNotFound
	}
	if err != nil {
		return f, j, err
	}
	if f.Kind == "candidate" {
		if !approved.Valid {
			return f, j, ErrNoApprovedBaseline
		}
		j.BaselineFileID = &approved.Int64
	} else if f.Kind != "baseline" {
		return f, j, fmt.Errorf("invalid file kind")
	}
	f, err = fileScan(tx.QueryRowContext(ctx, `INSERT INTO files(project_id,kind,original_name,storage_path,size_bytes) VALUES($1,$2,$3,$4,$5) RETURNING id,project_id,kind,original_name,storage_path,size_bytes,created_at`, f.ProjectID, f.Kind, f.OriginalName, f.StoragePath, f.SizeBytes))
	if err != nil {
		return f, j, err
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO jobs(project_id,file_id,baseline_file_id,max_attempts) VALUES($1,$2,$3,$4) RETURNING id,available_at,created_at,updated_at`, f.ProjectID, f.ID, j.BaselineFileID, maxAttempts).Scan(&j.ID, &j.AvailableAt, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return f, j, err
	}
	if err = tx.Commit(); err != nil {
		return f, j, err
	}
	j.ProjectID = f.ProjectID
	j.FileID = f.ID
	j.State = "queued"
	j.MaxAttempts = maxAttempts
	return f, j, nil
}

func jobScan(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var base sql.NullInt64
	var token, by, msg sql.NullString
	var lease, started, completed sql.NullTime
	err := row.Scan(&j.ID, &j.ProjectID, &j.FileID, &base, &j.State, &j.Attempts, &j.MaxAttempts, &token, &by, &lease, &j.AvailableAt, &msg, &j.CreatedAt, &j.UpdatedAt, &started, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	if base.Valid {
		j.BaselineFileID = &base.Int64
	}
	if token.Valid {
		j.ClaimToken = token.String
	}
	if by.Valid {
		j.ClaimedBy = by.String
	}
	if msg.Valid {
		j.ErrorMessage = msg.String
	}
	if lease.Valid {
		j.LeaseExpiresAt = &lease.Time
	}
	if started.Valid {
		j.StartedAt = &started.Time
	}
	if completed.Valid {
		j.CompletedAt = &completed.Time
	}
	return j, nil
}

const jobColumns = `id,project_id,file_id,baseline_file_id,state,attempts,max_attempts,claim_token,claimed_by,lease_expires_at,available_at,error_message,created_at,updated_at,started_at,completed_at`

func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	return jobScan(s.DB.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=$1`, id))
}
func (s *Store) ListJobs(ctx context.Context, projectID int64) ([]Job, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE project_id=$1 ORDER BY created_at DESC,id DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, e := jobScan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *Store) GetReport(ctx context.Context, jobID int64) (Report, error) {
	var r Report
	err := s.DB.QueryRowContext(ctx, `SELECT id,job_id,decision,payload,created_at FROM reports WHERE job_id=$1`, jobID).Scan(&r.ID, &r.JobID, &r.Decision, &r.Payload, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}
func (s *Store) ApproveBaseline(ctx context.Context, projectID, fileID int64) (Project, error) {
	var p Project
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	var passing bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM files f JOIN jobs j ON j.file_id=f.id JOIN reports r ON r.job_id=j.id WHERE f.id=$1 AND f.project_id=$2 AND f.kind='baseline' AND j.state='completed' AND r.decision='pass')`, fileID, projectID).Scan(&passing)
	if err != nil {
		return p, err
	}
	if !passing {
		return p, fmt.Errorf("%w: baseline needs a completed Pass report", ErrConflict)
	}
	p, err = projectScan(tx.QueryRowContext(ctx, `UPDATE projects SET approved_baseline_file_id=$1,updated_at=now() WHERE id=$2 RETURNING id,name,rules,approved_baseline_file_id,created_at,updated_at`, fileID, projectID))
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func (s *Store) ClaimNextJob(ctx context.Context, workerID string, lease time.Duration) (Job, error) {
	var j Job
	if lease <= 0 {
		return j, fmt.Errorf("lease must be positive")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return j, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET state='failed',error_message='Worker stopped and retry limit was reached.',claim_token=NULL,claimed_by=NULL,lease_expires_at=NULL,completed_at=now(),updated_at=now() WHERE state='running' AND lease_expires_at<now() AND attempts>=max_attempts`)
	if err != nil {
		return j, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE ((state='queued' AND available_at<=now()) OR (state='running' AND lease_expires_at<now())) AND attempts<max_attempts ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if e := tx.Commit(); e != nil {
			return j, e
		}
		return j, ErrNoJob
	}
	if err != nil {
		return j, err
	}
	token, err := randomToken()
	if err != nil {
		return j, err
	}
	j, err = jobScan(tx.QueryRowContext(ctx, `UPDATE jobs SET state='running',attempts=attempts+1,claim_token=$2,claimed_by=$3,lease_expires_at=now()+($4 * interval '1 second'),started_at=COALESCE(started_at,now()),error_message=NULL,updated_at=now() WHERE id=$1 RETURNING `+jobColumns, id, token, workerID, lease.Seconds()))
	if err != nil {
		return j, err
	}
	return j, tx.Commit()
}
func (s *Store) RenewLease(ctx context.Context, id int64, token string, lease time.Duration) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=now()+($3 * interval '1 second'),updated_at=now() WHERE id=$1 AND claim_token=$2 AND state='running'`, id, token, lease.Seconds())
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrStaleClaim
	}
	return nil
}
func (s *Store) CompleteJob(ctx context.Context, id int64, token, decision string, payload json.RawMessage) error {
	if decision != "pass" && decision != "block" {
		return fmt.Errorf("invalid decision")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var lockedID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE id=$1 AND claim_token=$2 AND state='running' FOR UPDATE`, id, token).Scan(&lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleClaim
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO reports(job_id,decision,payload) VALUES($1,$2,$3) ON CONFLICT(job_id) DO UPDATE SET decision=EXCLUDED.decision,payload=EXCLUDED.payload,created_at=now()`, id, decision, payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET state='completed',claim_token=NULL,claimed_by=NULL,lease_expires_at=NULL,error_message=NULL,completed_at=now(),updated_at=now() WHERE id=$1`, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) RetryOrFailJob(ctx context.Context, id int64, token, message string, transient bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempts, maxAttempts int
	err = tx.QueryRowContext(ctx, `SELECT attempts,max_attempts FROM jobs WHERE id=$1 AND claim_token=$2 AND state='running' FOR UPDATE`, id, token).Scan(&attempts, &maxAttempts)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleClaim
	}
	if err != nil {
		return err
	}
	if len(message) > 1000 {
		message = message[:1000]
	}
	if transient && attempts < maxAttempts {
		delay := time.Duration(1<<min(attempts, 5)) * time.Second
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET state='queued',claim_token=NULL,claimed_by=NULL,lease_expires_at=NULL,available_at=$2,error_message=$3,updated_at=now() WHERE id=$1`, id, time.Now().Add(delay), message)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET state='failed',claim_token=NULL,claimed_by=NULL,lease_expires_at=NULL,error_message=$2,completed_at=now(),updated_at=now() WHERE id=$1`, id, message)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func isUnique(err error) bool {
	return err != nil && (contains(err.Error(), "duplicate key") || contains(err.Error(), "23505"))
}
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
