package media

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os/exec"
	"strings"
)

// EditOptions describes a local image edit. Crop is a fraction box of the
// source (0..1); Scale above 1 upscales; Sharpen applies a light unsharp mask.
type EditOptions struct {
	Crop    *[4]float64 // x0, y0, x1, y1
	Scale   float64
	Sharpen bool
	Format  string // "jpeg" (default) or "png"
}

const maxEditEdge = 4096

// EditImage applies the edit with ffmpeg and returns the encoded result.
func EditImage(ctx context.Context, src []byte, opts EditOptions) (string, []byte, error) {
	if len(src) == 0 {
		return "", nil, fmt.Errorf("edit: empty image")
	}
	scale := opts.Scale
	if scale <= 0 {
		scale = 1
	}
	if scale > 4 {
		scale = 4
	}
	if opts.Crop == nil && scale <= 1 && !opts.Sharpen {
		return "", nil, fmt.Errorf("edit: 请至少指定 crop、scale 或 sharpen")
	}

	var filters []string
	if c := opts.Crop; c != nil {
		for _, v := range c {
			if v < 0 || v > 1 {
				return "", nil, fmt.Errorf("edit: crop 的四个值都应在 0~1 之间")
			}
		}
		if c[2] <= c[0] || c[3] <= c[1] {
			return "", nil, fmt.Errorf("edit: crop 应为 [x0,y0,x1,y1] 且 x1>x0、y1>y0")
		}
		filters = append(filters, fmt.Sprintf(
			"crop=w=iw*%f:h=ih*%f:x=iw*%f:y=ih*%f", c[2]-c[0], c[3]-c[1], c[0], c[1]))
	}
	if scale > 1 {
		filters = append(filters, fmt.Sprintf(
			"scale=w=trunc(iw*%f/2)*2:h=trunc(ih*%f/2)*2:flags=lanczos", scale, scale))
	}
	if opts.Sharpen {
		filters = append(filters, "unsharp=5:5:1.2:5:5:0")
	}
	// mjpeg uses yuvj420p, which rejects odd dimensions.
	filters = append(filters, "scale=w=trunc(iw/2)*2:h=trunc(ih/2)*2")

	if w, h, ok := ImageSize(src); ok {
		cw, ch := float64(w), float64(h)
		if c := opts.Crop; c != nil {
			cw *= c[2] - c[0]
			ch *= c[3] - c[1]
		}
		ow, oh := int(cw*scale), int(ch*scale)
		if opts.Crop != nil && (ow < 4 || oh < 4) {
			return "", nil, fmt.Errorf("edit: 裁剪后太小了（%dx%d）", ow, oh)
		}
		if scale > 1 && (ow > maxEditEdge || oh > maxEditEdge) {
			return "", nil, fmt.Errorf("edit: 输出太大（%dx%d），最大 %dpx，请调小 scale", ow, oh, maxEditEdge)
		}
	}

	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", nil, fmt.Errorf("edit: ffmpeg not found")
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-i", "pipe:0",
		"-vf", strings.Join(filters, ","), "-frames:v", "1",
	}
	mime := "image/jpeg"
	switch strings.ToLower(opts.Format) {
	case "png":
		mime = "image/png"
		args = append(args, "-f", "image2pipe", "-c:v", "png")
	default:
		args = append(args, "-f", "mjpeg", "-q:v", "2")
	}
	args = append(args, "-y", "pipe:1")

	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.Stdin = bytes.NewReader(src)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("edit: ffmpeg: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	if out.Len() == 0 {
		return "", nil, fmt.Errorf("edit: empty output")
	}
	return mime, out.Bytes(), nil
}

// ImageSize reads the pixel dimensions of a JPEG/PNG/GIF image.
func ImageSize(data []byte) (int, int, bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}
