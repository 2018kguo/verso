package verso

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed reader.mjs
var reader string

type Options struct {
	RectoModule   string
	Node          string
	MaxPages      int
	MaxInputBytes int64
	Timeout       time.Duration
	// Validate checks the finished file before it is published; a failed or
	// unavailable validator leaves the destination untouched. Nil skips it
	// (the output is still schema-valid by construction; Report.Validated
	// says whether it was checked).
	Validate      func(context.Context, string) error
	AllowWarnings bool
}
type Report struct {
	Pages      int      `json:"pages"`
	Characters int      `json:"characters"`
	Tables     int      `json:"tables"`
	Images     int      `json:"images"`
	Warnings   []string `json:"warnings"`
	Validated  bool     `json:"validated"`
	ElapsedMS  int64    `json:"elapsedMs"`
}

// Extract runs the decoder with a deadline, a V8 heap limit and bounded I/O.
// The host must also enforce container RSS/CPU limits: WASM isn't V8 heap.
func Extract(ctx context.Context, path string, opts Options) ([]Page, error) {
	if opts.MaxPages == 0 {
		opts.MaxPages = 500
	}
	if opts.MaxPages < 1 || opts.MaxPages > 2000 {
		return nil, fmt.Errorf("max pages must be 1..2000")
	}
	if opts.MaxInputBytes == 0 {
		opts.MaxInputBytes = 128 << 20
	}
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Minute
	}
	if opts.Node == "" {
		opts.Node = "node"
	}
	if opts.RectoModule == "" {
		module, err := FindRecto()
		if err != nil {
			return nil, err
		}
		opts.RectoModule = module
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > opts.MaxInputBytes {
		return nil, fmt.Errorf("input exceeds byte limit or is not a regular file")
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, opts.Node, "--max-old-space-size=512", "--input-type=module", "--eval", reader, abs, opts.RectoModule, fmt.Sprint(opts.MaxPages))
	var stderr cappedBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	limited := &io.LimitedReader{R: stdout, N: (256 << 20) + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var pages []Page
	var decodeErr error
	for scanner.Scan() {
		if len(pages) >= opts.MaxPages {
			decodeErr = errors.New("page limit")
			break
		}
		var p Page
		if err = json.Unmarshal(scanner.Bytes(), &p); err != nil {
			decodeErr = fmt.Errorf("invalid decoder response: %w", err)
			break
		}
		p.Warnings = append(p.Warnings, p.sanitize()...)
		if err = p.validate(); err != nil {
			decodeErr = err
			break
		}
		pages = append(pages, p)
	}
	if decodeErr == nil {
		decodeErr = scanner.Err()
	}
	if limited.N == 0 {
		decodeErr = errors.New("decoder output exceeds 256 MiB limit")
	}
	if decodeErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if decodeErr != nil {
		return nil, decodeErr // cancel() above stopped the decoder; report why
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("decoder stopped: %w", ctx.Err())
	}
	if waitErr != nil {
		return nil, fmt.Errorf("PDF decoder failed: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	if len(pages) == 0 {
		return nil, errors.New("PDF contained no pages")
	}
	return pages, nil
}

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 8192 {
		keep := min(n, 8192-b.Len())
		_, _ = b.Buffer.Write(p[:keep])
	}
	return n, nil
}

// FindRecto locates the Recto decoder: $VERSO_RECTO_MODULE, the nearest
// node_modules above this executable, then the global npm root. Never the
// working directory: a converter run inside an upload must not load its code.
// The module must be the Recto 0.7 line that reader.mjs is written against.
func FindRecto() (string, error) {
	rel := filepath.Join("node_modules", "@2018kguo", "recto", "dist", "index.js")
	var candidates []string
	if m := os.Getenv("VERSO_RECTO_MODULE"); m != "" {
		candidates = append(candidates, m)
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
				candidates = append(candidates, filepath.Join(dir, rel))
				if filepath.Dir(dir) == dir {
					break
				}
			}
		}
	}
	if out, err := exec.Command("npm", "root", "-g").Output(); err == nil {
		candidates = append(candidates, filepath.Join(strings.TrimSpace(string(out)), "@2018kguo", "recto", "dist", "index.js"))
	}
	for _, m := range candidates {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(m), "..", "package.json"))
		if err != nil {
			continue
		}
		var pkg struct{ Name, Version string }
		if json.Unmarshal(data, &pkg) == nil && pkg.Name == "@2018kguo/recto" && strings.HasPrefix(pkg.Version, "0.7.") {
			return m, nil
		}
	}
	return "", errors.New("Recto 0.7 not found: npm install -g @2018kguo/recto@0.7, or pass its dist/index.js")
}

// Convert publishes only bytes that passed the validator. A same-filesystem
// hard link is atomic and refuses an existing destination, including races.
func Convert(ctx context.Context, input, output string, opts Options) (report Report, err error) {
	start := time.Now()
	defer func() { report.ElapsedMS = time.Since(start).Milliseconds() }()
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if _, err = os.Lstat(output); err == nil {
		return report, errors.New("destination already exists")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return report, err
	}
	pages, err := Extract(ctx, input, opts)
	if err != nil {
		return report, err
	}
	for i, p := range pages {
		for _, warning := range p.Warnings {
			report.Warnings = append(report.Warnings, fmt.Sprintf("page %d: %s", i+1, warning))
		}
	}
	if len(report.Warnings) > 0 && !opts.AllowWarnings {
		return report, errors.New("conversion requires review: see warnings; --allow-warnings produces a review copy")
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".verso-*.docx")
	if err != nil {
		return report, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	counts, err := write(ctx, tmp, pages)
	if err != nil {
		return report, err
	}
	report.Pages = counts.Pages
	report.Characters = counts.Characters
	report.Tables = counts.Tables
	report.Images = counts.Images
	if err = tmp.Close(); err != nil {
		return report, err
	}
	if opts.Validate != nil {
		before, err := os.ReadFile(tmp.Name())
		if err != nil {
			return report, err
		}
		if err = opts.Validate(ctx, tmp.Name()); err != nil {
			return report, fmt.Errorf("Word validation failed; output not published: %w", err)
		}
		after, err := os.ReadFile(tmp.Name())
		if err != nil {
			return report, err
		}
		if sha256.Sum256(before) != sha256.Sum256(after) {
			return report, errors.New("validator modified output; output not published")
		}
	}
	if ctx.Err() != nil {
		return report, ctx.Err()
	}
	if err = os.Link(tmp.Name(), output); err != nil {
		return report, err
	}
	report.Validated = opts.Validate != nil
	return report, nil
}
