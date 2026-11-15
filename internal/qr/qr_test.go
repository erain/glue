// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package qr

import (
	"strings"
	"testing"
)

func TestQRRenderDeterministic(t *testing.T) {
	var badSyn byte
	for _, v := range versions {
		for j, root := 0, byte(1); j < v[2]; j, root = j+1, gfMul(root, 2) {
			var syn byte
			for _, b := range append([]byte(strings.Repeat("a", v[1]/v[3])), rsEncode([]byte(strings.Repeat("a", v[1]/v[3])), v[2])...) {
				syn = gfMul(syn, root) ^ b
			}
			badSyn |= syn
		}
	}
	for ti, in := range []string{"", strings.Repeat("x", 135), "https://ntfy.sh", "https://ntfy.sh/glue-0123456789abcdef0123456789abcdef", "https://glue.example.com/pair/" + strings.Repeat("ab", 50)} {
		out1, err1 := Render(in)
		out2, err2 := Render(in)
		rows := strings.Split(strings.TrimSuffix(out1, "\n"), "\n")
		full, sz := len([]rune(rows[0])), len([]rune(rows[0]))-8
		px := func(r, c int) bool {
			return strings.ContainsRune([2]string{"▀█", "▄█"}[r%2], []rune(rows[r/2])[c])
		}
		ok := badSyn == 0 && ((ti < 2 && err1 != nil) || (ti >= 2 && err1 == nil && err2 == nil && out1 == out2 && sz == [5]int{0, 0, 21, 29, 41}[ti] && px(sz-4, 12)))
		for idx := range map[bool]int{true: full * full}[ti >= 2 && ok] {
			r, c := idx/full, idx%full
			ok = ok && !((uint(r-4) >= uint(sz) || uint(c-4) >= uint(sz)) && px(r, c)) && (r >= 7 || c >= 7 || (px(4+r, 4+c) == (r == 0 || r == 6 || c == 0 || c == 6 || (uint(r-2) < 3 && uint(c-2) < 3)) && px(4+r, sz-3+c) == px(4+r, 4+c) && px(sz-3+r, 4+c) == px(4+r, 4+c)))
		}
		if !ok {
			t.Fatalf("QR check %d failed (err=%v/%v badSyn=%d)", ti, err1, err2, badSyn)
		}
	}
}
