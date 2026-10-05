package codegen_test

import "runtime"

// exeSuffix is what a built program's name ends with here: Windows runs
// only files named .exe (TENG-3171).
var exeSuffix = func() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}()
