// Command verso converts a PDF into an editable Word document.
//
//	verso [flags] input.pdf output.docx
//
// It prints a JSON report and exits 1 when it refuses. With --validate it
// publishes only a file Microsoft's Open XML SDK accepts.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/2018kguo/verso"
)

func main() {
	var opts verso.Options
	flag.StringVar(&opts.RectoModule, "recto", "", "path to @2018kguo/recto's dist/index.js (default: $VERSO_RECTO_MODULE, node_modules above this binary, npm root -g)")
	flag.StringVar(&opts.Node, "node", "node", "Node.js 24+ executable")
	flag.IntVar(&opts.MaxPages, "max-pages", 500, "maximum PDF pages")
	flag.Int64Var(&opts.MaxInputBytes, "max-bytes", 128<<20, "maximum input bytes")
	flag.DurationVar(&opts.Timeout, "timeout", 2*time.Minute, "total deadline, including validation")
	flag.BoolVar(&opts.AllowWarnings, "allow-warnings", false, "write the file even when a fidelity warning applies (a review copy)")
	validate := flag.String("validate", "", "verso-validate.dll: publish only output Microsoft's Open XML SDK accepts")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: verso [flags] input.pdf output.docx")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	if *validate != "" {
		opts.Validate = verso.Validator("dotnet", *validate)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	report, err := verso.Convert(ctx, flag.Arg(0), flag.Arg(1), opts)
	result := struct {
		verso.Report
		Error string `json:"error,omitempty"`
	}{Report: report}
	if err != nil {
		result.Error = err.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	if err != nil {
		os.Exit(1)
	}
}
