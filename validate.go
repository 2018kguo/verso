package verso

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Validator invokes the repository's Microsoft SDK validator. Neither a zero
// exit code without JSON nor a repair/re-save counts as validated output.
func Validator(executable string, prefix ...string) func(context.Context, string) error {
	return func(ctx context.Context, path string) error {
		args := append(append([]string{}, prefix...), path, "--json")
		cmd := exec.CommandContext(ctx, executable, args...)
		var out, stderr cappedBuffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		err := cmd.Run()
		var result struct {
			Ok            bool
			RawErrorCount int
			OpenError     *string
			Validator     string
			Sha256        string
		}
		if decodeErr := json.Unmarshal(out.Bytes(), &result); decodeErr != nil {
			return fmt.Errorf("validator unavailable or response invalid: %v", err)
		}
		if err != nil || !result.Ok {
			if result.OpenError != nil {
				return fmt.Errorf("Microsoft validator could not open document: %s", *result.OpenError)
			}
			return fmt.Errorf("Microsoft validator rejected document (%d errors): %v", result.RawErrorCount, err)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !strings.HasPrefix(result.Validator, "microsoft-openxml:") || !strings.EqualFold(result.Sha256, fmt.Sprintf("%x", sha256.Sum256(data))) {
			return fmt.Errorf("validator identity or output digest mismatch")
		}
		return nil
	}
}
