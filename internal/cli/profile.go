package cli

import (
	"errors"
	"os"
	"runtime"
	"runtime/pprof"
	"runtime/trace"

	"github.com/spf13/cobra"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// profileFlags backs --cpuprofile, --memprofile and --exectrace. Same
// package-level shape, reset on NewRootCommand, and HAZARD under
// t.Parallel() as debugFlag.
var profileFlags struct {
	cpu, mem, exec string
	// stops are what finishProfiles runs, in order, once the command
	// returns; each flushes and closes one output.
	stops []func() error
}

func addProfileFlags(root *cobra.Command) {
	profileFlags.cpu, profileFlags.mem, profileFlags.exec = "", "", ""
	profileFlags.stops = nil
	root.PersistentFlags().StringVar(&profileFlags.cpu, "cpuprofile", "",
		"write a CPU profile of the command to this file, for go tool pprof")
	root.PersistentFlags().StringVar(&profileFlags.mem, "memprofile", "",
		"write a heap profile, taken when the command finishes, to this file")
	root.PersistentFlags().StringVar(&profileFlags.exec, "exectrace", "",
		"write a Go execution trace of the command to this file, for go tool trace")
	root.PersistentPreRunE = func(*cobra.Command, []string) error { return startProfiles() }
}

// startProfiles begins the CPU profile and execution trace the flags ask
// for. A profile that cannot start fails the command before it runs.
func startProfiles() error {
	if path := profileFlags.cpu; path != "" {
		f, err := create(path, "CPU profile")
		if err != nil {
			return err
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			_ = f.Close()
			return kerrors.Wrap(err, kerrors.CodeUnexpected, "starting the CPU profile")
		}
		profileFlags.stops = append(profileFlags.stops, func() error {
			pprof.StopCPUProfile()
			return f.Close()
		})
	}
	if path := profileFlags.exec; path != "" {
		f, err := create(path, "execution trace")
		if err != nil {
			return err
		}
		if err := trace.Start(f); err != nil {
			_ = f.Close()
			return kerrors.Wrap(err, kerrors.CodeUnexpected, "starting the execution trace")
		}
		profileFlags.stops = append(profileFlags.stops, func() error {
			trace.Stop()
			return f.Close()
		})
	}
	return nil
}

// finishProfiles stops what startProfiles began and writes the heap
// profile. It runs whether the command succeeded or not: a failing command
// is often the one worth profiling.
func finishProfiles() error {
	var errs []error
	for _, stop := range profileFlags.stops {
		errs = append(errs, stop())
	}
	profileFlags.stops = nil
	if path := profileFlags.mem; path != "" {
		errs = append(errs, writeHeapProfile(path))
	}
	if err := errors.Join(errs...); err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "writing profiles")
	}
	return nil
}

func writeHeapProfile(path string) error {
	f, err := create(path, "heap profile")
	if err != nil {
		return err
	}
	// Up-to-date allocation statistics, as runtime/pprof documents.
	runtime.GC()
	if err := pprof.WriteHeapProfile(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func create(path, what string) (*os.File, error) {
	f, err := os.Create(path) //nolint:gosec // G304: a path the operator named on the command line
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "creating the %s file %s", what, path)
	}
	return f, nil
}
