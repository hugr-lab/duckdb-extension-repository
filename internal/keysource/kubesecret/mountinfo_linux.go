//go:build linux

package kubesecret

import "os"

// readMountInfo reads this process's mount table.
func readMountInfo() ([]byte, error) { return os.ReadFile("/proc/self/mountinfo") }
