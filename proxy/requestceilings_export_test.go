package proxy

// SetRequestCeilings swaps the two request-size ceilings and returns the previous pair, for the
// external proxy_test size tests.
func SetRequestCeilings(request, compaction int64) (prevRequest, prevCompaction int64) {
	prevRequest, prevCompaction = maxRequestBytes, maxCompactionRequestBytes
	maxRequestBytes, maxCompactionRequestBytes = request, compaction
	return
}
