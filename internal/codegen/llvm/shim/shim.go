// Package shim embeds the LLVM backend's C runtime shim.
package shim

import _ "embed"

//go:embed shim.c.txt
var file string

// File is the C source of the runtime shim, written to a temporary file named
// `shim.c` and linked with the emitted LLVM IR.
func File() string { return file }
