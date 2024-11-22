// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package hkdf

import (
	"crypto/internal/fips140/hkdf"
	"crypto/internal/fips140only"
	"errors"
	"hash"
)

// Extract generates a pseudorandom key for use with [Expand] from an input
// secret and an optional independent salt.
//
// Only use this function if you need to reuse the extracted key with multiple
// Expand invocations and different context values. Most common scenarios,
// including the generation of multiple keys, should use [Key] instead.
func Extract[H hash.Hash](h func() H, secret, salt []byte) ([]byte, error) {
	if err := checkFIPS140Only(h, secret); err != nil {
		return nil, err
	}
	return hkdf.Extract(h, secret, salt), nil
}

// Expand derives a key from the given hash, key, and optional context info,
// returning a []byte of length keyLength that can be used as cryptographic key.
// The extraction step is skipped.
//
// The key should have been generated by [Extract], or be a uniformly
// random or pseudorandom cryptographically strong key. See RFC 5869, Section
// 3.3. Most common scenarios will want to use [Key] instead.
func Expand[H hash.Hash](h func() H, pseudorandomKey []byte, info string, keyLength int) ([]byte, error) {
	if err := checkFIPS140Only(h, pseudorandomKey); err != nil {
		return nil, err
	}

	limit := h().Size() * 255
	if keyLength > limit {
		return nil, errors.New("hkdf: requested key length too large")
	}

	return hkdf.Expand(h, pseudorandomKey, info, keyLength), nil
}

// Key derives a key from the given hash, secret, salt and context info,
// returning a []byte of length keyLength that can be used as cryptographic key.
// Salt and info can be nil.
func Key[Hash hash.Hash](h func() Hash, secret, salt []byte, info string, keyLength int) ([]byte, error) {
	if err := checkFIPS140Only(h, secret); err != nil {
		return nil, err
	}

	limit := h().Size() * 255
	if keyLength > limit {
		return nil, errors.New("hkdf: requested key length too large")
	}

	return hkdf.Key(h, secret, salt, info, keyLength), nil
}

func checkFIPS140Only[H hash.Hash](h func() H, key []byte) error {
	if !fips140only.Enabled {
		return nil
	}
	if len(key) < 112/8 {
		return errors.New("crypto/hkdf: use of keys shorter than 112 bits is not allowed in FIPS 140-only mode")
	}
	if !fips140only.ApprovedHash(h()) {
		return errors.New("crypto/hkdf: use of hash functions other than SHA-2 or SHA-3 is not allowed in FIPS 140-only mode")
	}
	return nil
}
