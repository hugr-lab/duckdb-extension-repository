package api

// SetMetaRate replaces the actor's metadata write rate (spec 0036), for tests.
func (h *Handler) SetMetaRate(rate float64, burst int) { h.metaLimiter = newLimiter(rate, burst) }
