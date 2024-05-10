// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package slices

import (
	"cmp"
	"iter"
)

// All returns an iterator over index-value pairs in the slice.
// The indexes range in the usual order, from 0 through len(s)-1.
func All[Slice ~[]E, E any](s Slice) iter.Seq2[int, E] {
	return func(yield func(int, E) bool) {
		for i, v := range s {
			if !yield(i, v) {
				return
			}
		}
	}
}

// Backward returns an iterator over index-value pairs in the slice,
// traversing it backward. The indexes range from len(s)-1 down to 0.
func Backward[Slice ~[]E, E any](s Slice) iter.Seq2[int, E] {
	return func(yield func(int, E) bool) {
		for i := len(s) - 1; i >= 0; i-- {
			if !yield(i, s[i]) {
				return
			}
		}
	}
}

// Values returns an iterator over the slice elements.
// starting with s[0].
func Values[Slice ~[]E, E any](s Slice) iter.Seq[E] {
	return func(yield func(E) bool) {
		for _, v := range s {
			if !yield(v) {
				return
			}
		}
	}
}

// AppendSeq appends the values from seq to the slice and
// returns the extended slice.
func AppendSeq[Slice ~[]E, E any](s Slice, seq iter.Seq[E]) Slice {
	for v := range seq {
		s = append(s, v)
	}
	return s
}

// Collect collects values from seq into a new slice and returns it.
func Collect[E any](seq iter.Seq[E]) []E {
	return AppendSeq([]E(nil), seq)
}

// Sorted collects values from seq into a new slice, sorts the slice,
// and returns it.
func Sorted[E cmp.Ordered](seq iter.Seq[E]) []E {
	s := Collect(seq)
	Sort(s)
	return s
}

// SortedFunc collects values from seq into a new slice, sorts the slice
// using the comparison function, and returns it.
func SortedFunc[E any](seq iter.Seq[E], cmp func(E, E) int) []E {
	s := Collect(seq)
	SortFunc(s, cmp)
	return s
}

// SortedStableFunc collects values from seq into a new slice.
// It then sorts the slice while keeping the original order of equal elements,
// using the comparison function to compare elements.
// It returns the new slice.
func SortedStableFunc[E any](seq iter.Seq[E], cmp func(E, E) int) []E {
	s := Collect(seq)
	SortStableFunc(s, cmp)
	return s
}
