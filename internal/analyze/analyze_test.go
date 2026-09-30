package analyze

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{Columns: []ColumnRule{{"id", "string"}, {"age", "integer"}, {"target", "string"}}, IDColumn: "id", TargetColumn: "target", MaxRowDropPercent: 20, MaxMissingTargetIncreasePercent: 10}
}
func csvFile(t *testing.T, contents string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "input.csv")
	if err := os.WriteFile(name, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return name
}
func hasFinding(v Validation, code string) bool {
	for _, f := range v.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func TestAnalyzeValidAndInvalidCSV(t *testing.T) {
	cfg := testConfig()
	valid, err := Analyze(csvFile(t, "id,age,target\na,20,yes\nb,32,\n"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !valid.Pass || valid.RowCount != 2 || valid.MissingTargetCount != 1 || valid.TargetLabels["yes"] != 1 {
		t.Fatalf("unexpected valid result: %+v", valid)
	}
	bad, err := Analyze(csvFile(t, "id,age,target\na,twenty,yes\na,21,no\n,18,yes\nc,19\n"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"type_mismatch", "duplicate_id", "empty_id", "malformed_row"} {
		if !hasFinding(bad, code) {
			t.Errorf("missing %s: %+v", code, bad.Findings)
		}
	}
	if bad.Pass {
		t.Fatal("invalid CSV passed")
	}
}

func TestHeaderRulesAndExampleCap(t *testing.T) {
	cfg := testConfig()
	v, err := Analyze(csvFile(t, "id,id,target\na,b,yes\n"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(v, "duplicate_header") || !hasFinding(v, "missing_required_column") {
		t.Fatalf("header findings: %+v", v.Findings)
	}
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, "id"+string(rune('a'+i))+",bad,yes")
	}
	v, err = Analyze(csvFile(t, "id,age,target\n"+strings.Join(lines, "\n")+"\n"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range v.Findings {
		if f.Code == "type_mismatch" {
			if f.Count != 10 || len(f.Examples) != 5 {
				t.Fatalf("unexpected cap: %+v", f)
			}
			return
		}
	}
	t.Fatal("type finding absent")
}

func TestComparePassAndBlock(t *testing.T) {
	cfg := testConfig()
	base := Validation{Pass: true, RowCount: 10, MissingTargetCount: 1, TargetLabels: map[string]int64{"yes": 5, "no": 4}}
	pass := Validation{Pass: true, RowCount: 9, MissingTargetCount: 1, TargetLabels: map[string]int64{"yes": 4, "no": 4}}
	if result := Compare(base, pass, cfg); !result.Pass || len(result.Checks) != 3 {
		t.Fatalf("expected pass: %+v", result)
	}
	blocked := Validation{Pass: true, RowCount: 6, MissingTargetCount: 2, TargetLabels: map[string]int64{"yes": 2, "no": 1, "maybe": 1}}
	result := Compare(base, blocked, cfg)
	if result.Pass {
		t.Fatal("comparison should block")
	}
	for _, check := range result.Checks {
		if check.Pass {
			t.Errorf("expected %s to block", check.Code)
		}
	}
}
