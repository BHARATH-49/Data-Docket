package analyze

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const maxExamples = 5

type ColumnRule struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type Config struct {
	Columns                         []ColumnRule `json:"columns"`
	IDColumn                        string       `json:"id_column"`
	TargetColumn                    string       `json:"target_column"`
	MaxRowDropPercent               float64      `json:"max_row_drop_percent"`
	MaxMissingTargetIncreasePercent float64      `json:"max_missing_target_increase_percent"`
}

type Example struct {
	Row   int64  `json:"row"`
	Value string `json:"value,omitempty"`
}

type Finding struct {
	Code     string    `json:"code"`
	Message  string    `json:"message"`
	Column   string    `json:"column,omitempty"`
	Count    int64     `json:"count"`
	Examples []Example `json:"examples,omitempty"`
}

type Validation struct {
	Pass               bool             `json:"pass"`
	RowCount           int64            `json:"row_count"`
	MissingTargetCount int64            `json:"missing_target_count"`
	Headers            []string         `json:"headers"`
	TargetLabels       map[string]int64 `json:"target_labels"`
	Findings           []Finding        `json:"findings"`
}

type Check struct {
	Code           string `json:"code"`
	Column         string `json:"column,omitempty"`
	Pass           bool   `json:"pass"`
	BaselineValue  any    `json:"baseline_value"`
	CandidateValue any    `json:"candidate_value"`
	Threshold      any    `json:"threshold"`
	ObservedValue  any    `json:"observed_value"`
	Unit           string `json:"unit,omitempty"`
	Message        string `json:"message"`
}

type Comparison struct {
	Pass     bool      `json:"pass"`
	Checks   []Check   `json:"checks"`
	Findings []Finding `json:"findings,omitempty"`
}

func ValidateConfig(c Config) []Finding {
	var out []Finding
	add := func(code, message, column string) {
		out = append(out, Finding{Code: code, Message: message, Column: column, Count: 1})
	}
	if len(c.Columns) == 0 {
		add("missing_rules", "Define at least one required column.", "")
	}
	seen := map[string]bool{}
	for _, rule := range c.Columns {
		name := strings.TrimSpace(rule.Name)
		if name == "" {
			add("empty_rule_name", "A required column name is empty.", "")
			continue
		}
		if name != rule.Name {
			add("padded_rule_name", "Required column names must not start or end with spaces.", rule.Name)
		}
		if seen[name] {
			add("duplicate_rule", "A required column is defined more than once.", name)
		}
		seen[name] = true
		switch rule.Type {
		case "string", "integer", "int", "number", "float", "boolean", "bool", "date", "datetime":
		default:
			add("invalid_type", "Use string, integer, number, boolean, date, or datetime.", name)
		}
	}
	if !seen[c.IDColumn] {
		add("invalid_id_column", "ID column must be a required column.", c.IDColumn)
	}
	if !seen[c.TargetColumn] {
		add("invalid_target_column", "Target column must be a required column.", c.TargetColumn)
	}
	if c.IDColumn == c.TargetColumn && c.IDColumn != "" {
		add("same_id_target", "ID and target columns must differ.", c.IDColumn)
	}
	if math.IsNaN(c.MaxRowDropPercent) || math.IsInf(c.MaxRowDropPercent, 0) || c.MaxRowDropPercent < 0 || c.MaxRowDropPercent > 100 {
		add("invalid_threshold", "Row drop threshold must be between 0 and 100 percent.", "")
	}
	if math.IsNaN(c.MaxMissingTargetIncreasePercent) || math.IsInf(c.MaxMissingTargetIncreasePercent, 0) || c.MaxMissingTargetIncreasePercent < 0 || c.MaxMissingTargetIncreasePercent > 100 {
		add("invalid_threshold", "Missing target increase threshold must be between 0 and 100 percentage points.", "")
	}
	return out
}

func Analyze(path string, cfg Config) (Validation, error) {
	v := Validation{Pass: true, Headers: []string{}, TargetLabels: map[string]int64{}, Findings: []Finding{}}
	if issues := ValidateConfig(cfg); len(issues) != 0 {
		v.Pass = false
		v.Findings = issues
		return v, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return v, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	headers, err := r.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			appendFinding(&v, "missing_header", "CSV file has no header row.", "", 1, "")
			return v, nil
		}
		var pe *csv.ParseError
		if errors.As(err, &pe) {
			appendFinding(&v, "malformed_header", pe.Err.Error(), "", int64(pe.Line), "")
			return v, nil
		}
		return v, err
	}
	headers = slices.Clone(headers)
	if len(headers) > 0 {
		headers[0] = strings.TrimPrefix(headers[0], "\ufeff")
	}
	positions := map[string]int{}
	for i, h := range headers {
		h = strings.TrimSpace(h)
		headers[i] = h
		if h == "" {
			appendFinding(&v, "missing_header_name", "Header name is empty.", "", 1, "")
			continue
		}
		if _, ok := positions[h]; ok {
			appendFinding(&v, "duplicate_header", "Header name appears more than once.", h, 1, "")
			continue
		}
		positions[h] = i
	}
	v.Headers = headers
	for _, rule := range cfg.Columns {
		if _, ok := positions[rule.Name]; !ok {
			appendFinding(&v, "missing_required_column", "Required column is absent.", rule.Name, 1, "")
		}
	}
	ids := map[string]struct{}{}
	for {
		record, readErr := r.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		var pe *csv.ParseError
		if readErr != nil {
			if errors.As(readErr, &pe) {
				v.RowCount++
				appendFinding(&v, "malformed_row", pe.Err.Error(), "", int64(pe.Line), "")
				continue
			}
			return v, readErr
		}
		v.RowCount++
		line, _ := r.FieldPos(0)
		row := int64(line)
		if len(record) != len(headers) {
			appendFinding(&v, "malformed_row", fmt.Sprintf("Expected %d fields; found %d.", len(headers), len(record)), "", row, "")
			continue
		}
		if index, ok := positions[cfg.IDColumn]; ok {
			id := strings.TrimSpace(record[index])
			if id == "" {
				appendFinding(&v, "empty_id", "ID value is empty.", cfg.IDColumn, row, "")
			} else if _, exists := ids[id]; exists {
				appendFinding(&v, "duplicate_id", "ID value appears more than once.", cfg.IDColumn, row, id)
			} else {
				ids[id] = struct{}{}
			}
		}
		if index, ok := positions[cfg.TargetColumn]; ok {
			label := strings.TrimSpace(record[index])
			if label == "" {
				v.MissingTargetCount++
			} else {
				v.TargetLabels[label]++
			}
		}
		for _, rule := range cfg.Columns {
			index, ok := positions[rule.Name]
			if !ok {
				continue
			}
			value := strings.TrimSpace(record[index])
			if value != "" && !matchesType(value, rule.Type) {
				appendFinding(&v, "type_mismatch", "Nonempty value does not match configured type "+rule.Type+".", rule.Name, row, value)
			}
		}
	}
	if v.RowCount == 0 {
		appendFinding(&v, "empty_dataset", "CSV has no data rows.", "", 1, "")
	}
	return v, nil
}

func appendFinding(v *Validation, code, message, column string, row int64, value string) {
	v.Pass = false
	for i := range v.Findings {
		f := &v.Findings[i]
		if f.Code == code && f.Column == column && f.Message == message {
			f.Count++
			if row > 0 && len(f.Examples) < maxExamples {
				f.Examples = append(f.Examples, Example{Row: row, Value: value})
			}
			return
		}
	}
	f := Finding{Code: code, Message: message, Column: column, Count: 1}
	if row > 0 {
		f.Examples = []Example{{Row: row, Value: value}}
	}
	v.Findings = append(v.Findings, f)
}

func matchesType(value, kind string) bool {
	switch kind {
	case "string":
		return true
	case "integer", "int":
		_, err := strconv.ParseInt(value, 10, 64)
		return err == nil
	case "number", "float":
		n, err := strconv.ParseFloat(value, 64)
		return err == nil && !math.IsInf(n, 0) && !math.IsNaN(n)
	case "boolean", "bool":
		_, err := strconv.ParseBool(value)
		return err == nil
	case "date":
		_, err := time.Parse("2006-01-02", value)
		return err == nil
	case "datetime":
		_, err := time.Parse(time.RFC3339, value)
		return err == nil
	default:
		return false
	}
}

func Compare(base, candidate Validation, cfg Config) Comparison {
	c := Comparison{Pass: true, Checks: []Check{}}
	if !base.Pass || !candidate.Pass {
		c.Pass = false
		c.Findings = []Finding{{Code: "comparison_skipped", Message: "Comparison requires a valid candidate and approved baseline.", Count: 1}}
		return c
	}
	drop := 0.0
	if base.RowCount > 0 {
		drop = math.Max(0, float64(base.RowCount-candidate.RowCount)/float64(base.RowCount)*100)
	}
	rowPass := drop <= cfg.MaxRowDropPercent
	c.Checks = append(c.Checks, Check{Code: "row_count_drop", Pass: rowPass, BaselineValue: base.RowCount, CandidateValue: candidate.RowCount, Threshold: cfg.MaxRowDropPercent, ObservedValue: round(drop), Unit: "percent drop", Message: fmt.Sprintf("Row count dropped %.2f%%; allowed drop is %.2f%%.", drop, cfg.MaxRowDropPercent)})
	baseRate := float64(base.MissingTargetCount) / float64(base.RowCount) * 100
	candidateRate := float64(candidate.MissingTargetCount) / float64(candidate.RowCount) * 100
	increase := math.Max(0, candidateRate-baseRate)
	missingPass := increase <= cfg.MaxMissingTargetIncreasePercent
	c.Checks = append(c.Checks, Check{Code: "missing_target_increase", Column: cfg.TargetColumn, Pass: missingPass, BaselineValue: round(baseRate), CandidateValue: round(candidateRate), Threshold: cfg.MaxMissingTargetIncreasePercent, ObservedValue: round(increase), Unit: "percentage points", Message: fmt.Sprintf("Missing %s increased by %.2f percentage points; allowed increase is %.2f.", cfg.TargetColumn, increase, cfg.MaxMissingTargetIncreasePercent)})
	var newLabels []string
	for label := range candidate.TargetLabels {
		if _, ok := base.TargetLabels[label]; !ok {
			newLabels = append(newLabels, label)
		}
	}
	slices.Sort(newLabels)
	labelPass := len(newLabels) == 0
	c.Checks = append(c.Checks, Check{Code: "unseen_target_labels", Column: cfg.TargetColumn, Pass: labelPass, BaselineValue: sortedKeys(base.TargetLabels), CandidateValue: sortedKeys(candidate.TargetLabels), Threshold: "No new labels", ObservedValue: newLabels, Unit: "labels", Message: fmt.Sprintf("Candidate has %d target label(s) absent from the baseline: %s.", len(newLabels), strings.Join(newLabels, ", "))})
	c.Pass = rowPass && missingPass && labelPass
	return c
}

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
func round(n float64) float64 { return math.Round(n*100) / 100 }
