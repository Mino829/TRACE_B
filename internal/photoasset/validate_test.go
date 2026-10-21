package photoasset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"strings"
	"testing"
	"time"
)

// Real libwebp-encoded 2x2 RGB fixtures; the four pixels are the ASCII byte
// triples ABC, DEF, GHI, JKL. Tests exercise actual decoding, not stand-in codecs.
const losslessWebP = "UklGRjgAAABXRUJQVlA4TCwAAAAvAUAAAB9AkG0b2nazE2Tbhrad7ATZtqFtR/v8x//bbLMNyAYoU4VZRP9jBw=="
const lossyWebP = "UklGRiYAAABXRUJQVlA4IBoAAAAwAQCdASoCAAIAAgA0JaQAA3AA/vD6D54QAA=="

func jpegFixture(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, width, height))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 37)
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func webpFixture(t *testing.T, encoded string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func imageMetadata(data []byte, mime string, width, height int) Metadata {
	hash := sha256.Sum256(data)
	return Metadata{MIMEType: mime, SizeBytes: int64(len(data)), Width: width, Height: height, SHA256: hex.EncodeToString(hash[:])}
}

func requireInvalid(t *testing.T, data []byte, meta Metadata) {
	t.Helper()
	if err := Validate(context.Background(), data, meta); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("Validate error = %v, want invalid image", err)
	}
}

func TestValidateAllowedImages(t *testing.T) {
	for _, tc := range []struct {
		name, mime    string
		data          []byte
		width, height int
	}{
		{"jpeg", "image/jpeg", jpegFixture(t, 7, 11), 7, 11},
		{"jpeg maximum long edge", "image/jpeg", jpegFixture(t, MaxLongEdge, 1), MaxLongEdge, 1},
		{"webp lossless", "image/webp", webpFixture(t, losslessWebP), 2, 2},
		{"webp lossy", "image/webp", webpFixture(t, lossyWebP), 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := imageMetadata(tc.data, tc.mime, tc.width, tc.height)
			if err := Validate(context.Background(), tc.data, meta); err != nil {
				t.Fatal(err)
			}
			meta.SHA256 = strings.ToUpper(meta.SHA256)
			if err := Validate(context.Background(), tc.data, meta); err != nil {
				t.Fatal("hexadecimal hash should be case insensitive")
			}
		})
	}
}

func TestValidateMetadataBoundaries(t *testing.T) {
	data := jpegFixture(t, 7, 11)
	for _, tc := range []struct {
		name   string
		mutate func(*Metadata)
	}{
		{"wrong size", func(m *Metadata) { m.SizeBytes++ }},
		{"zero size", func(m *Metadata) { m.SizeBytes = 0 }},
		{"oversized declaration", func(m *Metadata) { m.SizeBytes = MaxBytes + 1 }},
		{"negative size", func(m *Metadata) { m.SizeBytes = -1 }},
		{"wrong width", func(m *Metadata) { m.Width++ }},
		{"wrong height", func(m *Metadata) { m.Height++ }},
		{"zero width", func(m *Metadata) { m.Width = 0 }},
		{"negative height", func(m *Metadata) { m.Height = -1 }},
		{"width too large", func(m *Metadata) { m.Width = MaxLongEdge + 1 }},
		{"height too large", func(m *Metadata) { m.Height = MaxLongEdge + 1 }},
		{"wrong hash", func(m *Metadata) { m.SHA256 = strings.Repeat("0", 64) }},
		{"nonhex hash", func(m *Metadata) { m.SHA256 = strings.Repeat("z", 64) }},
		{"short hash", func(m *Metadata) { m.SHA256 = m.SHA256[:63] }},
		{"long hash", func(m *Metadata) { m.SHA256 += "0" }},
		{"mime alias", func(m *Metadata) { m.MIMEType = "image/jpg" }},
		{"png", func(m *Metadata) { m.MIMEType = "image/png" }},
		{"mime parameters", func(m *Metadata) { m.MIMEType = "image/jpeg; charset=utf-8" }},
		{"wrong allowed mime", func(m *Metadata) { m.MIMEType = "image/webp" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := imageMetadata(data, "image/jpeg", 7, 11)
			tc.mutate(&meta)
			requireInvalid(t, data, meta)
		})
	}
	large := make([]byte, MaxBytes+1)
	requireInvalid(t, large, imageMetadata(large, "image/jpeg", 1, 1))
	wide := jpegFixture(t, MaxLongEdge+1, 1)
	requireInvalid(t, wide, imageMetadata(wide, "image/jpeg", MaxLongEdge+1, 1))
	requireInvalid(t, nil, imageMetadata(nil, "image/jpeg", 1, 1))
}

func jpegSegment(marker byte, payload []byte) []byte {
	segment := make([]byte, 4+len(payload))
	segment[0], segment[1] = 0xff, marker
	binary.BigEndian.PutUint16(segment[2:4], uint16(len(payload)+2))
	copy(segment[4:], payload)
	return segment
}

func withJPEGSegment(data, segment []byte) []byte {
	result := append(bytes.Clone(data[:2]), segment...)
	return append(result, data[2:]...)
}

func TestValidateJPEGMetadata(t *testing.T) {
	data := jpegFixture(t, 7, 11)
	jfif := []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")
	adobe := []byte("Adobe\x00\x64\x00\x00\x00\x00\x01")
	for _, tc := range []struct {
		name    string
		marker  byte
		payload []byte
	}{
		{"exif and embedded thumbnail", 0xe1, []byte("Exif\x00\x00II\x2a\x00private GPS and thumbnail")},
		{"xmp", 0xe1, []byte("http://ns.adobe.com/xap/1.0/\x00private")},
		{"icc", 0xe2, []byte("ICC_PROFILE\x00private")},
		{"comment", 0xfe, []byte("private comment")},
		{"unknown APPn", 0xef, []byte("private")},
		{"JFXX thumbnail", 0xe0, []byte("JFXX\x00thumbnail")},
		{"JFIF appended thumbnail", 0xe0, append(bytes.Clone(jfif), 1, 2, 3)},
		{"Adobe appended payload", 0xee, append(bytes.Clone(adobe), 'x')},
		{"arbitrary APP14", 0xee, []byte("secret data!")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := withJPEGSegment(data, jpegSegment(tc.marker, tc.payload))
			requireInvalid(t, encoded, imageMetadata(encoded, "image/jpeg", 7, 11))
		})
	}
	thumb := bytes.Clone(jfif)
	thumb[12] = 1
	encoded := withJPEGSegment(data, jpegSegment(0xe0, thumb))
	requireInvalid(t, encoded, imageMetadata(encoded, "image/jpeg", 7, 11))
	badAdobe := bytes.Clone(adobe)
	badAdobe[7] = 1
	encoded = withJPEGSegment(data, jpegSegment(0xee, badAdobe))
	requireInvalid(t, encoded, imageMetadata(encoded, "image/jpeg", 7, 11))
	for _, segment := range [][]byte{jpegSegment(0xe0, jfif), jpegSegment(0xee, adobe)} {
		encoded := withJPEGSegment(data, segment)
		if err := Validate(context.Background(), encoded, imageMetadata(encoded, "image/jpeg", 7, 11)); err != nil {
			t.Fatal("fixed structural browser header rejected:", err)
		}
	}
	// A byte sequence inside a quantization table is not a metadata marker.
	// A substring-based EXIF filter would reject this valid decodable JPEG.
	encoded = bytes.Clone(data)
	dqt := bytes.Index(encoded, []byte{0xff, 0xdb})
	if dqt < 0 {
		t.Fatal("fixture has no quantization table")
	}
	copy(encoded[dqt+5:], "Exif")
	if err := Validate(context.Background(), encoded, imageMetadata(encoded, "image/jpeg", 7, 11)); err != nil {
		t.Fatal("nonmetadata byte sequence rejected:", err)
	}
}

func webpChunk(kind string, payload []byte) []byte {
	chunk := make([]byte, 8+len(payload)+(len(payload)&1))
	copy(chunk, kind)
	binary.LittleEndian.PutUint32(chunk[4:8], uint32(len(payload)))
	copy(chunk[8:], payload)
	return chunk
}

func webpContainer(chunks ...[]byte) []byte {
	data := []byte("RIFF\x00\x00\x00\x00WEBP")
	for _, chunk := range chunks {
		data = append(data, chunk...)
	}
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	return data
}

func extendedWebP(flags byte, width, height int) []byte {
	payload := make([]byte, 10)
	payload[0] = flags
	for i := range 3 {
		payload[4+i], payload[7+i] = byte((width-1)>>(i*8)), byte((height-1)>>(i*8))
	}
	return webpChunk("VP8X", payload)
}

func TestValidateWebPMetadataAndContainers(t *testing.T) {
	lossy := webpFixture(t, lossyWebP)[12:]
	lossless := webpFixture(t, losslessWebP)[12:]
	for _, kind := range []string{"EXIF", "XMP ", "ICCP", "ANIM", "ANMF", "JUNK"} {
		t.Run(kind, func(t *testing.T) {
			for _, data := range [][]byte{
				webpContainer(webpChunk(kind, []byte("private")), lossless),
				webpContainer(lossless, webpChunk(kind, []byte("private"))),
			} {
				requireInvalid(t, data, imageMetadata(data, "image/webp", 2, 2))
			}
		})
	}
	for _, flags := range []byte{0x01, 0x02, 0x04, 0x08, 0x20, 0x40, 0x80} {
		data := webpContainer(extendedWebP(flags, 2, 2), lossless)
		requireInvalid(t, data, imageMetadata(data, "image/webp", 2, 2))
	}
	alpha := webpChunk("ALPH", []byte{0, 255, 128, 64, 0})
	for _, data := range [][]byte{
		webpContainer(extendedWebP(0, 2, 2), lossy),
		webpContainer(extendedWebP(0x10, 2, 2), alpha, lossy),
		webpContainer(extendedWebP(0, 2, 2), lossless),
	} {
		if err := Validate(context.Background(), data, imageMetadata(data, "image/webp", 2, 2)); err != nil {
			t.Fatal("structural extended WebP rejected:", err)
		}
	}
	for _, chunks := range [][][]byte{
		{extendedWebP(0, 2, 2), extendedWebP(0, 2, 2), lossy},
		{lossy, lossless},
		{alpha, lossy},
		{extendedWebP(0x10, 2, 2), lossy},
		{extendedWebP(0, 2, 2), alpha, lossy},
		{extendedWebP(0x10, 2, 2), alpha, lossless},
		{extendedWebP(0, 1, 1), lossless},
		{extendedWebP(0, MaxLongEdge+1, 2), lossy},
	} {
		data := webpContainer(chunks...)
		requireInvalid(t, data, imageMetadata(data, "image/webp", 2, 2))
	}
	// Extended canvas dimensions are not sufficient: a VP8L inner header can
	// otherwise request a huge allocation before the full decoder compares it.
	bomb := bytes.Clone(lossless)
	bomb[9], bomb[10], bomb[11], bomb[12] = 0xff, 0xff, 0xff, 0x0f
	data := webpContainer(extendedWebP(0, 2, 2), bomb)
	requireInvalid(t, data, imageMetadata(data, "image/webp", 2, 2))
	reserved := extendedWebP(0, 2, 2)
	reserved[9] = 1
	data = webpContainer(reserved, lossy)
	requireInvalid(t, data, imageMetadata(data, "image/webp", 2, 2))
	badPad := bytes.Clone(alpha)
	badPad[len(badPad)-1] = 1
	data = webpContainer(extendedWebP(0x10, 2, 2), badPad, lossy)
	requireInvalid(t, data, imageMetadata(data, "image/webp", 2, 2))
}

func TestValidateFullDecodeAndTruncation(t *testing.T) {
	jpegData := jpegFixture(t, 7, 11)
	sos := bytes.Index(jpegData, []byte{0xff, 0xda})
	if sos < 0 {
		t.Fatal("fixture has no scan")
	}
	scanStart := sos + 2 + int(binary.BigEndian.Uint16(jpegData[sos+2:sos+4]))
	broken := append(bytes.Clone(jpegData[:scanStart]), 0xff, 0xd9)
	if _, err := jpeg.DecodeConfig(bytes.NewReader(broken)); err != nil {
		t.Fatal("test must pass header-only JPEG decoding:", err)
	}
	requireInvalid(t, broken, imageMetadata(broken, "image/jpeg", 7, 11))
	for _, tc := range []struct {
		data          []byte
		mime          string
		width, height int
	}{
		{jpegData, "image/jpeg", 7, 11},
		{webpFixture(t, losslessWebP), "image/webp", 2, 2},
		{webpFixture(t, lossyWebP), "image/webp", 2, 2},
	} {
		for removed := 1; removed <= 16; removed++ {
			data := tc.data[:len(tc.data)-removed]
			requireInvalid(t, data, imageMetadata(data, tc.mime, tc.width, tc.height))
		}
		data := append(bytes.Clone(tc.data), []byte("trailing private data")...)
		requireInvalid(t, data, imageMetadata(data, tc.mime, tc.width, tc.height))
	}
	// Repair container lengths after truncation, so the lossless decoder must
	// detect missing bitstream data instead of relying on the RIFF length.
	webpData := webpFixture(t, losslessWebP)
	truncated := bytes.Clone(webpData[:len(webpData)-12])
	binary.LittleEndian.PutUint32(truncated[4:8], uint32(len(truncated)-8))
	binary.LittleEndian.PutUint32(truncated[16:20], uint32(len(truncated)-20))
	requireInvalid(t, truncated, imageMetadata(truncated, "image/webp", 2, 2))
}

func FuzzValidate(f *testing.F) {
	f.Add([]byte{0xff, 0xd8, 0xff, 0xd9}, true)
	data, _ := base64.StdEncoding.DecodeString(losslessWebP)
	f.Add(data, false)
	f.Fuzz(func(t *testing.T, data []byte, jpegFormat bool) {
		mime := "image/webp"
		if jpegFormat {
			mime = "image/jpeg"
		}
		// Every candidate has a matching digest and length, so fuzzing reaches
		// structural parsing and real decoders rather than just the hash guard.
		err := Validate(context.Background(), data, imageMetadata(data, mime, 2, 2))
		if err != nil && !errors.Is(err, ErrInvalidImage) {
			t.Fatal("unexpected validator error")
		}
	})
}

func TestWebPValidationWaitCanBeCancelled(t *testing.T) {
	data := webpFixture(t, losslessWebP)
	webpDecodeSlot <- struct{}{}
	defer func() { <-webpDecodeSlot }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- Validate(ctx, data, imageMetadata(data, "image/webp", 2, 2)) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled validation returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled validation remained blocked behind the decoder")
	}
}
