package protocol

// PackMarshalCounts exposes the packer's marshal cost to tests so the O(n)
// guarantee is asserted rather than assumed.
func PackMarshalCounts() (elements, envelopes int) {
	return int(packElementMarshals.Load()), int(packEnvelopeMarshals.Load())
}

// BoundedSizeForTest exposes the marshal-free upper bound so its soundness —
// never under-estimating the real marshalled size — can be asserted directly.
func BoundedSizeForTest(gs []Goroutine, ts []Thread, deltas int) int {
	return boundedSize(gs, ts, deltas)
}

// PackAllowances exposes the fixed per-element allowances so a test can pin them
// against a real marshal and catch a newly added field.
func PackAllowances() (envelope, goroutine, thread, delta int) {
	return envelopeAllowance, goroutineAllowance, threadAllowance, deltaAllowance
}

// ResetPackMarshalCounts zeroes the counters before a measured pack.
func ResetPackMarshalCounts() {
	packElementMarshals.Store(0)
	packEnvelopeMarshals.Store(0)
}
