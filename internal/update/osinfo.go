package update

import "runtime"

func goosWindows() bool { return runtime.GOOS == "windows" }
