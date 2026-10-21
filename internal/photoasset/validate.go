package photoasset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/jpeg"

	"golang.org/x/image/vp8"
	"golang.org/x/image/vp8l"
	"golang.org/x/image/webp"
)

// WebP entropy tables have upstream allocation limits, but several simultaneous
// decodes can still exceed a 512MiB instance. Waiters retain only encoded bytes.
var webpDecodeSlot = make(chan struct{}, 1)

// Validate accepts only fully decodable, single-frame JPEG/WebP images without
// user metadata. Marker/chunk parsing, rather than searching pixel bytes, keeps
// encoded pixel data from being mistaken for EXIF, XMP, or other metadata.
func Validate(ctx context.Context, data []byte, meta Metadata) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	expected, valid := metadataHash(meta)
	if !valid || int64(len(data)) != meta.SizeBytes || sha256.Sum256(data) != expected {
		return ErrInvalidImage
	}

	var config image.Config
	var decoded image.Image
	var err error
	switch meta.MIMEType {
	case "image/jpeg":
		if !cleanJPEG(data) {
			return ErrInvalidImage
		}
		config, err = jpeg.DecodeConfig(bytes.NewReader(data))
	case "image/webp":
		if !cleanWebP(data, meta.Width, meta.Height) {
			return ErrInvalidImage
		}
		config, err = webp.DecodeConfig(bytes.NewReader(data))
	default:
		return ErrInvalidImage
	}
	if err != nil || config.Width != meta.Width || config.Height != meta.Height {
		return ErrInvalidImage
	}
	// Both the container and inner WebP bitstream dimensions were checked before
	// a decoder can allocate the pixel buffer. JPEG DecodeConfig reads the SOF.
	// Official x/image v0.47 includes the VP8L entropy-table allocation fix for
	// both image and ALPH. Serialize full WebP decoding within this process.
	if meta.MIMEType == "image/webp" {
		select {
		case webpDecodeSlot <- struct{}{}:
			defer func() { <-webpDecodeSlot }()
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if meta.MIMEType == "image/jpeg" {
		decoded, err = jpeg.Decode(bytes.NewReader(data))
	} else {
		decoded, err = webp.Decode(bytes.NewReader(data))
	}
	if err != nil || decoded.Bounds().Dx() != meta.Width || decoded.Bounds().Dy() != meta.Height {
		return ErrInvalidImage
	}
	return nil
}

func validMetadata(meta Metadata) bool {
	_, valid := metadataHash(meta)
	return valid
}

func metadataHash(meta Metadata) ([sha256.Size]byte, bool) {
	var hash [sha256.Size]byte
	if meta.MIMEType != "image/jpeg" && meta.MIMEType != "image/webp" {
		return hash, false
	}
	if meta.SizeBytes <= 0 || meta.SizeBytes > MaxBytes || meta.Width <= 0 || meta.Height <= 0 ||
		meta.Width > MaxLongEdge || meta.Height > MaxLongEdge || len(meta.SHA256) != sha256.Size*2 {
		return hash, false
	}
	_, err := hex.Decode(hash[:], []byte(meta.SHA256))
	return hash, err == nil
}

func cleanJPEG(data []byte) bool {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return false
	}
	pos := 2
	inScan := false
	seenScan := false
	for pos < len(data) {
		if inScan {
			// In entropy-coded data FF00 is a literal FF and RSTn is a
			// restart marker. Any other marker ends this scan (including
			// progressive JPEG scans, which may be followed by more tables).
			for pos < len(data) && data[pos] != 0xff {
				pos++
			}
		} else if data[pos] != 0xff {
			return false
		}
		if pos >= len(data) {
			return false
		}
		for pos < len(data) && data[pos] == 0xff {
			pos++
		}
		if pos >= len(data) {
			return false
		}
		marker := data[pos]
		pos++
		if inScan && (marker == 0 || marker >= 0xd0 && marker <= 0xd7) {
			continue
		}
		inScan = false
		if marker == 0xd9 {
			return seenScan && pos == len(data)
		}
		if marker == 0 || marker == 0xd8 || marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 || pos+2 > len(data) {
			return false
		}
		size := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		if size < 2 || size > len(data)-pos {
			return false
		}
		payload := data[pos+2 : pos+size]
		if marker >= 0xe0 && marker <= 0xef {
			// Browser re-encoders may emit structural JFIF/Adobe headers.
			// Permit only their exact fixed layouts, never thumbnails,
			// JFXX extensions, EXIF/XMP/ICC, or arbitrary APPn payloads.
			switch marker {
			case 0xe0:
				if len(payload) != 14 || string(payload[:5]) != "JFIF\x00" || payload[5] != 1 || payload[6] > 2 ||
					payload[7] > 2 || payload[12] != 0 || payload[13] != 0 {
					return false
				}
			case 0xee:
				if len(payload) != 12 || string(payload[:5]) != "Adobe" || binary.BigEndian.Uint16(payload[5:7]) != 100 ||
					payload[7] != 0 || payload[8] != 0 || payload[9] != 0 || payload[10] != 0 || payload[11] > 2 {
					return false
				}
			default:
				return false
			}
		} else if marker == 0xfe {
			return false
		}
		pos += size
		if marker == 0xda {
			seenScan = true
			inScan = true
		}
	}
	return false
}

func cleanWebP(data []byte, width, height int) bool {
	if len(data) < 20 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" ||
		uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return false
	}
	seenExtended, seenAlpha, seenImage, alphaFlag := false, false, false, false
	for pos := 12; pos < len(data); {
		if len(data)-pos < 8 {
			return false
		}
		kind := string(data[pos : pos+4])
		size := uint64(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		start := pos + 8
		end := uint64(start) + size
		paddedEnd := end + (size & 1)
		if paddedEnd > uint64(len(data)) || size == 0 {
			return false
		}
		if size&1 != 0 && data[int(end)] != 0 {
			return false
		}
		payload := data[start:int(end)]
		switch kind {
		case "VP8X":
			if pos != 12 || seenExtended || len(payload) != 10 || payload[0]&^byte(0x10) != 0 ||
				payload[1] != 0 || payload[2] != 0 || payload[3] != 0 {
				return false
			}
			canvasW := 1 + int(payload[4]) + (int(payload[5]) << 8) + (int(payload[6]) << 16)
			canvasH := 1 + int(payload[7]) + (int(payload[8]) << 8) + (int(payload[9]) << 16)
			if canvasW != width || canvasH != height {
				return false
			}
			seenExtended = true
			alphaFlag = payload[0]&0x10 != 0
		case "ALPH":
			if !seenExtended || !alphaFlag || seenAlpha || seenImage || payload[0]&0xc0 != 0 || payload[0]&3 > 1 {
				return false
			}
			seenAlpha = true
		case "VP8 ":
			if seenImage || alphaFlag != seenAlpha {
				return false
			}
			decoder := vp8.NewDecoder()
			decoder.Init(bytes.NewReader(payload), len(payload))
			frame, err := decoder.DecodeFrameHeader()
			if err != nil || frame.Width != width || frame.Height != height {
				return false
			}
			seenImage = true
		case "VP8L":
			if seenImage || seenAlpha {
				return false
			}
			config, err := vp8l.DecodeConfig(bytes.NewReader(payload))
			if err != nil || config.Width != width || config.Height != height {
				return false
			}
			seenImage = true
		default:
			// This allowlist rejects EXIF, XMP, ICCP, animation, unknown
			// chunks, and data hidden after the actual image bitstream.
			return false
		}
		pos = int(paddedEnd)
	}
	return seenImage
}
