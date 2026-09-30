package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func integrationStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	st := New(db)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}
func testProject(t *testing.T, st *Store) Project {
	t.Helper()
	ctx := context.Background()
	p, err := st.CreateProject(ctx, fmt.Sprintf("store-test-%d", time.Now().UnixNano()), json.RawMessage(`{"columns":[{"name":"id","type":"string"},{"name":"target","type":"string"}],"id_column":"id","target_column":"target","max_row_drop_percent":20,"max_missing_target_increase_percent":10}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DB.ExecContext(context.Background(), `DELETE FROM projects WHERE id=$1`, p.ID) })
	return p
}
func testUpload(t *testing.T, st *Store, p Project, kind string) (File, Job) {
	t.Helper()
	f, j, err := st.CreateUploadJob(context.Background(), File{ProjectID: p.ID, Kind: kind, OriginalName: "sample.csv", StoragePath: fmt.Sprintf("test-%d.csv", time.Now().UnixNano()), SizeBytes: 10}, 3)
	if err != nil {
		t.Fatal(err)
	}
	return f, j
}

func TestConcurrentClaimAndRecovery(t *testing.T) {
	st := integrationStore(t)
	p := testProject(t, st)
	_, created := testUpload(t, st, p, "baseline")
	ctx := context.Background()
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	claims := make(chan Job, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			j, err := st.ClaimNextJob(ctx, fmt.Sprintf("worker-%d", i), time.Minute)
			if err == nil {
				claims <- j
			}
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(claims)
	success, noJob := 0, 0
	var first Job
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrNoJob) {
			noJob++
		} else {
			t.Fatal(err)
		}
	}
	for j := range claims {
		first = j
	}
	if success != 1 || noJob != 1 || first.ID != created.ID {
		t.Fatalf("claims: success=%d noJob=%d job=%+v", success, noJob, first)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := st.ClaimNextJob(ctx, "replacement", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.ClaimToken == first.ClaimToken || second.Attempts != 2 {
		t.Fatalf("recovery claim: %+v", second)
	}
	if err := st.CompleteJob(ctx, first.ID, first.ClaimToken, "pass", json.RawMessage(`{}`)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("old claim should be rejected: %v", err)
	}
	if err := st.CompleteJob(ctx, second.ID, second.ClaimToken, "pass", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetReport(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteJob(ctx, second.ID, second.ClaimToken, "pass", json.RawMessage(`{}`)); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("completed claim should be rejected: %v", err)
	}
}

func TestApproveOnlyPassingBaselineAndSnapshot(t *testing.T) {
	st := integrationStore(t)
	p := testProject(t, st)
	f, j := testUpload(t, st, p, "baseline")
	ctx := context.Background()
	if _, err := st.ApproveBaseline(ctx, p.ID, f.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("early approval: %v", err)
	}
	claimed, err := st.ClaimNextJob(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != j.ID {
		t.Fatalf("claimed wrong job: %d", claimed.ID)
	}
	if err := st.CompleteJob(ctx, j.ID, claimed.ClaimToken, "pass", json.RawMessage(`{"validation":{"pass":true}}`)); err != nil {
		t.Fatal(err)
	}
	approved, err := st.ApproveBaseline(ctx, p.ID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.ApprovedBaselineFileID == nil || *approved.ApprovedBaselineFileID != f.ID {
		t.Fatal("baseline not approved")
	}
	_, candidate := testUpload(t, st, p, "candidate")
	if candidate.BaselineFileID == nil || *candidate.BaselineFileID != f.ID {
		t.Fatalf("candidate did not snapshot baseline: %+v", candidate)
	}
}
