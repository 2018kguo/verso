package main

import (
	"archive/zip"
	"context"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A real child process drives supervisor tests; no mocked process accounting.
func TestProcessHelper(t *testing.T) {
	mode := os.Getenv("PDF_BENCH_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "sleep":
		time.Sleep(20 * time.Second)
	case "fork":
		cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
		cmd.Env = append(os.Environ(), "PDF_BENCH_HELPER=allocate")
		if e := cmd.Start(); e != nil {
			os.Exit(3)
		}
		fmt.Println(cmd.Process.Pid)
		time.Sleep(20 * time.Second)
	case "allocate":
		b := make([]byte, 96<<20)
		for i := range b {
			b[i] = byte(i)
		}
		fmt.Println(len(b))
		time.Sleep(20 * time.Second)
		fmt.Println(b[0])
	case "flood":
		for i := 0; i < 300000; i++ {
			fmt.Println(strings.Repeat("x", 80))
		}
	case "disk":
		_ = os.WriteFile("large", make([]byte, 4<<20), 0600)
		time.Sleep(20 * time.Second)
	case "zero":
	default:
		os.Exit(4)
	}
	os.Exit(0)
}
func helper(t *testing.T, mode string, timeout time.Duration, memory, disk int64) execution {
	t.Helper()
	t.Setenv("PDF_BENCH_HELPER", mode)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	return execute(context.Background(), timeout, memory, disk, t.TempDir(), os.Args[0], "-test.run=^TestProcessHelper$")
}
func TestProcessLimitsAndDescendants(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("resource ceilings are enforced only by the Linux runner")
	}
	for _, tc := range []struct {
		mode, want   string
		timeout      time.Duration
		memory, disk int64
	}{{"zero", "", time.Second, 256 << 20, 8 << 20}, {"sleep", "timeout", 100 * time.Millisecond, 256 << 20, 8 << 20}, {"fork", "memory_limit", 3 * time.Second, 64 << 20, 8 << 20}, {"flood", "output_limit", 5 * time.Second, 256 << 20, 8 << 20}, {"disk", "disk_limit", 3 * time.Second, 256 << 20, 1 << 20}} {
		t.Run(tc.mode, func(t *testing.T) {
			x := helper(t, tc.mode, tc.timeout, tc.memory, tc.disk)
			if x.Failure != tc.want {
				t.Fatalf("want %q, got %+v", tc.want, x)
			}
			if tc.mode == "fork" {
				if x.PeakRSS < 64<<20 {
					t.Fatalf("child RSS not counted: %d", x.PeakRSS)
				}
				pid, _ := strconv.Atoi(strings.TrimSpace(x.Stdout))
				if pid <= 0 {
					t.Fatal("missing child PID")
				}
				// SIGKILL delivery is asynchronous even after the parent is reaped.
				// Require the descendant to terminate within a bounded interval.
				deadline := time.Now().Add(time.Second)
				for {
					b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
					if len(b) == 0 || strings.Contains(string(b), ") Z ") {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("child survived: %s", b)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	x := execute(ctx, time.Second, 256<<20, 0, t.TempDir(), "/bin/sleep", "20")
	if x.Failure != "timeout" {
		t.Fatalf("cancellation was not refusal: %+v", x)
	}
	x = execute(context.Background(), time.Second, 256<<20, 0, t.TempDir(), "/does/not/exist")
	if x.Failure != "start" {
		t.Fatal(x)
	}
}
func TestContentAndVisualCannotTurnLossIntoSuccess(t *testing.T) {
	// A ten-point shift crosses an 18-point table row while remaining below
	// the old two-percent page tolerance. Keep physical distances visible.
	aPage := boxPage{Width: 612, Height: 792, Words: []boxWord{{Text: "123.45", XMin: 100, XMax: 130, YMin: 100, YMax: 110}}}
	bPage := boxPage{Width: 612, Height: 792, Words: []boxWord{{Text: "123.45", XMin: 100, XMax: 130, YMin: 110, YMax: 120}}}
	shift := positions(aPage, bPage)
	if shift.PositionedRecall == nil || *shift.PositionedRecall != 1 || shift.MaxDistancePt == nil || *shift.MaxDistancePt < 9.99 {
		t.Fatalf("row shift hidden: %+v", shift)
	}
	full := scoreText("alpha alpha beta 中文", "alpha beta 中文 extra")
	if full.Recall == nil || *full.Recall != .8 || full.Precision == nil || *full.Precision != .8 {
		t.Fatal(full)
	}
	empty := scoreText("", "")
	if empty.Recall != nil || empty.Precision != nil || empty.Status != "no_source_text" {
		t.Fatal(empty)
	}
	lost := scoreText("all words vanished", "")
	if lost.Recall == nil || *lost.Recall != 0 {
		t.Fatal(lost)
	}
	makePage := func(offset int) *image.NRGBA {
		p := image.NewNRGBA(image.Rect(0, 0, 100, 100))
		for y := 0; y < 100; y++ {
			for x := 0; x < 100; x++ {
				p.Set(x, y, color.White)
			}
		}
		for y := 10; y < 30; y++ {
			for x := 10 + offset; x < 30+offset; x++ {
				p.Set(x, y, color.Black)
			}
		}
		return p
	}
	a := makePage(0)
	same := inkMatch(a, a)
	shifted := inkMatch(a, makePage(50))
	missing := inkMatch(a, nil)
	if same.F1 == nil || *same.F1 != 1 || shifted.F1 == nil || *shifted.F1 != 0 || missing.F1 == nil || *missing.F1 != 0 || missing.SameDimensions {
		t.Fatalf("same=%+v shifted=%+v missing=%+v", same, shifted, missing)
	}
	blank := inkMatch(image.NewNRGBA(image.Rect(0, 0, 10, 10)), nil)
	if blank.F1 != nil {
		t.Fatal("blank page cannot establish fidelity")
	}
	page := boxPage{Width: 100, Height: 100, Words: []boxWord{{XMin: 10, YMin: 10, XMax: 20, YMax: 20, Text: "same"}, {XMin: 30, YMin: 10, XMax: 40, YMax: 20, Text: "same"}}}
	if sameSize(page, boxPage{Width: 200, Height: 200}) {
		t.Fatal("proportional page scaling concealed physical size loss")
	}
	p := positions(page, boxPage{Width: 100, Height: 100, Words: page.Words[:1]})
	if p.MatchedWords != 1 || p.PositionedRecall == nil || *p.PositionedRecall != .5 {
		t.Fatal(p)
	}
}
func TestDOCXReadersAndUntrustedOracles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sample.docx")
	f, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	z := zip.NewWriter(f)
	w, _ := z.Create("word/document.xml")
	fmt.Fprintf(w, `<w:document xmlns:w="%s"><w:body><w:p><w:r><w:t>inter</w:t></w:r><w:r><w:t>national</w:t></w:r></w:p><w:p><w:r><w:t>law</w:t></w:r></w:p></w:body></w:document>`, wordNS)
	z.Close()
	f.Close()
	s, e := wordText(p)
	if e != nil || strings.Join(tokens(s), " ") != "international law" {
		t.Fatalf("%q %v", s, e)
	}
	if _, e = readBoxes("garbage"); e == nil {
		t.Fatal("missing bbox output accepted")
	}
	if countPages("Pages: 0") != 0 || countPages("Pages: 12\n") != 12 {
		t.Fatal("page parser")
	}
	if stats("not json") != nil || stats(`{"error":"refused","pages":0}`).ReportsError != true {
		t.Fatal("converter status")
	}
}
func TestEvidenceMustStayOutsideGit(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.MkdirAll(filepath.Join(repo, ".git"), 0700)
	if _, e := outsideGit(filepath.Join(repo, "out")); e == nil {
		t.Fatal("evidence inside checkout accepted")
	}
	alias := filepath.Join(root, "alias")
	if e := os.Symlink(repo, alias); e != nil {
		t.Fatal(e)
	}
	if _, e := outsideGit(filepath.Join(alias, "out")); e == nil {
		t.Fatal("symlink bypass")
	}
	out := filepath.Join(root, "out")
	if _, e := newEvidenceDir(out); e != nil {
		t.Fatal(e)
	}
	if _, e := newEvidenceDir(out); e == nil {
		t.Fatal("overwrites evidence")
	}
}

func TestParagraphRecallIgnoresNumbering(t *testing.T) {
	s := scoreParagraphs("If any dispute arises the parties shall meet\nCustomer may terminate the agreement in part\n", "1.1.1 If any dispute arises the parties shall meet\nCustomer may terminate\nthe agreement in part\n")
	if s.Truth != 2 || s.Intact != 1 {
		t.Fatalf("%+v: spelled-out numbering is intact, a split paragraph is not", s)
	}
}

// A header part shows on every section that names or inherits it, its PAGE
// field reading each section's own number.
func TestHeaderTextCountsPerSection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sections.docx")
	f, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	z := zip.NewWriter(f)
	part := func(name, body string) {
		w, _ := z.Create(name)
		io.WriteString(w, body)
	}
	const r = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	sect := `<w:sectPr><w:footerReference w:type="default" r:id="rId1"/><w:pgNumType w:start="4"/></w:sectPr>`
	part("word/document.xml", fmt.Sprintf(`<w:document xmlns:w="%s" xmlns:r="%s"><w:body><w:p><w:pPr>%s</w:pPr><w:r><w:t>one</w:t></w:r></w:p><w:p><w:r><w:t>two</w:t></w:r></w:p><w:sectPr/></w:body></w:document>`, wordNS, r, sect))
	part("word/_rels/document.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="footer" Target="footer1.xml"/></Relationships>`)
	part("word/footer1.xml", fmt.Sprintf(`<w:ftr xmlns:w="%s"><w:p><w:r><w:t>Page </w:t></w:r><w:fldSimple w:instr=" PAGE "><w:r><w:t>1</w:t></w:r></w:fldSimple></w:p></w:ftr>`, wordNS))
	z.Close()
	f.Close()
	s, e := wordText(p)
	if got := strings.Join(tokens(s), " "); e != nil || got != "one two page 4 page 5" {
		t.Fatalf("%q %v", got, e)
	}
}
