package main

import (
	"bytes"
	"os"
	"testing"
)

func TestDemux(t *testing.T) {
	for _, f := range []string{"testdata/ps264.mpg", "testdata/ps265.mpg"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Skip("sample recordings not present (make them with ffmpeg -f vob)")
		}
		raw = append(append([]byte("IMKH"), make([]byte, 36)...), raw...) // fake Hikvision header
		var out bytes.Buffer
		n, keys := 0, 0
		var first, last int64 = -1, 0
		err = demuxPS(bytes.NewReader(raw), func(pts int64, au []byte, key bool) error {
			if n == 0 {
				au = fromParamSets(au)
				if au == nil {
					t.Fatalf("%s: first AU has no param sets", f)
				}
			}
			if first < 0 {
				first = pts
			}
			last = pts
			n++
			if key {
				keys++
			}
			out.Write(au)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d AUs, %d keyframes, span %.2fs", f, n, keys, float64(last-first)/90000)
	}
}
