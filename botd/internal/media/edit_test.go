package media

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os/exec"
	"testing"
)

func makeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 3), G: uint8(y * 5), B: 100, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEditImageCropAndScale(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	crop := [4]float64{0.25, 0.25, 0.75, 0.75}
	mime, out, err := EditImage(context.Background(), makeTestPNG(t, 80, 40),
		EditOptions{Crop: &crop, Scale: 2})
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg", mime)
	}
	w, h, ok := ImageSize(out)
	if !ok || w != 80 || h != 40 {
		t.Fatalf("size = %dx%d (ok=%v), want 80x40", w, h, ok)
	}
}

func TestEditImageSharpenOnlyPNG(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	mime, out, err := EditImage(context.Background(), makeTestPNG(t, 64, 32),
		EditOptions{Sharpen: true, Format: "png"})
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" {
		t.Fatalf("mime = %q, want image/png", mime)
	}
	w, h, ok := ImageSize(out)
	if !ok || w != 64 || h != 32 {
		t.Fatalf("size = %dx%d (ok=%v), want 64x32", w, h, ok)
	}
}

func TestEditImageRejectsBadCrop(t *testing.T) {
	src := makeTestPNG(t, 20, 20)
	cases := [][4]float64{
		{0.5, 0, 0.2, 1},  // x1 <= x0
		{-0.1, 0, 0.5, 1}, // out of range
		{0, 0, 1.2, 1},    // out of range
		{0, 0.9, 1, 0.95}, // too short, < 4px after scaling
	}
	for _, c := range cases {
		c := c
		if _, _, err := EditImage(context.Background(), src, EditOptions{Crop: &c}); err == nil {
			t.Errorf("crop %v: expected error", c)
		}
	}
}

func TestEditImageRequiresOperation(t *testing.T) {
	if _, _, err := EditImage(context.Background(), makeTestPNG(t, 10, 10), EditOptions{}); err == nil {
		t.Fatal("expected error for no-op edit")
	}
}
