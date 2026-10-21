//go:build !linux

package kubesecret

// readMountInfo: other platforms have no /proc/self/mountinfo, so the kind is refused.
func readMountInfo() ([]byte, error) { return nil, ErrUnsupported }
