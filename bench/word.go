package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Microsoft Word for Mac as the render oracle: it is the renderer people
// use, where LibreOffice is only a stand-in. Word is one process, so renders
// are serialized; it reads files only inside its sandbox container.
var wordMu sync.Mutex

const wordScript = `on run argv
  tell application "Microsoft Word"
    launch
    set n to count of documents
    open (POSIX file (item 1 of argv))
    repeat 240 times
      if (count of documents) > n then exit repeat
      delay 0.25
    end repeat
    if (count of documents) is n then error "Word did not open the document"
    set d to document 1
    save as d file name (item 2 of argv) file format format PDF
    close d saving no
  end tell
end run`

func renderWithWord(ctx context.Context, timeout time.Duration, docx, pdf string) execution {
	wordMu.Lock()
	defer wordMu.Unlock()
	home, err := os.UserHomeDir()
	if err != nil {
		return execution{Exit: -1, Failure: "start", Stderr: err.Error()}
	}
	box, err := os.MkdirTemp(filepath.Join(home, "Library/Containers/com.microsoft.Word/Data"), "verso-bench-")
	if err != nil {
		return execution{Exit: -1, Failure: "start", Stderr: err.Error()}
	}
	defer os.RemoveAll(box)
	in, out := filepath.Join(box, "output.docx"), filepath.Join(box, "output.pdf")
	data, err := os.ReadFile(docx)
	if err == nil {
		err = os.WriteFile(in, data, 0600)
	}
	if err != nil {
		return execution{Exit: -1, Failure: "start", Stderr: err.Error()}
	}
	x := execute(ctx, timeout, 0, 0, box, "osascript", "-e", wordScript, in, out)
	if x.Failure != "" {
		// Word refusing a file shows a modal alert, which blocks every later
		// render until answered. Record it as the failure and dismiss it.
		if alerts := dismissWordAlerts(); len(alerts) > 0 {
			x.Failure, x.Stderr = "alert", strings.Join(alerts, "\n")
		}
		// A hung Word holds every later render; close what it has open.
		_ = execute(context.Background(), 20*time.Second, 0, 0, "", "osascript", "-e", `tell application "Microsoft Word" to close every document saving no`)
		return x
	}
	if data, err = os.ReadFile(out); err == nil {
		err = os.WriteFile(pdf, data, 0600)
	}
	if err != nil {
		x.Failure, x.Stderr = "exit", fmt.Sprint(err)
	}
	return x
}

type paragraphScore struct {
	Truth  int      `json:"truthParagraphs"`
	Intact int      `json:"intactParagraphs"`
	Recall *float64 `json:"recall"`
}

var numbering = regexp.MustCompile(`^(?:\d{1,3}|[a-z]|[ivx]{1,4})$`)

// paragraphs splits wordText output: one line per Word paragraph or cell.
// Leading list numbering is dropped on both sides: a source DOCX numbers
// with w:numPr while its PDF, and so the conversion, spells "1.1.1" out.
func paragraphs(text string, minWords int) []string {
	var out []string
	for _, p := range strings.Split(text, "\n") {
		t := tokens(p)
		for i := 0; i < 4 && len(t) > 0 && numbering.MatchString(t[0]); i++ {
			t = t[1:]
		}
		if len(t) >= minWords {
			out = append(out, strings.Join(t, " "))
		}
	}
	return out
}

// A source paragraph is intact when one output paragraph carries exactly its
// words: an editor can then select, replace or comment on it as one unit.
// Lines split into frames, or neighbours merged together, both miss.
func scoreParagraphs(truth, output string) *paragraphScore {
	want := paragraphs(truth, 6)
	if len(want) == 0 {
		return nil
	}
	have := map[string]int{}
	for _, p := range paragraphs(output, 1) {
		have[p]++
	}
	s := &paragraphScore{Truth: len(want)}
	for _, p := range want {
		if have[p] > 0 {
			have[p]--
			s.Intact++
		}
	}
	s.Recall = ratio(s.Intact, s.Truth)
	return s
}

// rescoreCLI recomputes paragraph recall for a finished run from the DOCX it
// kept as evidence, so a metric fix needs no second render.
func rescoreCLI(args []string) error {
	f := flag.NewFlagSet("rescore", flag.ContinueOnError)
	truth := f.String("truth", "", "directory of NAME.docx sources (required)")
	corpus := f.String("corpus", "", "the run's PDF directory, to map hashes to names (required)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *truth == "" || *corpus == "" || f.NArg() != 1 {
		return errors.New("usage: rescore --truth DIR --corpus DIR RUN")
	}
	names := map[string]string{}
	_ = filepath.WalkDir(*corpus, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".pdf") {
			if sha, e := digest(path, 1<<30); e == nil {
				names[sha] = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			}
		}
		return nil
	})
	path := filepath.Join(f.Arg(0), "report.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var s suite
	if err = json.Unmarshal(data, &s); err != nil {
		return err
	}
	for i, r := range s.Results {
		out, e := wordText(filepath.Join(f.Arg(0), r.Evidence, "output.docx"))
		want, e2 := wordText(filepath.Join(*truth, names[r.SHA256]+".docx"))
		if e == nil && e2 == nil {
			s.Results[i].Paragraphs = scoreParagraphs(want, out)
		}
	}
	return writeJSON(path, s)
}

// dismissWordAlerts reads and dismisses Word's modal alerts through the
// agent-desktop CLI when it is installed (macOS accessibility), answering No
// to a recovery offer so a damaged file is never silently repaired.
func dismissWordAlerts() []string {
	if _, err := exec.LookPath("agent-desktop"); err != nil {
		return nil
	}
	type node struct {
		Role     string `json:"role"`
		Name     string `json:"name"`
		Value    string `json:"value"`
		Ref      string `json:"ref_id"`
		Children []node `json:"children"`
	}
	var alerts []string
	for range 50 {
		out, err := exec.Command("agent-desktop", "snapshot", "--app", "Microsoft Word", "--compact").Output()
		var snap struct {
			OK   bool `json:"ok"`
			Data struct {
				Tree node `json:"tree"`
			} `json:"data"`
		}
		if err != nil || json.Unmarshal(out, &snap) != nil || !snap.OK || snap.Data.Tree.Name != "alert" {
			return alerts
		}
		buttons := map[string]string{}
		var walk func(n node)
		walk = func(n node) {
			if n.Role == "statictext" && n.Value != "Microsoft Word" {
				alerts = append(alerts, n.Value)
			}
			if n.Ref != "" {
				buttons[n.Name] = n.Ref
			}
			for _, c := range n.Children {
				walk(c)
			}
		}
		walk(snap.Data.Tree)
		ref := cmp.Or(buttons["No"], buttons["OK"], buttons["Cancel"])
		if ref == "" || exec.Command("agent-desktop", "click", ref).Run() != nil {
			return alerts
		}
		time.Sleep(time.Second)
	}
	return alerts
}
