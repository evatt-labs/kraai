package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each flag writes its output, a failing command included: the command
// worth profiling is often the one that failed.
func TestProfileFlagsWriteTheirFiles(t *testing.T) {
	for name, args := range map[string][]string{
		"succeeding": {"version"},
		"failing":    {"plan", "kraai-profile-test", "--dir", "/nonexistent/manifest"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cpu, mem, exec := filepath.Join(dir, "cpu"), filepath.Join(dir, "mem"), filepath.Join(dir, "exec")
			_ = Execute(append(args, "--cpuprofile", cpu, "--memprofile", mem, "--exectrace", exec))
			for _, path := range []string{cpu, mem, exec} {
				info, err := os.Stat(path)
				if err != nil || info.Size() == 0 {
					t.Errorf("%s: %v, size %v", filepath.Base(path), err, info)
				}
			}
		})
	}
}

func TestAProfileThatCannotBeWrittenFailsTheCommand(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no", "such", "dir", "cpu")
	err := Execute([]string{"version", "--cpuprofile", missing})
	if err == nil || !strings.Contains(err.Error(), "CPU profile") {
		t.Fatalf("Execute = %v, want a named failure", err)
	}
	if err := Execute([]string{"version"}); err != nil {
		t.Fatalf("a later command without profiling: %v", err)
	}
}
