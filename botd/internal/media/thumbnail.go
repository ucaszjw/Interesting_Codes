package media

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// ThumbnailJPEG shrinks an image to at most maxPx on its long edge, as JPEG.
// Used before sending a candidate to the vision checker so large originals do
// not blow up the verification request.
func ThumbnailJPEG(ctx context.Context, data []byte, maxPx int) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("thumbnail: empty image")
	}
	if maxPx <= 0 {
		maxPx = 512
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("thumbnail: ffmpeg not found")
	}
	filter := fmt.Sprintf("scale='min(%d,iw)':-2", maxPx)
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-i", "pipe:0", "-vf", filter, "-f", "mjpeg", "-q:v", "5", "-y", "pipe:1")
	cmd.Stdin = bytes.NewReader(data)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("thumbnail: ffmpeg: %w", err)
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("thumbnail: empty output")
	}
	return out.Bytes(), nil
}
