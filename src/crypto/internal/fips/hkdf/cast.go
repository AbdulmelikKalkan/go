// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package hkdf

import (
	"bytes"
	"crypto/internal/fips"
	_ "crypto/internal/fips/check"
	"crypto/internal/fips/sha256"
	"errors"
)

func init() {
	fips.CAST("HKDF-SHA2-256", func() error {
		input := []byte{
			0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
			0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		}
		want := []byte{
			0xb6, 0x53, 0x00, 0x5b, 0x51, 0x6d, 0x2b, 0xc9,
			0x4a, 0xe4, 0xf9, 0x51, 0x73, 0x1f, 0x71, 0x21,
			0xa6, 0xc1, 0xde, 0x42, 0x4f, 0x2c, 0x99, 0x60,
			0x64, 0xdb, 0x66, 0x3e, 0xec, 0xa6, 0x37, 0xff,
		}
		got := Key(sha256.New, input, input, input, len(want))
		if !bytes.Equal(got, want) {
			return errors.New("unexpected result")
		}
		return nil
	})
}
