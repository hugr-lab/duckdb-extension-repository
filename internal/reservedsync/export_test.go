package reservedsync

import "context"

// Read is read, for the network test.
func (s *Sync) Read(ctx context.Context) ([]string, string, error) { return s.read(ctx) }
