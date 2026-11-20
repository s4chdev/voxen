//go:build speex

package dsp

import "math"

// rms computes the root-mean-square level of a frame.
//
// It lives in a speex-tagged file because only the measurement tests need it; the
// default build should not carry helpers it never calls.
func rms(f []int16) float64 {
	if len(f) == 0 {
		return 0
	}
	var sum float64
	for _, v := range f {
		x := float64(v)
		sum += x * x
	}
	return math.Sqrt(sum / float64(len(f)))
}
