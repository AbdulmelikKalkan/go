// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm || 386 || s390x

package sha1

import "unsafe"

//go:noescape
func doBlock(dig *digest, p *byte, n int)

func block(dig *digest, p []byte) {
	doBlock(dig, unsafe.SliceData(p), len(p))
}

func blockString(dig *digest, s string) {
	doBlock(dig, unsafe.StringData(s), len(s))
}
