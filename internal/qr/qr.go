// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

// Package qr renders byte-mode QR codes (Versions 1-6, ECC Level L) for terminals.
package qr

import (
	"fmt"
	"slices"
	"strings"
)

var versions = [][4]int{{21, 19, 7, 1}, {25, 34, 10, 1}, {29, 55, 15, 1}, {33, 80, 20, 1}, {37, 108, 26, 1}, {41, 136, 18, 2}}

// Render encodes text as a QR code with a 4-module quiet zone using UTF-8 half-blocks.
func Render(text string) (string, error) {
	vi := slices.IndexFunc(versions, func(v [4]int) bool { return len(text) <= v[1]-2 })
	if text == "" || vi < 0 {
		return "", fmt.Errorf("qr: input length %d outside 1..134 bytes", len(text))
	}
	sz, dLen, ecLen, blks := versions[vi][0], versions[vi][1], versions[vi][2], versions[vi][3]
	data, k, cw := append([]byte{0x40 | byte(len(text)>>4), byte(len(text) << 4)}, make([]byte, dLen-2)...), dLen/blks, make([]byte, dLen+ecLen*blks)
	for i, b := range []byte(text) {
		data[1+i], data[2+i] = data[1+i]|(b>>4), b<<4
	}
	for i := 2 + len(text); i < dLen; i++ {
		data[i] = [2]byte{0xec, 0x11}[(i-len(text))&1]
	}
	for b := range blks {
		for i, v := range append(data[b*k:(b+1)*k:(b+1)*k], rsEncode(data[b*k:(b+1)*k], ecLen)...) {
			cw[map[bool]int{true: i*blks + b, false: dLen + (i-k)*blks + b}[i < k]] = v
		}
	}
	dark, res := make([]bool, sz*sz), make([]bool, sz*sz)
	dist := func(r0, c0, r1, c1 int) int { return max(r0-r1, r1-r0, c0-c1, c1-c0) }
	for idx := range sz * sz {
		r, c := idx/sz, idx%sz
		df, da := min(dist(r, c, 3, 3), dist(r, c, 3, sz-4), dist(r, c, sz-4, 3)), dist(r, c, sz-7, sz-7)
		res[idx] = df <= 4 || (sz >= 25 && da <= 2) || r == 6 || c == 6
		dark[idx] = (df <= 4 && df != 2 && df != 4) || (df > 4 && ((sz >= 25 && da <= 2 && da != 1) || ((sz < 25 || da > 2) && (r == 6 || c == 6) && (r+c)%2 == 0)))
	}
	for k, c := range [8]int{0, 1, 2, 3, 4, 5, 7, 8} {
		r := [8]int{8, 7, 5, 4, 3, 2, 1, 0}[k]
		res[r*sz+8], dark[r*sz+8], res[(sz-1-k)*sz+8], dark[(sz-1-k)*sz+8] = true, (0x77c4>>(7-k))&1 != 0, true, (0x77c4>>(14-k))&1 != 0
		res[8*sz+c], dark[8*sz+c], res[8*sz+sz-8+k], dark[8*sz+sz-8+k] = true, (0x77c4>>(14-k))&1 != 0, true, (0x77c4>>(7-k))&1 != 0
	}
	for bit, s := 0, 0; s < (sz/2)*sz*2; s++ {
		p, m := s/(sz*2), s%(sz*2)
		if r, c := map[bool]int{true: sz - 1 - m/2, false: m / 2}[p%2 == 0], sz-1-2*p-map[bool]int{true: 1}[2*p >= sz-7]-m%2; !res[r*sz+c] {
			dark[r*sz+c], bit = (bit < len(cw)*8 && (cw[bit/8]>>(7-bit%8))&1 != 0) != ((r+c)%2 == 0), bit+1
		}
	}
	var sb strings.Builder
	px := func(r, c int) int {
		return map[bool]int{true: 1}[uint(r-4) < uint(sz) && uint(c-4) < uint(sz) && dark[(r-4)*sz+c-4]]
	}
	for r := 0; r < sz+8; r += 2 {
		for c := range sz + 8 {
			sb.WriteRune([4]rune{' ', '▄', '▀', '█'}[px(r, c)*2+px(r+1, c)])
		}
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}

func gfMul(a, b byte) (p byte) {
	for ; b > 0; a, b = (a<<1)^(0x1d*(a>>7)), b>>1 {
		p ^= a * (b & 1)
	}
	return p
}

func rsEncode(data []byte, ecLen int) []byte {
	gen, rem := append([]byte{1}, make([]byte, ecLen)...), make([]byte, ecLen)
	for i, r := 0, byte(1); i < ecLen; i, r = i+1, gfMul(r, 2) {
		for j := i + 1; j > 0; j-- {
			gen[j] ^= gfMul(gen[j-1], r)
		}
	}
	for _, b := range data {
		lead := b ^ rem[0]
		for j := range ecLen {
			rem[j] = map[bool]byte{true: rem[min(j+1, ecLen-1)]}[j+1 < ecLen] ^ gfMul(gen[j+1], lead)
		}
	}
	return rem
}
