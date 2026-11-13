package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type schemaScore struct {
	Status         string `json:"status"`
	Validator      string `json:"validator,omitempty"`
	ArtifactSHA256 string `json:"artifactSha256,omitempty"`
}
type pageScore struct {
	SourceSize   *pageSize      `json:"sourceSize"`
	RenderedSize *pageSize      `json:"renderedSize"`
	SamePageSize *bool          `json:"samePageSize"`
	Page         int            `json:"page"`
	Status       string         `json:"status"`
	Ink          *inkScore      `json:"ink"`
	Position     *positionScore `json:"position"`
}
type pageSize struct {
	Width  float64 `json:"widthPt"`
	Height float64 `json:"heightPt"`
}

func sameSize(a, b boxPage) bool {
	return math.Abs(a.Width-b.Width) <= .5 && math.Abs(a.Height-b.Height) <= .5
}

type converterStats struct {
	Pages        int     `json:"pages"`
	Characters   int     `json:"characters"`
	Tables       int     `json:"tables"`
	Images       int     `json:"images"`
	Warnings     int     `json:"warningCount"`
	Validated    bool    `json:"validated"`
	ElapsedMS    float64 `json:"elapsedMs"`
	ReportsError bool    `json:"reportsError"`
}
type result struct {
	SHA256          string               `json:"sha256"`
	Label           string               `json:"label,omitempty"`
	Features        []string             `json:"features,omitempty"`
	Repetition      int                  `json:"repetition"`
	Evidence        string               `json:"evidence"`
	Outcome         string               `json:"outcome"`
	Failures        []string             `json:"failures"`
	SourceStatus    string               `json:"sourceStatus"`
	SourcePages     int                  `json:"sourcePages"`
	RenderedPages   int                  `json:"renderedPages"`
	Schema          schemaScore          `json:"schema"`
	Text            textScore            `json:"text"`
	Paragraphs      *paragraphScore      `json:"paragraphs,omitempty"`
	VisualStatus    string               `json:"visualStatus"`
	Pages           []pageScore          `json:"pages"`
	Converter       *converterStats      `json:"converterReported"`
	Executions      map[string]execution `json:"executions"`
	WallMS          int64                `json:"wallMs"`
	OutputBytes     int64                `json:"outputBytes"`
	RefusalExpected bool                 `json:"refusalExpected"`
	RefusalObserved bool                 `json:"refusalObserved"`
}

var pageCountRE = regexp.MustCompile(`(?m)^Pages:\s+(\d+)\s*$`)

func countPages(s string) int {
	m := pageCountRE.FindStringSubmatch(s)
	if len(m) != 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
func stats(s string) *converterStats {
	var raw struct {
		Pages      int             `json:"pages"`
		Characters int             `json:"characters"`
		Tables     int             `json:"tables"`
		Images     int             `json:"images"`
		Warnings   json.RawMessage `json:"warnings"`
		Validated  bool            `json:"validated"`
		ElapsedMS  float64         `json:"elapsedMs"`
		Error      json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(s), &raw) != nil {
		return nil
	}
	n := 0
	var warnings []json.RawMessage
	if json.Unmarshal(raw.Warnings, &warnings) == nil {
		n = len(warnings)
	}
	e := string(raw.Error)
	return &converterStats{raw.Pages, raw.Characters, raw.Tables, raw.Images, n, raw.Validated, raw.ElapsedMS, e != "" && e != "null" && e != `""`}
}

func runCase(parent context.Context, c config, input string, repetition int, labels, expected map[string]fixture) (r result) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, c.Timeout)
	defer cancel()
	r = result{Repetition: repetition, Outcome: "unmeasured", SourceStatus: "unknown", Schema: schemaScore{Status: "unknown"}, Text: textScore{Status: "unmeasured"}, VisualStatus: "unmeasured", Executions: map[string]execution{}, Failures: []string{}, Pages: []pageScore{}, RefusalExpected: c.Probe != "normal"}
	sha, err := digest(input, c.Input<<20)
	if err != nil {
		r.Outcome = "input_limit_or_read_error"
		r.Failures = append(r.Failures, "input_hash")
		return r
	}
	r.SHA256 = sha
	if err = os.MkdirAll(filepath.Join(c.Out, sha), 0700); err != nil {
		r.Outcome = "evidence_io_error"
		return r
	}
	dir, err := os.MkdirTemp(filepath.Join(c.Out, sha), fmt.Sprintf("run-%04d-", repetition))
	if err != nil {
		r.Outcome = "evidence_io_error"
		return r
	}
	r.Evidence, _ = filepath.Rel(c.Out, dir)
	defer func() {
		r.WallMS = time.Since(started).Milliseconds()
		if e := writeJSON(filepath.Join(dir, "result.json"), r); e != nil {
			r.Outcome = "evidence_io_error"
			r.Failures = append(r.Failures, "write_result")
		}
	}()
	if x, ok := labels[sha]; ok {
		r.Label = x.ID
		r.Features = x.Features
	}
	if x, ok := expected[filepath.Base(input)]; ok && x.SHA256 != sha {
		r.Outcome = "corpus_hash_mismatch"
		r.Failures = append(r.Failures, "manifest_sha256")
		return r
	}
	stage := func(name string, args ...string) execution {
		x := execute(ctx, c.ToolTimeout, c.Memory<<20, c.Disk<<20, dir, args...)
		r.Executions[name] = x
		if err := os.WriteFile(filepath.Join(dir, name+".stdout.log"), []byte(x.Stdout), 0600); err != nil {
			r.Failures = append(r.Failures, name+":evidence_io")
		}
		if err := os.WriteFile(filepath.Join(dir, name+".stderr.log"), []byte(x.Stderr), 0600); err != nil {
			r.Failures = append(r.Failures, name+":evidence_io")
		}
		return x
	}
	fail := func(stage string, x execution) { r.Failures = append(r.Failures, stage+":"+x.Failure) }
	sourceInfo := stage("source-info", "pdfinfo", input)
	r.SourcePages = countPages(sourceInfo.Stdout)
	var sourceText string
	var sourceBoxes []boxPage
	if sourceInfo.Failure != "" || r.SourcePages == 0 {
		r.SourceStatus = "unreadable"
		fail("source_info", sourceInfo)
	} else if r.SourcePages > c.MaxPages {
		r.SourceStatus = "reference_page_limit"
		r.Failures = append(r.Failures, "reference_page_limit")
	} else {
		r.SourceStatus = "measured"
		x := stage("source-text", "pdftotext", "-enc", "UTF-8", input, "-")
		if x.Failure == "" {
			sourceText = x.Stdout
		} else {
			r.SourceStatus = "text_unmeasured"
			fail("source_text", x)
		}
		x = stage("source-bbox", "pdftotext", "-bbox", "-enc", "UTF-8", input, "-")
		if x.Failure == "" {
			sourceBoxes, err = readBoxes(x.Stdout)
			if err != nil || len(sourceBoxes) != r.SourcePages {
				r.Failures = append(r.Failures, "source_bbox_parse")
				sourceBoxes = nil
			}
		} else {
			fail("source_bbox", x)
		}
	}
	output := filepath.Join(dir, "output.docx")
	args := append([]string{c.Converter}, c.Args...)
	args = append(args, input, output)
	// Converter runtime has its own ceiling; all stages also share the case deadline.
	x := stage("converter", args...)
	r.Converter = stats(x.Stdout)
	conversionFailed := x.Failure != "" || (r.Converter != nil && r.Converter.ReportsError)
	if conversionFailed {
		if x.Failure != "" {
			fail("converter", x)
		} else {
			r.Failures = append(r.Failures, "converter:reported_error")
		}
	}
	info, err := os.Lstat(output)
	if os.IsNotExist(err) {
		r.RefusalObserved = conversionFailed
		r.Outcome = "missing_output"
		if conversionFailed {
			r.Outcome = "refused"
		}
		if x.Failure == "timeout" || x.Failure == "memory_limit" || x.Failure == "disk_limit" || x.Failure == "output_limit" {
			r.Outcome = x.Failure
		}
		return r
	}
	if err != nil || !info.Mode().IsRegular() {
		r.Outcome = "unsafe_output"
		return r
	}
	r.OutputBytes = info.Size()
	if info.Size() > 100<<20 || info.Size() <= 0 {
		r.Outcome = "output_size_limit"
		return r
	}
	outputHash, err := digest(output, 100<<20)
	if err != nil {
		r.Outcome = "output_read_error"
		return r
	}
	x = stage("validator", "dotnet", c.Validator, output, "--json")
	var validation struct {
		Ok        *bool  `json:"Ok"`
		Validator string `json:"Validator"`
		SHA256    string `json:"Sha256"`
	}
	if json.Unmarshal([]byte(x.Stdout), &validation) != nil || validation.Ok == nil || validation.SHA256 != outputHash || !strings.HasPrefix(validation.Validator, "microsoft-openxml:") {
		r.Outcome = "validator_unavailable"
		r.Failures = append(r.Failures, "validator_contract")
		return r
	}
	r.Schema.Validator = validation.Validator
	r.Schema.ArtifactSHA256 = validation.SHA256
	invalid := !*validation.Ok
	if invalid {
		r.Schema.Status = "invalid"
		r.Outcome = "invalid_output_published"
		if !c.ScoreInvalid {
			return r
		}
	}
	if x.Failure != "" && !invalid {
		r.Outcome = "validator_unavailable"
		fail("validator", x)
		return r
	}
	if !invalid {
		r.Schema.Status = "valid"
	}
	if conversionFailed && !c.ScoreInvalid {
		r.Outcome = "artifact_on_failure"
		return r
	}
	if r.Converter == nil {
		r.Failures = append(r.Failures, "converter_json_unreadable")
	}
	outputText, err := wordText(output)
	if err != nil {
		r.Text.Status = "extraction_failed"
		r.Failures = append(r.Failures, "docx_text")
	}
	if err == nil && r.SourceStatus == "measured" {
		r.Text = scoreText(sourceText, outputText)
	}
	if c.Truth != "" && err == nil {
		name := strings.TrimSuffix(filepath.Base(input), filepath.Ext(input)) + ".docx"
		if truth, e := wordText(filepath.Join(c.Truth, name)); e == nil {
			r.Paragraphs = scoreParagraphs(truth, outputText)
		}
	}
	if r.SourcePages == 0 || r.SourcePages > c.MaxPages {
		r.Outcome = "source_unmeasured"
		return r
	}
	renderDir := filepath.Join(dir, "rendered")
	if err = os.Mkdir(renderDir, 0700); err != nil {
		r.Outcome = "evidence_io_error"
		return r
	}
	rendered := filepath.Join(renderDir, "output.pdf")
	if c.Renderer == "word" {
		x = renderWithWord(ctx, c.ToolTimeout, output, rendered)
		r.Executions["word"] = x
	} else {
		profile := (&url.URL{Scheme: "file", Path: filepath.Join(dir, "lo-profile")}).String()
		x = stage("libreoffice", "libreoffice", "-env:UserInstallation="+profile, "--headless", "--convert-to", "pdf:writer_pdf_Export", "--outdir", renderDir, output)
	}
	if x.Failure != "" {
		r.Outcome = "render_failed"
		fail(c.Renderer, x)
		return r
	}
	x = stage("render-info", "pdfinfo", rendered)
	r.RenderedPages = countPages(x.Stdout)
	if x.Failure != "" || r.RenderedPages == 0 {
		r.Outcome = "render_failed"
		fail("render_info", x)
		return r
	}
	if r.RenderedPages > c.MaxPages {
		r.Outcome = "render_page_limit"
		return r
	}
	var renderedBoxes []boxPage
	x = stage("render-bbox", "pdftotext", "-bbox", "-enc", "UTF-8", rendered, "-")
	if x.Failure == "" {
		renderedBoxes, err = readBoxes(x.Stdout)
		if err != nil || len(renderedBoxes) != r.RenderedPages {
			renderedBoxes = nil
			r.Failures = append(r.Failures, "render_bbox_parse")
		}
	} else {
		fail("render_bbox", x)
	}
	r.VisualStatus = "measured"
	mismatch := r.SourcePages != r.RenderedPages
	// Compare the union of pages: missing/extra pages cannot disappear from averages.
	for page := 1; page <= max(r.SourcePages, r.RenderedPages); page++ {
		p := pageScore{Page: page, Status: "measured"}
		var a, b image.Image
		for side, pdf := range []string{input, rendered} {
			if side == 0 && page > r.SourcePages || side == 1 && page > r.RenderedPages {
				continue
			}
			prefix := filepath.Join(dir, fmt.Sprintf("page-%04d-%d", page, side))
			name := fmt.Sprintf("raster-%04d-%d", page, side)
			x = stage(name, "pdftoppm", "-f", strconv.Itoa(page), "-l", strconv.Itoa(page), "-singlefile", "-scale-to", "1400", "-r", "96", "-png", pdf, prefix)
			if x.Failure != "" {
				p.Status = "render_failed"
				fail(name, x)
				continue
			}
			im, e := loadPNG(prefix + ".png")
			if e != nil {
				p.Status = "decode_failed"
				r.Failures = append(r.Failures, name+":png")
				continue
			}
			if side == 0 {
				a = im
			} else {
				b = im
			}
		}
		if page > r.SourcePages {
			p.Status = "extra_page"
		}
		if page > r.RenderedPages {
			p.Status = "missing_page"
		}
		if a != nil || b != nil {
			s := inkMatch(a, b)
			p.Ink = &s
			if !s.SameDimensions || s.F1 != nil && *s.F1 < .90 {
				mismatch = true
			}
		}
		if page <= len(sourceBoxes) && page <= len(renderedBoxes) {
			a, b := sourceBoxes[page-1], renderedBoxes[page-1]
			p.SourceSize = &pageSize{a.Width, a.Height}
			p.RenderedSize = &pageSize{b.Width, b.Height}
			same := sameSize(a, b)
			p.SamePageSize = &same
			if !same {
				mismatch = true
			}
			s := positions(sourceBoxes[page-1], renderedBoxes[page-1])
			p.Position = &s
			if s.Status != "measured" {
				r.VisualStatus = "positions_unmeasured"
				r.Failures = append(r.Failures, "position_resource_limit")
			}
			if (s.PositionedRecall != nil && *s.PositionedRecall < .95) || (s.MaxDistancePt != nil && *s.MaxDistancePt > 3) {
				mismatch = true
			}
		}
		if p.Status == "render_failed" || p.Status == "decode_failed" {
			r.VisualStatus = "incomplete"
		}
		r.Pages = append(r.Pages, p)
		if ctx.Err() != nil {
			r.VisualStatus = "incomplete"
			r.Failures = append(r.Failures, "case_timeout")
			break
		}
	}
	if len(sourceBoxes) != r.SourcePages || len(renderedBoxes) != r.RenderedPages {
		r.VisualStatus = "positions_unmeasured"
	}
	if r.Text.Recall != nil && *r.Text.Recall < .99 || r.Text.Precision != nil && *r.Text.Precision < .99 {
		mismatch = true
	}
	r.Outcome = "measured"
	if mismatch {
		r.Outcome = "fidelity_mismatch"
	}
	if r.Text.Status == "no_source_text" {
		r.Failures = append(r.Failures, "editable_content_unverified")
	}
	if len(r.Failures) > 0 || r.VisualStatus != "measured" || r.Text.Status != "measured" {
		if !mismatch {
			r.Outcome = "incomplete_metrics"
		}
	}
	if r.RefusalExpected && !r.RefusalObserved {
		r.Failures = append(r.Failures, "probe_did_not_refuse")
	}
	if invalid {
		r.Outcome = "invalid_output_published" // scored for diagnosis only
	} else if conversionFailed {
		r.Outcome = "artifact_on_failure"
	}
	return r
}
