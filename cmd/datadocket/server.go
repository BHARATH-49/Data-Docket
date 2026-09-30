package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"datadocket/internal/analyze"
	"datadocket/internal/store"
	"datadocket/internal/ui"
)

type server struct {
	store          *store.Store
	uploadDir      string
	maxUploadBytes int64
}

func newServer(st *store.Store, uploadDir string, maxUploadBytes int64) http.Handler {
	s := &server{store: st, uploadDir: uploadDir, maxUploadBytes: maxUploadBytes}
	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", ui.Assets()))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.DB.PingContext(r.Context()); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/projects", http.StatusSeeOther) })
	mux.HandleFunc("GET /projects", s.projectsPage)
	mux.HandleFunc("POST /projects", s.createProjectPage)
	mux.HandleFunc("GET /projects/{id}", s.projectPage)
	mux.HandleFunc("POST /projects/{id}/baseline", s.uploadBaselinePage)
	mux.HandleFunc("POST /projects/{id}/candidates", s.uploadCandidatePage)
	mux.HandleFunc("POST /projects/{id}/baselines/{fileID}/approve", s.approvePage)
	mux.HandleFunc("GET /jobs/{id}", s.jobPage)
	mux.HandleFunc("GET /reports/{id}", s.reportPage)
	mux.HandleFunc("POST /api/projects", s.createProjectAPI)
	mux.HandleFunc("GET /api/projects", s.listProjectsAPI)
	mux.HandleFunc("GET /api/projects/{id}", s.getProjectAPI)
	mux.HandleFunc("GET /api/projects/{id}/jobs", s.listJobsAPI)
	mux.HandleFunc("POST /api/projects/{id}/baseline", s.uploadBaselineAPI)
	mux.HandleFunc("POST /api/projects/{id}/candidates", s.uploadCandidateAPI)
	mux.HandleFunc("POST /api/projects/{id}/baselines/{fileID}/approve", s.approveAPI)
	mux.HandleFunc("GET /api/jobs/{id}", s.getJobAPI)
	mux.HandleFunc("GET /api/reports/{id}", s.getReportAPI)
	return mux
}

type projectInput struct {
	Name                            string               `json:"name"`
	Columns                         []analyze.ColumnRule `json:"columns"`
	IDColumn                        string               `json:"id_column"`
	TargetColumn                    string               `json:"target_column"`
	MaxRowDropPercent               float64              `json:"max_row_drop_percent"`
	MaxMissingTargetIncreasePercent float64              `json:"max_missing_target_increase_percent"`
}

func (p projectInput) config() analyze.Config {
	return analyze.Config{Columns: p.Columns, IDColumn: p.IDColumn, TargetColumn: p.TargetColumn, MaxRowDropPercent: p.MaxRowDropPercent, MaxMissingTargetIncreasePercent: p.MaxMissingTargetIncreasePercent}
}

func parseProjectInput(w http.ResponseWriter, r *http.Request, api bool) (projectInput, error) {
	var p projectInput
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if api {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return p, fmt.Errorf("invalid JSON: %w", err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return p, fmt.Errorf("send one JSON object")
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return p, err
		}
		p.Name = strings.TrimSpace(r.FormValue("name"))
		p.IDColumn = strings.TrimSpace(r.FormValue("id_column"))
		p.TargetColumn = strings.TrimSpace(r.FormValue("target_column"))
		var err error
		p.MaxRowDropPercent, err = strconv.ParseFloat(r.FormValue("max_row_drop_percent"), 64)
		if err != nil {
			return p, fmt.Errorf("row drop threshold must be a number")
		}
		p.MaxMissingTargetIncreasePercent, err = strconv.ParseFloat(r.FormValue("max_missing_target_increase_percent"), 64)
		if err != nil {
			return p, fmt.Errorf("missing target threshold must be a number")
		}
		for _, line := range strings.Split(r.FormValue("columns"), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			pair := strings.Split(line, ",")
			if len(pair) != 2 {
				return p, fmt.Errorf("each column line must be name,type")
			}
			p.Columns = append(p.Columns, analyze.ColumnRule{Name: strings.TrimSpace(pair[0]), Type: strings.TrimSpace(pair[1])})
		}
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 120 {
		return p, fmt.Errorf("project name must contain 1 to 120 characters")
	}
	if issues := analyze.ValidateConfig(p.config()); len(issues) > 0 {
		return p, fmt.Errorf("invalid rules: %s", issues[0].Message)
	}
	return p, nil
}
func (s *server) createProject(r *http.Request, w http.ResponseWriter, api bool) (store.Project, error) {
	p, err := parseProjectInput(w, r, api)
	if err != nil {
		return store.Project{}, err
	}
	rules, err := json.Marshal(p.config())
	if err != nil {
		return store.Project{}, err
	}
	return s.store.CreateProject(r.Context(), p.Name, rules)
}

func pathID(r *http.Request, key string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return id, nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func statusFor(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return 404
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNoApprovedBaseline):
		return 409
	default:
		return 500
	}
}
func apiError(w http.ResponseWriter, err error, status int) {
	if status == 0 {
		status = statusFor(err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func pageRedirectError(w http.ResponseWriter, r *http.Request, url string, err error) {
	http.Redirect(w, r, url+"?error="+queryEscape(err.Error()), http.StatusSeeOther)
}
func queryEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s *server) createProjectAPI(w http.ResponseWriter, r *http.Request) {
	p, err := s.createProject(r, w, true)
	if err != nil {
		apiError(w, err, 400)
		return
	}
	writeJSON(w, 201, p)
}
func (s *server) createProjectPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.createProject(r, w, false)
	if err != nil {
		pageRedirectError(w, r, "/projects", err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d", p.ID), 303)
}
func (s *server) listProjectsAPI(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.ListProjects(r.Context())
	if err != nil {
		apiError(w, err, 0)
		return
	}
	writeJSON(w, 200, p)
}
func (s *server) getProjectAPI(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		apiError(w, err, 400)
		return
	}
	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		apiError(w, err, 0)
		return
	}
	writeJSON(w, 200, p)
}
func (s *server) listJobsAPI(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		apiError(w, err, 400)
		return
	}
	if _, err = s.store.GetProject(r.Context(), id); err != nil {
		apiError(w, err, 0)
		return
	}
	jobs, err := s.store.ListJobs(r.Context(), id)
	if err != nil {
		apiError(w, err, 0)
		return
	}
	writeJSON(w, 200, jobs)
}
func (s *server) getJobAPI(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		apiError(w, err, 400)
		return
	}
	j, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		apiError(w, err, 0)
		return
	}
	writeJSON(w, 200, j)
}
func (s *server) getReportAPI(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		apiError(w, err, 400)
		return
	}
	rep, err := s.store.GetReport(r.Context(), id)
	if err != nil {
		apiError(w, err, 0)
		return
	}
	writeJSON(w, 200, rep)
}

func (s *server) saveUpload(w http.ResponseWriter, r *http.Request, kind string) (store.Job, error) {
	var empty store.Job
	id, err := pathID(r, "id")
	if err != nil {
		return empty, err
	}
	project, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		return empty, err
	}
	if kind == "candidate" && project.ApprovedBaselineFileID == nil {
		return empty, store.ErrNoApprovedBaseline
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadBytes+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		return empty, fmt.Errorf("expected multipart CSV upload: %w", err)
	}
	var partFound bool
	for {
		part, e := reader.NextPart()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return empty, e
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		if partFound {
			part.Close()
			return empty, fmt.Errorf("send one CSV file")
		}
		partFound = true
		name := path.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
		if !strings.EqualFold(filepath.Ext(name), ".csv") {
			part.Close()
			return empty, fmt.Errorf("file name must end in .csv")
		}
		bytes := make([]byte, 16)
		if _, e = rand.Read(bytes); e != nil {
			part.Close()
			return empty, e
		}
		storageName := hex.EncodeToString(bytes) + ".csv"
		fullPath := filepath.Join(s.uploadDir, storageName)
		file, e := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			part.Close()
			return empty, e
		}
		n, copyErr := io.Copy(file, io.LimitReader(part, s.maxUploadBytes+1))
		closeErr := file.Close()
		part.Close()
		if copyErr != nil || closeErr != nil || n > s.maxUploadBytes || n == 0 {
			os.Remove(fullPath)
			if n > s.maxUploadBytes {
				return empty, fmt.Errorf("CSV exceeds the %d byte upload limit", s.maxUploadBytes)
			}
			if n == 0 {
				return empty, fmt.Errorf("CSV is empty")
			}
			if copyErr != nil {
				return empty, copyErr
			}
			return empty, closeErr
		}
		meta := store.File{ProjectID: id, Kind: kind, OriginalName: name, StoragePath: storageName, SizeBytes: n}
		_, job, e := s.store.CreateUploadJob(r.Context(), meta, 3)
		if e != nil {
			os.Remove(fullPath)
			return empty, e
		}
		return job, nil
	}
	return empty, fmt.Errorf("multipart field 'file' is required")
}
func (s *server) uploadBaselineAPI(w http.ResponseWriter, r *http.Request) {
	s.uploadAPI(w, r, "baseline")
}
func (s *server) uploadCandidateAPI(w http.ResponseWriter, r *http.Request) {
	s.uploadAPI(w, r, "candidate")
}
func (s *server) uploadAPI(w http.ResponseWriter, r *http.Request, kind string) {
	j, err := s.saveUpload(w, r, kind)
	if err != nil {
		apiError(w, err, uploadStatus(err))
		return
	}
	writeJSON(w, 202, map[string]any{"job_id": j.ID, "state": j.State, "job_url": fmt.Sprintf("/api/jobs/%d", j.ID)})
}
func uploadStatus(err error) int {
	if errors.Is(err, store.ErrNotFound) {
		return 404
	}
	if errors.Is(err, store.ErrNoApprovedBaseline) {
		return 409
	}
	return 400
}
func (s *server) uploadBaselinePage(w http.ResponseWriter, r *http.Request) {
	s.uploadPage(w, r, "baseline")
}
func (s *server) uploadCandidatePage(w http.ResponseWriter, r *http.Request) {
	s.uploadPage(w, r, "candidate")
}
func (s *server) uploadPage(w http.ResponseWriter, r *http.Request, kind string) {
	j, err := s.saveUpload(w, r, kind)
	if err != nil {
		pageRedirectError(w, r, "/projects/"+r.PathValue("id"), err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/jobs/%d", j.ID), 303)
}
func (s *server) approve(w http.ResponseWriter, r *http.Request) (store.Project, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return store.Project{}, err
	}
	fileID, err := pathID(r, "fileID")
	if err != nil {
		return store.Project{}, err
	}
	return s.store.ApproveBaseline(r.Context(), id, fileID)
}
func (s *server) approveAPI(w http.ResponseWriter, r *http.Request) {
	p, err := s.approve(w, r)
	if err != nil {
		apiError(w, err, 0)
		return
	}
	writeJSON(w, 200, p)
}
func (s *server) approvePage(w http.ResponseWriter, r *http.Request) {
	p, err := s.approve(w, r)
	if err != nil {
		pageRedirectError(w, r, "/projects/"+r.PathValue("id"), err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?notice=Baseline+approved", p.ID), 303)
}

func (s *server) projectsPage(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	items := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		items = append(items, map[string]any{"ID": p.ID, "Name": p.Name})
	}
	_ = ui.Render(w, "projects", map[string]any{"Title": "Projects", "Projects": items, "Error": r.URL.Query().Get("error"), "Notice": r.URL.Query().Get("notice")})
}
func (s *server) projectPage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), statusFor(err))
		return
	}
	var cfg analyze.Config
	if err := json.Unmarshal(p.Rules, &cfg); err != nil {
		http.Error(w, "stored rules are invalid", 500)
		return
	}
	jobs, err := s.store.ListJobs(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	view := map[string]any{"ID": p.ID, "Name": p.Name, "Columns": cfg.Columns, "IDColumn": cfg.IDColumn, "TargetColumn": cfg.TargetColumn, "MaxRowDropPercent": cfg.MaxRowDropPercent, "MaxMissingTargetIncreasePercent": cfg.MaxMissingTargetIncreasePercent}
	if p.ApprovedBaselineFileID != nil {
		if f, e := s.store.GetFile(r.Context(), *p.ApprovedBaselineFileID); e == nil {
			view["ApprovedBaseline"] = map[string]any{"ID": f.ID, "Filename": f.OriginalName}
		}
	}
	history := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		filename := "(missing file)"
		if f, e := s.store.GetFile(r.Context(), j.FileID); e == nil {
			filename = f.OriginalName
		}
		row := map[string]any{"JobID": j.ID, "Kind": j.State, "Filename": filename, "Status": j.State, "CreatedAt": j.CreatedAt.Local().Format("02 Jan 2006 15:04")}
		if f, e := s.store.GetFile(r.Context(), j.FileID); e == nil {
			row["Kind"] = f.Kind
		}
		if rep, e := s.store.GetReport(r.Context(), j.ID); e == nil {
			row["Decision"] = rep.Decision
			row["ReportID"] = j.ID
			if row["Kind"] == "baseline" && rep.Decision == "pass" && (p.ApprovedBaselineFileID == nil || *p.ApprovedBaselineFileID != j.FileID) {
				if _, exists := view["PendingBaseline"]; !exists {
					view["PendingBaseline"] = map[string]any{"ID": j.FileID, "Filename": filename, "ReportID": j.ID}
				}
			}
		}
		history = append(history, row)
	}
	view["History"] = history
	_ = ui.Render(w, "project", map[string]any{"Title": p.Name, "Project": view, "Error": r.URL.Query().Get("error"), "Notice": r.URL.Query().Get("notice")})
}
func (s *server) jobPage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	j, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), statusFor(err))
		return
	}
	f, _ := s.store.GetFile(r.Context(), j.FileID)
	view := map[string]any{"ID": j.ID, "ProjectID": j.ProjectID, "Kind": f.Kind, "Filename": f.OriginalName, "Status": j.State, "Attempts": j.Attempts, "MaxAttempts": j.MaxAttempts, "Error": j.ErrorMessage, "CreatedAt": j.CreatedAt.Local().Format("02 Jan 2006 15:04")}
	if rep, e := s.store.GetReport(r.Context(), j.ID); e == nil {
		view["ReportID"] = j.ID
		view["Decision"] = rep.Decision
	}
	_ = ui.Render(w, "job", map[string]any{"Title": fmt.Sprintf("Job %d", j.ID), "Job": view})
}
func formatValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "—"
	case []string:
		return strings.Join(v, ", ")
	case []any:
		parts := make([]string, 0, len(v))
		for _, x := range v {
			parts = append(parts, fmt.Sprint(x))
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(v)
	}
}
func (s *server) reportPage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	rep, err := s.store.GetReport(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), statusFor(err))
		return
	}
	j, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), statusFor(err))
		return
	}
	f, _ := s.store.GetFile(r.Context(), j.FileID)
	var payload reportPayload
	if err := json.Unmarshal(rep.Payload, &payload); err != nil {
		http.Error(w, "report data is invalid", 500)
		return
	}
	findings := make([]map[string]any, 0, len(payload.Validation.Findings))
	for _, item := range payload.Validation.Findings {
		rows := make([]string, 0, len(item.Examples))
		for _, ex := range item.Examples {
			if ex.Value != "" {
				rows = append(rows, fmt.Sprintf("%d (%s)", ex.Row, ex.Value))
			} else {
				rows = append(rows, fmt.Sprint(ex.Row))
			}
		}
		findings = append(findings, map[string]any{"Code": item.Code, "Message": fmt.Sprintf("%s (%d occurrence(s))", item.Message, item.Count), "Column": item.Column, "Rows": strings.Join(rows, ", ")})
	}
	checks := []map[string]any{}
	comparisonMessage := "Baseline uploads are validated without a comparison."
	if payload.Comparison != nil {
		for _, check := range payload.Comparison.Checks {
			outcome := "block"
			if check.Pass {
				outcome = "pass"
			}
			baselineValue, candidateValue := formatValue(check.BaselineValue), formatValue(check.CandidateValue)
			threshold := formatValue(check.Threshold)
			if check.Code == "missing_target_increase" {
				baselineValue += "%"
				candidateValue += "%"
			}
			if check.Code != "unseen_target_labels" {
				threshold = strings.TrimSpace(threshold + " " + check.Unit)
			}
			checks = append(checks, map[string]any{"Name": strings.ReplaceAll(check.Code, "_", " "), "Baseline": baselineValue, "Candidate": candidateValue, "Threshold": threshold, "Outcome": outcome, "Message": check.Message})
		}
		if len(payload.Comparison.Checks) == 0 {
			comparisonMessage = "Comparison skipped because validation did not pass."
		}
	}
	view := map[string]any{"ProjectID": j.ProjectID, "JobID": j.ID, "Kind": f.Kind, "Filename": f.OriginalName, "Decision": rep.Decision, "CreatedAt": rep.CreatedAt.Local().Format("02 Jan 2006 15:04"), "Summary": payload.Summary, "Validation": map[string]any{"Rows": payload.Validation.RowCount, "MissingTargetCount": payload.Validation.MissingTargetCount, "Findings": findings}, "Comparison": map[string]any{"Checks": checks, "Message": comparisonMessage}}
	_ = ui.Render(w, "report", map[string]any{"Title": fmt.Sprintf("Report %d", rep.ID), "Report": view})
}
