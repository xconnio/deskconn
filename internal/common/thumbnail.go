package common

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"time"
)

// MaxThumbnailSourceSize is the largest image a thumbnail is generated for.
const MaxThumbnailSourceSize = 20 * 1024 * 1024 // 20 MB
// ThumbnailMaxDim is the longest side of a generated thumbnail, in pixels.
const ThumbnailMaxDim = 480

// MaxThumbnailSourcePixels is the largest image (width x height) a thumbnail is made of:
// a small file can claim huge dimensions, and decoding it would take ~4 bytes a pixel.
const MaxThumbnailSourcePixels = 50_000_000

// ThumbnailTimeout bounds one ffmpeg/pdftocairo run, so a broken file (or a hung network
// mount) can't hold a thumbnail worker forever.
const ThumbnailTimeout = 30 * time.Second

// ImageThumbnail returns a base64 JPEG thumbnail of the image at path, or "" if it can't
// be decoded.
func ImageThumbnail(path string) string {
	//nolint:gosec // path comes from an already-resolved FileEntry/walk under a jailed root
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	cfg, _, err := image.DecodeConfig(f)
	if err != nil || cfg.Width*cfg.Height > MaxThumbnailSourcePixels {
		return ""
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	src, _, err := image.Decode(f)
	if err != nil {
		return ""
	}

	thumb := scaledImage(src, ThumbnailMaxDim)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 85}); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// VideoThumbnail returns a base64 JPEG of the video at path one second in, via ffmpeg, or
// "" if ffmpeg or the file is unavailable.
func VideoThumbnail(path string) string {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), ThumbnailTimeout)
	defer cancel()
	//nolint:gosec // path comes from an already-resolved FileEntry/walk under a jailed root
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-ss", "00:00:01",
		"-i", path,
		"-vframes", "1",
		"-vf", fmt.Sprintf("scale=%d:-1", ThumbnailMaxDim),
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-",
	)
	cmd.WaitDelay = time.Second // don't wait on a killed tool's children holding stdout
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(out)
}

// PDFThumbnail renders the first page of a PDF to a JPEG via
// pdftocairo (poppler-utils). Returns "" if the tool or file is unavailable.
func PDFThumbnail(path string) string {
	pdftocairo, err := exec.LookPath("pdftocairo")
	if err != nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), ThumbnailTimeout)
	defer cancel()
	//nolint:gosec // path comes from an already-resolved FileEntry/walk under a jailed root
	cmd := exec.CommandContext(ctx, pdftocairo,
		"-jpeg",
		"-singlefile",
		"-scale-to", fmt.Sprint(ThumbnailMaxDim),
		"-f", "1",
		"-l", "1",
		path,
		"-",
	)
	cmd.WaitDelay = time.Second // don't wait on a killed tool's children holding stdout
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(out)
}

func scaledImage(src image.Image, maxDim int) image.Image {
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= maxDim && srcH <= maxDim {
		return src
	}

	dstW, dstH := maxDim, maxDim
	if srcW > srcH {
		dstH = srcH * maxDim / srcW
	} else {
		dstW = srcW * maxDim / srcH
	}
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}

	const samples = 4
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		for x := 0; x < dstW; x++ {
			var r, g, bl, a uint32
			for sy := 0; sy < samples; sy++ {
				py := b.Min.Y + (y*samples+sy)*srcH/(dstH*samples)
				for sx := 0; sx < samples; sx++ {
					px := b.Min.X + (x*samples+sx)*srcW/(dstW*samples)
					cr, cg, cb, ca := src.At(px, py).RGBA()
					r, g, bl, a = r+cr, g+cg, bl+cb, a+ca
				}
			}
			n := uint32(samples * samples)
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), uint8(a / n >> 8)})
		}
	}
	return dst
}
