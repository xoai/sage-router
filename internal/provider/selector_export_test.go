package provider

import "math/rand"

// seedRNGForTest reseeds the SelectP2C sampling source so distribution tests
// are deterministic. It lives in a _test.go file so this test-only seam is
// excluded from production builds while staying accessible to package tests
// (M3 review minor — keep the seam out of the shipped binary).
func (s *Selector) seedRNGForTest(seed int64) {
	s.rngMu.Lock()
	defer s.rngMu.Unlock()
	s.rng = rand.New(rand.NewSource(seed))
}
