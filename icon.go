package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

// makeBatteryIcon generates a 16×16 battery icon as Windows ICO bytes.
// fill color varies by level and charging state.
func makeBatteryIcon(level int, charging bool) []byte {
	const W, H = 16, 16
	img := image.NewNRGBA(image.Rect(0, 0, W, H))

	// Fill colour by battery level / state
	var fill color.NRGBA
	switch {
	case charging:
		fill = color.NRGBA{0x42, 0xA5, 0xF5, 0xFF} // blue
	case level >= 50:
		fill = color.NRGBA{0x66, 0xBB, 0x6A, 0xFF} // green
	case level >= 20:
		fill = color.NRGBA{0xFF, 0xA7, 0x26, 0xFF} // orange
	default:
		fill = color.NRGBA{0xEF, 0x53, 0x50, 0xFF} // red
	}

	outline := color.NRGBA{0xCC, 0xCC, 0xCC, 0xFF}

	// ── Battery body: x=[1..11], y=[4..11] ──────────────────────────────────
	for x := 1; x <= 11; x++ {
		img.SetNRGBA(x, 4, outline)
		img.SetNRGBA(x, 11, outline)
	}
	for y := 4; y <= 11; y++ {
		img.SetNRGBA(1, y, outline)
		img.SetNRGBA(11, y, outline)
	}

	// Battery cap: x=[12..13], y=[6..9]
	for x := 12; x <= 13; x++ {
		for y := 6; y <= 9; y++ {
			img.SetNRGBA(x, y, outline)
		}
	}

	// ── Interior fill ────────────────────────────────────────────────────────
	// Interior: x=[2..10] (9 cols), y=[5..10] (6 rows)
	const maxFillCols = 9
	fillCols := level * maxFillCols / 100
	if fillCols > maxFillCols {
		fillCols = maxFillCols
	}
	for x := 2; x < 2+fillCols; x++ {
		for y := 5; y <= 10; y++ {
			img.SetNRGBA(x, y, fill)
		}
	}

	// ── Lightning bolt overlay when charging ─────────────────────────────────
	if charging {
		bolt := color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}
		for _, p := range [][2]int{
			{7, 5},
			{7, 6}, {6, 6},
			{6, 7}, {7, 7},
			{7, 8}, {8, 8},
			{8, 9},
		} {
			img.SetNRGBA(p[0], p[1], bolt)
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return wrapICO(buf.Bytes())
}

// wrapICO wraps a PNG image into a minimal ICO container.
// Windows Vista+ supports PNG-in-ICO (RFC-compliant embedded PNG).
func wrapICO(pngData []byte) []byte {
	buf := new(bytes.Buffer)

	// ICONDIR (6 bytes)
	_ = binary.Write(buf, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(buf, binary.LittleEndian, uint16(1)) // type = ICO
	_ = binary.Write(buf, binary.LittleEndian, uint16(1)) // count

	// ICONDIRENTRY (16 bytes)
	buf.WriteByte(16) // width
	buf.WriteByte(16) // height
	buf.WriteByte(0)  // colorCount
	buf.WriteByte(0)  // reserved
	_ = binary.Write(buf, binary.LittleEndian, uint16(1))                // planes
	_ = binary.Write(buf, binary.LittleEndian, uint16(32))               // bitCount
	_ = binary.Write(buf, binary.LittleEndian, uint32(len(pngData)))     // image size
	_ = binary.Write(buf, binary.LittleEndian, uint32(22))               // offset = 6+16

	buf.Write(pngData)
	return buf.Bytes()
}
