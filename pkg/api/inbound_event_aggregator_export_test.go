package api

// FlushNowForTest exposes the internal flushNow method to the external
// api_test package so tests can deterministically drive a flush instead
// of waiting for the 60-second tick.
func (a *InboundEventAggregator) FlushNowForTest() {
	a.flushNow()
}
