package protocol

// PackMarshalCounts exposes the packer's marshal cost to tests so the O(n)
// guarantee is asserted rather than assumed.
func PackMarshalCounts() (elements, envelopes int) {
	return int(packElementMarshals.Load()), int(packEnvelopeMarshals.Load())
}

// ResetPackMarshalCounts zeroes the counters before a measured pack.
func ResetPackMarshalCounts() {
	packElementMarshals.Store(0)
	packEnvelopeMarshals.Store(0)
}
