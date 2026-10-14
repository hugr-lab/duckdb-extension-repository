package app

import (
	"fmt"
	"runtime"
)

// Version is kista's version, set at build time (-ldflags "-X …/internal/app.Version=v1.2.3").
var Version = "dev"

// VersionLine is the version, the Go toolchain and the platform, as `kista version` prints them.
func VersionLine() string {
	return fmt.Sprintf("kista %s (%s, %s/%s)", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
