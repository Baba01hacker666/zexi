package driver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"zexi/internal/codegen/llvm"
	"zexi/internal/codegen/llvm/shim"
	"zexi/internal/mir"
	"zexi/internal/sema"
)

// CompileLLVM builds from typed program info through MIR to LLVM IR, links the
// generated IR with the backend's runtime shim into a native binary at
// outPath, and returns the emitted IR.
func CompileLLVM(info *sema.Info, outPath string, opts Options, emitIR bool) (string, error) {
	res := mir.Lower(info)
	if len(res.Unsupported) > 0 {
		return "", fmt.Errorf("llvm backend does not support: %v", res.Unsupported)
	}
	if err := res.Prog.Validate(); err != nil {
		return "", fmt.Errorf("lowering produced invalid MIR: %w", err)
	}
	ir, err := llvm.Generate(res.Prog, llvm.Options{DebugChecks: opts.OptLevel == "0"})
	if err != nil {
		return "", err
	}
	if emitIR {
		return ir, nil
	}
	dir, err := os.MkdirTemp("", "zexic-llvm-")
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	irFile := filepath.Join(dir, "out.ll")
	if err := os.WriteFile(irFile, []byte(ir), 0o644); err != nil {
		return "", fmt.Errorf("writing LLVM IR: %w", err)
	}
	shimFile := filepath.Join(dir, "shim.c")
	if err := os.WriteFile(shimFile, []byte(shim.File()), 0o644); err != nil {
		return "", fmt.Errorf("writing runtime shim: %w", err)
	}

	// Only clang (and LLVM-based drivers) can compile .ll input, so an
	// explicit CC of gcc can't be honored here; fall back to clang.
	cc := opts.CC
	if cc == "" {
		cc = os.Getenv("CC")
	}
	if cc == "" || !isClang(cc) {
		cc = "clang"
	}
	if _, err := exec.LookPath(cc); err != nil {
		return ir, fmt.Errorf("clang not found; the LLVM backend needs it (install clang or set CC=clang)")
	}
	args := []string{"-Wno-override-module", irFile, shimFile, "-o", outPath, "-lm", "-Wl,--gc-sections"}
	if runtime.GOOS == "darwin" {
		args = []string{"-Wno-override-module", irFile, shimFile, "-o", outPath, "-lm", "-Wl,-dead_strip"}
	}
	if opts.Pthreads {
		args = append(args, "-pthread")
	}
	cmd := exec.Command(cc, args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return ir, fmt.Errorf("llvm link (%s %s) failed: %w", cc, strings.Join(args, " "), err)
	}
	return ir, nil
}

// isClang reports whether the compiler name looks like an LLVM toolchain
// driver, the only kind that accepts `.ll` input.
func isClang(cc string) bool {
	base := filepath.Base(cc)
	return strings.Contains(base, "clang")
}
