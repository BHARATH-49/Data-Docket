package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"datadocket/internal/store"
)

func TestUploadToReportWorkflow(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	st := store.New(db)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	uploadDir := t.TempDir()
	site := httptest.NewServer(newServer(st, uploadDir, 1<<20))
	defer site.Close()
	client := site.Client()
	projectJSON := fmt.Sprintf(`{"name":"workflow-%d","columns":[{"name":"id","type":"string"},{"name":"age","type":"integer"},{"name":"income","type":"number"},{"name":"approved","type":"string"}],"id_column":"id","target_column":"approved","max_row_drop_percent":20,"max_missing_target_increase_percent":10}`, time.Now().UnixNano())
	resp, err := client.Post(site.URL+"/api/projects", "application/json", bytes.NewBufferString(projectJSON))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create project: %d %s", resp.StatusCode, body)
	}
	var project store.Project
	if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	t.Cleanup(func() { db.ExecContext(context.Background(), `DELETE FROM projects WHERE id=$1`, project.ID) })
	upload := func(kind, filename string) int64 {
		t.Helper()
		content, err := os.ReadFile(filepath.Join("..", "..", "examples", filename))
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		url := fmt.Sprintf("%s/api/projects/%d/%s", site.URL, project.ID, kind)
		response, err := client.Post(url, writer.FormDataContentType(), &body)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 202 {
			data, _ := io.ReadAll(response.Body)
			t.Fatalf("upload %s: %d %s", filename, response.StatusCode, data)
		}
		var result struct {
			JobID int64 `json:"job_id"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result.JobID
	}
	process := func(jobID int64, want string) {
		t.Helper()
		claimed, err := st.ClaimNextJob(ctx, "test-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if claimed.ID != jobID {
			t.Fatalf("claimed %d, want %d", claimed.ID, jobID)
		}
		payload, decision, err := processJob(ctx, st, uploadDir, claimed)
		if err != nil {
			t.Fatal(err)
		}
		if decision != want {
			t.Fatalf("job %d decision=%s, want %s: %+v", jobID, decision, want, payload)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CompleteJob(ctx, jobID, claimed.ClaimToken, decision, encoded); err != nil {
			t.Fatal(err)
		}
		response, err := client.Get(fmt.Sprintf("%s/api/reports/%d", site.URL, jobID))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("report status=%d", response.StatusCode)
		}
		var report store.Report
		if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
			t.Fatal(err)
		}
		if report.Decision != want {
			t.Fatalf("stored decision=%s", report.Decision)
		}
	}
	baselineJob := upload("baseline", "baseline.csv")
	process(baselineJob, "pass")
	baseline, err := st.GetJob(ctx, baselineJob)
	if err != nil {
		t.Fatal(err)
	}
	approveURL := fmt.Sprintf("%s/api/projects/%d/baselines/%d/approve", site.URL, project.ID, baseline.FileID)
	response, err := client.Post(approveURL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("approve status=%d", response.StatusCode)
	}
	process(upload("candidates", "candidate-pass.csv"), "pass")
	process(upload("candidates", "candidate-block.csv"), "block")
}
