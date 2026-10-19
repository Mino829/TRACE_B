package photoasset

import (
	"bytes"
	"context"
	"testing"

	"golang.org/x/image/vp8l"
	"golang.org/x/image/webp"
)

type webpBitWriter struct {
	data []byte
	bits uint
}

func (w *webpBitWriter) write(value uint32, count uint) {
	for i := range count {
		if w.bits&7 == 0 {
			w.data = append(w.data, 0)
		}
		if value&(1<<i) != 0 {
			w.data[len(w.data)-1] |= 1 << (w.bits & 7)
		}
		w.bits++
	}
}

// entropyGroupFixture is a complete, constant-color 2x2 lossless image. Its
// one-pixel entropy map selects group index groupIndex. Every tree is the
// single-symbol simple form, so literal pixels consume no additional bits.
// Before x/image v0.45, the 65535-index case made the decoder allocate 65,536
// groups (over 160 MiB of fixed lookup tables) from under 0.5 MiB of input.
func entropyGroupFixture(groupIndex int) []byte {
	var bits webpBitWriter
	bits.write(0x2f, 8)
	bits.write(1, 14) // width minus one
	bits.write(1, 14) // height minus one
	bits.write(0, 1)  // alpha hint
	bits.write(0, 3)  // version
	bits.write(0, 1)  // no transforms
	bits.write(0, 1)  // no color cache
	bits.write(1, 1)  // entropy image follows
	bits.write(0, 3)  // entropy tile size = 4 pixels
	bits.write(0, 1)  // entropy image has no color cache
	simple := func(symbol uint32) {
		bits.write(1, 1) // simple tree
		bits.write(0, 1) // one symbol
		bits.write(1, 1) // eight-bit first symbol
		bits.write(symbol, 8)
	}
	// RG channels of the entropy image carry the 16-bit group index. Trees
	// are encoded green, red, blue, alpha, distance, in that order.
	for _, symbol := range []uint32{uint32(groupIndex & 255), uint32(groupIndex >> 8), 0, 255, 0} {
		simple(symbol)
	}
	for range groupIndex + 1 {
		for _, symbol := range []uint32{17, 19, 23, 255, 0} {
			simple(symbol)
		}
	}
	return bits.data
}

func TestWebPRejectsExcessiveEntropyGroupsBeforeAllocation(t *testing.T) {
	lossy := webpFixture(t, lossyWebP)[12:]
	for _, groupIndex := range []int{2600, 65535} {
		malicious := entropyGroupFixture(groupIndex)
		if len(malicious) >= 512*1024 {
			t.Fatal("regression fixture must demonstrate compressed-size amplification")
		}
		if _, err := vp8l.Decode(bytes.NewReader(malicious)); err == nil {
			t.Fatal("lossless decoder accepted excessive entropy groups")
		}
		for _, data := range [][]byte{
			webpContainer(webpChunk("VP8L", malicious)),
			// ALPH compression method 1 omits the five-byte VP8L image header;
			// the WebP decoder synthesizes it from the validated canvas size.
			webpContainer(extendedWebP(0x10, 2, 2), webpChunk("ALPH", append([]byte{1}, malicious[5:]...)), lossy),
		} {
			if _, err := webp.Decode(bytes.NewReader(data)); err == nil {
				t.Fatal("WebP decoder accepted excessive entropy groups in image or ALPH")
			}
			requireInvalid(t, data, imageMetadata(data, "image/webp", 2, 2))
		}
	}
}

func TestWebPAcceptsEntropyGroupLimitBoundary(t *testing.T) {
	allowed := entropyGroupFixture(2599)
	lossy := webpFixture(t, lossyWebP)[12:]
	for _, data := range [][]byte{
		webpContainer(webpChunk("VP8L", allowed)),
		webpContainer(extendedWebP(0x10, 2, 2), webpChunk("ALPH", append([]byte{1}, allowed[5:]...)), lossy),
	} {
		if err := Validate(context.Background(), data, imageMetadata(data, "image/webp", 2, 2)); err != nil {
			t.Fatal("valid image at entropy-group boundary was rejected:", err)
		}
	}
}
