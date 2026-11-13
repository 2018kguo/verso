// verso-bench compares converters without importing their implementation.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type fixture struct {
	ID         string   `json:"id"`
	URL        string   `json:"url"`
	SHA256     string   `json:"sha256"`
	Features   []string `json:"features"`
	Provenance string   `json:"provenance"`
}
type manifest struct {
	Version  int       `json:"version"`
	Fixtures []fixture `json:"fixtures"`
}
type argsList []string

func (a *argsList) String() string     { return strings.Join(*a, " ") }
func (a *argsList) Set(s string) error { *a = append(*a, s); return nil }

type config struct {
	Converter   string        `json:"converter"`
	Args        argsList      `json:"converterArgs"`
	Validator   string        `json:"validatorDll"`
	Corpus      string        `json:"-"`
	Out         string        `json:"-"`
	Manifest    string        `json:"-"`
	Timeout     time.Duration `json:"caseTimeoutNs"`
	ToolTimeout time.Duration `json:"toolTimeoutNs"`
	RunTimeout  time.Duration `json:"runTimeoutNs"`
	Memory      int64         `json:"processMemoryMiB"`
	Disk        int64         `json:"caseDiskMiB"`
	Input       int64         `json:"maxInputMiB"`
	MaxPages    int           `json:"maxReferencePages"`
	Repeat      int           `json:"repeat"`
	Parallel    int           `json:"parallel"`
	Probe       string        `json:"probe"`
	Renderer    string        `json:"renderer"`
	Truth       string        `json:"-"`
	// Diagnostic: measure fidelity of schema-invalid output (Word may still open it).
	ScoreInvalid bool `json:"scoreInvalid"`
}
type suite struct {
	ConverterSHA256 string            `json:"converterSha256"`
	ValidatorSHA256 string            `json:"validatorDllSha256"`
	ManifestSHA256  string            `json:"manifestSha256,omitempty"`
	Version         int               `json:"version"`
	Started         string            `json:"started"`
	Config          config            `json:"config"`
	Tools           map[string]string `json:"tools"`
	Results         []result          `json:"results"`
	Counts          map[string]int    `json:"outcomeCounts"`
	Completed       int               `json:"completed"`
	Planned         int               `json:"planned"`
	Complete        bool              `json:"complete"`
	ElapsedMS       int64             `json:"elapsedMs"`
}

func main() {
	if err := cli(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
func cli(argv []string) error {
	if len(argv) == 0 {
		return errors.New("usage: verso-bench fetch|run|stress|rescore [flags]")
	}
	if argv[0] == "fetch" {
		return fetchCLI(argv[1:])
	}
	if argv[0] == "rescore" {
		return rescoreCLI(argv[1:])
	}
	if argv[0] != "run" && argv[0] != "stress" {
		return errors.New("unknown command")
	}
	f := flag.NewFlagSet(argv[0], flag.ContinueOnError)
	c := config{}
	f.StringVar(&c.Converter, "converter", "verso", "executable or wrapper accepting [flags] INPUT OUTPUT")
	f.Var(&c.Args, "converter-arg", "repeatable literal argument; use --converter-arg=--flag=value")
	f.StringVar(&c.Validator, "validator-dll", "", "independent Microsoft OpenXmlValidate.dll (required)")
	f.StringVar(&c.Corpus, "corpus", "", "PDF directory, recursively read; local/private directories supported")
	f.StringVar(&c.Out, "out", "", "new evidence directory outside every git checkout (required)")
	f.StringVar(&c.Manifest, "manifest", "", "optional pinned corpus manifest for labels/hash verification")
	f.DurationVar(&c.Timeout, "timeout", 3*time.Minute, "whole-case deadline, including oracles")
	f.DurationVar(&c.ToolTimeout, "tool-timeout", 30*time.Second, "per-subprocess deadline")
	f.DurationVar(&c.RunTimeout, "run-timeout", 30*time.Minute, "whole-suite deadline")
	f.Int64Var(&c.Memory, "memory-mib", 1536, "per-process-tree sampled RSS ceiling")
	f.Int64Var(&c.Disk, "disk-mib", 512, "per-case sampled disk ceiling")
	f.Int64Var(&c.Input, "input-mib", 100, "maximum input size, refuses larger files")
	f.IntVar(&c.MaxPages, "max-reference-pages", 100, "reference/render safety bound; exceeding it is unmeasured, never sampled")
	f.IntVar(&c.Repeat, "repeat", 1, "repetitions per file (stress)")
	f.IntVar(&c.Parallel, "parallel", 1, "parallel cases (stress), each with an isolated LibreOffice profile")
	f.StringVar(&c.Probe, "probe", "normal", "stress: normal, timeout, memory, pages; resource probes still count as refusals")
	f.StringVar(&c.Renderer, "renderer", "libreoffice", "libreoffice, or word (Microsoft Word for Mac, serialized)")
	f.BoolVar(&c.ScoreInvalid, "score-invalid", false, "diagnostic: keep measuring fidelity after invalid OOXML or a converter error; outcome stays invalid")
	f.StringVar(&c.Truth, "truth", "", "directory of NAME.docx sources for NAME.pdf inputs; scores paragraph recall")
	if err := f.Parse(argv[1:]); err != nil {
		return err
	}
	if c.Renderer != "libreoffice" && c.Renderer != "word" {
		return errors.New("unknown renderer")
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if c.Timeout <= 0 || c.ToolTimeout <= 0 || c.RunTimeout <= 0 || c.Memory <= 0 || c.Memory > 65536 || c.Disk <= 0 || c.Disk > 1<<20 || c.Input <= 0 || c.Input > 4096 || c.MaxPages <= 0 || c.MaxPages > 10000 || c.Repeat < 1 || c.Repeat > 10000 || c.Parallel < 1 || c.Parallel > 32 {
		return errors.New("invalid bounds")
	}
	if argv[0] == "run" && (c.Repeat != 1 || c.Parallel != 1 || c.Probe != "normal") {
		return errors.New("use stress for repetitions, parallelism or probes")
	}
	switch c.Probe {
	case "normal":
	case "timeout":
		c.Args = append(c.Args, "--timeout=1ms")
	case "pages":
		c.Args = append(c.Args, "--max-pages=1")
	case "memory":
		c.Memory = 32
	default:
		return errors.New("unknown probe")
	}
	if c.Corpus == "" || c.Out == "" || c.Validator == "" {
		return errors.New("--corpus, --out and --validator-dll are required")
	}
	var err error
	c.Corpus, err = filepath.Abs(c.Corpus)
	if err != nil {
		return err
	}
	c.Validator, err = filepath.Abs(c.Validator)
	if err != nil {
		return err
	}
	if s, e := os.Stat(c.Validator); e != nil || !s.Mode().IsRegular() {
		return errors.New("validator DLL does not exist")
	}
	c.Converter, err = exec.LookPath(c.Converter)
	if err != nil {
		return err
	}
	c.Converter, _ = filepath.Abs(c.Converter)
	tools := []string{"dotnet", "pdfinfo", "pdftotext", "pdftoppm", "libreoffice"}
	if c.Renderer == "word" {
		tools[4] = "osascript"
	}
	for _, tool := range tools {
		if _, err = exec.LookPath(tool); err != nil {
			return fmt.Errorf("required tool %s: %w", tool, err)
		}
	}
	c.Out, err = newEvidenceDir(c.Out)
	if err != nil {
		return err
	}
	return runSuite(c)
}

// Resolve existing symlinks before checking .git ancestors: evidence must not
// accidentally enter a checkout through an alias or a symlinked parent.
func outsideGit(path string) (string, error) {
	abs, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	cursor := abs
	var suffix []string
	for {
		_, e = os.Lstat(cursor)
		if e == nil {
			break
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return "", e
		}
		suffix = append(suffix, filepath.Base(cursor))
		cursor = parent
	}
	cursor, e = filepath.EvalSymlinks(cursor)
	if e != nil {
		return "", e
	}
	for p := cursor; ; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			return "", errors.New("corpus/evidence destinations must be outside git checkouts")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		cursor = filepath.Join(cursor, suffix[i])
	}
	return cursor, nil
}
func newEvidenceDir(path string) (string, error) {
	p, e := outsideGit(path)
	if e != nil {
		return "", e
	}
	if _, e = os.Lstat(p); !os.IsNotExist(e) {
		return "", errors.New("--out must be a new directory; never overwrite evidence")
	}
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return "", e
	}
	if e = os.Mkdir(p, 0700); e != nil {
		return "", e
	}
	return p, nil
}
func digest(path string, limit int64) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, limit+1))
	if e != nil {
		return "", e
	}
	if n > limit {
		return "", errors.New("input_limit")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	b = append(b, '\n')
	p := path + ".partial"
	if e = os.WriteFile(p, b, 0600); e != nil {
		return e
	}
	return os.Rename(p, path)
}
func readManifest(path string) (manifest, error) {
	var m manifest
	f, e := os.Open(path)
	if e != nil {
		return m, e
	}
	defer f.Close()
	e = json.NewDecoder(io.LimitReader(f, 4<<20)).Decode(&m)
	if e != nil {
		return m, e
	}
	if m.Version != 1 || len(m.Fixtures) == 0 {
		return m, errors.New("invalid manifest")
	}
	seen := map[string]bool{}
	for _, x := range m.Fixtures {
		sha, e := hex.DecodeString(x.SHA256)
		if e != nil || len(sha) != 32 || x.ID == "" || filepath.Base(x.ID) != x.ID || seen[x.ID] {
			return m, errors.New("invalid fixture ID or SHA256")
		}
		seen[x.ID] = true
	}
	return m, nil
}
func fetchCLI(args []string) error {
	f := flag.NewFlagSet("fetch", flag.ContinueOnError)
	var path, out string
	f.StringVar(&path, "manifest", "corpus.json", "pinned fixture manifest")
	f.StringVar(&out, "out", "", "destination outside git")
	if e := f.Parse(args); e != nil {
		return e
	}
	if out == "" {
		return errors.New("--out is required")
	}
	m, e := readManifest(path)
	if e != nil {
		return e
	}
	out, e = outsideGit(out)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(out, 0700); e != nil {
		return e
	}
	client := &http.Client{Timeout: 60 * time.Second}
	for _, x := range m.Fixtures {
		target := filepath.Join(out, x.ID+".pdf")
		if got, e := digest(target, 100<<20); e == nil && got == x.SHA256 {
			fmt.Println(x.ID, "cached")
			continue
		}
		if !strings.HasPrefix(x.URL, "https://raw.githubusercontent.com/") {
			return errors.New("manifest requires HTTPS raw.githubusercontent.com pinned URLs")
		}
		u := strings.Split(strings.TrimPrefix(x.URL, "https://raw.githubusercontent.com/"), "/")
		if len(u) < 4 || len(u[2]) != 40 {
			return errors.New("URL must pin a 40-character commit")
		}
		if _, e = hex.DecodeString(u[2]); e != nil {
			return errors.New("URL commit must be hexadecimal")
		}
		resp, e := client.Get(x.URL)
		if e != nil {
			return e
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("%s: HTTP %d", x.ID, resp.StatusCode)
		}
		p := target + ".partial"
		f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			resp.Body.Close()
			return e
		}
		h := sha256.New()
		n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 100<<20+1))
		ce := f.Close()
		resp.Body.Close()
		if e != nil || ce != nil || n > 100<<20 || hex.EncodeToString(h.Sum(nil)) != x.SHA256 {
			os.Remove(p)
			return fmt.Errorf("%s: fetch/hash/size failure", x.ID)
		}
		if e = os.Rename(p, target); e != nil {
			return e
		}
		fmt.Println(x.ID, x.SHA256)
	}
	return nil
}
func runSuite(c config) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), c.RunTimeout)
	defer cancel()
	labels := map[string]fixture{}
	expected := map[string]fixture{}
	if c.Manifest != "" {
		m, e := readManifest(c.Manifest)
		if e != nil {
			return e
		}
		for _, x := range m.Fixtures {
			labels[x.SHA256] = x
			expected[x.ID+".pdf"] = x
		}
	}
	var files []string
	err := filepath.WalkDir(c.Corpus, func(p string, d os.DirEntry, e error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".pdf") {
			files = append(files, p)
			if len(files)*c.Repeat > 10000 {
				return errors.New("suite exceeds 10000 cases")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no PDFs found")
	}
	sort.Strings(files)
	s := suite{Version: 1, Started: start.UTC().Format(time.RFC3339), Config: c, Tools: map[string]string{}, Counts: map[string]int{}, Planned: len(files) * c.Repeat}
	s.ConverterSHA256, err = digest(c.Converter, 512<<20)
	if err != nil {
		return err
	}
	s.ValidatorSHA256, err = digest(c.Validator, 100<<20)
	if err != nil {
		return err
	}
	if c.Manifest != "" {
		s.ManifestSHA256, err = digest(c.Manifest, 4<<20)
		if err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"dotnet", "--version"}, {"pdfinfo", "-v"}, {"pdftotext", "-v"}, {"pdftoppm", "-v"}, {"libreoffice", "--version"}} {
		r := execute(ctx, 5*time.Second, c.Memory<<20, 0, "", args...)
		s.Tools[args[0]] = strings.TrimSpace(r.Stdout + r.Stderr)
	}
	if _, e := exec.LookPath("fc-list"); e == nil {
		r := execute(ctx, 5*time.Second, c.Memory<<20, 0, "", "fc-list", "--format=%{family}\\n")
		families := strings.Split(strings.TrimSpace(r.Stdout), "\n")
		sort.Strings(families)
		unique := make([]string, 0, len(families))
		for _, family := range families {
			if len(unique) == 0 || unique[len(unique)-1] != family {
				unique = append(unique, family)
			}
		}
		s.Tools["font-families"] = strings.Join(unique, "\n")
	}
	type job struct {
		path   string
		repeat int
	}
	jobs := make(chan job)
	results := make(chan result)
	var wg sync.WaitGroup
	for i := 0; i < c.Parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				x := runCase(ctx, c, j.path, j.repeat, labels, expected)
				results <- x
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, p := range files {
			for i := 0; i < c.Repeat; i++ {
				select {
				case jobs <- job{p, i}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	for r := range results {
		s.Results = append(s.Results, r)
		s.Counts[r.Outcome]++
		s.Completed++
		s.ElapsedMS = time.Since(start).Milliseconds()
		s.Complete = s.Completed == s.Planned
		sort.Slice(s.Results, func(i, j int) bool { return s.Results[i].Evidence < s.Results[j].Evidence })
		if e := writeJSON(filepath.Join(c.Out, "report.json"), s); e != nil {
			return e
		}
		fmt.Printf("%s run=%d outcome=%s\n", r.SHA256, r.Repetition, r.Outcome)
	}
	if !s.Complete {
		return errors.New("suite deadline reached: report is incomplete")
	}
	return nil
}
