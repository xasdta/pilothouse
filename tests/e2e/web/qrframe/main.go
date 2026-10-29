// Command qrframe writes a Motion-JPEG file showing a QR code of a text, for
// Chrome's fake camera (--use-file-for-fake-video-capture) in the web e2e
// checks:
//
//	go run ./tests/e2e/web/qrframe <text> <out.mjpeg>
package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"log"
	"os"

	"rsc.io/qr"
)

func main() {
	if len(os.Args) != 3 {
		log.Fatal("usage: qrframe <text> <out.mjpeg>")
	}
	code, err := qr.Encode(os.Args[1], qr.M)
	if err != nil {
		log.Fatal(err)
	}
	const w, h = 640, 480
	frame := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(frame, frame.Bounds(), image.NewUniform(color.RGBA{90, 90, 90, 255}), image.Point{}, draw.Src)
	// A white card with the code, as on a screen.
	scale := 360 / (code.Size + 8)
	side := (code.Size + 8) * scale
	x0, y0 := (w-side)/2, (h-side)/2
	draw.Draw(frame, image.Rect(x0, y0, x0+side, y0+side), image.White, image.Point{}, draw.Src)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				r := image.Rect(x0+(x+4)*scale, y0+(y+4)*scale, x0+(x+5)*scale, y0+(y+5)*scale)
				draw.Draw(frame, r, image.Black, image.Point{}, draw.Src)
			}
		}
	}
	var one bytes.Buffer
	if err := jpeg.Encode(&one, frame, &jpeg.Options{Quality: 90}); err != nil {
		log.Fatal(err)
	}
	var out bytes.Buffer
	for i := 0; i < 30; i++ {
		out.Write(one.Bytes())
	}
	if err := os.WriteFile(os.Args[2], out.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
}
