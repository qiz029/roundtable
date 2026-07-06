package roundtable

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"strings"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

const (
	maxAvatarUploadBytes        = 2 * 1024 * 1024
	maxAvatarInputDimension     = 4096
	maxAvatarInputPixels        = 12 * 1024 * 1024
	maxAvatarOutputDimension    = 512
	normalizedAvatarContentType = "image/jpeg"
)

func normalizeAvatarImage(body []byte) ([]byte, string, error) {
	if len(body) == 0 {
		return nil, "", errInvalidInput("avatar file is required")
	}
	if len(body) > maxAvatarUploadBytes {
		return nil, "", errInvalidInput("avatar file cannot exceed 2MB")
	}

	format, err := avatarInputFormat(body)
	if err != nil {
		return nil, "", err
	}
	cfg, err := decodeAvatarConfig(format, body)
	if err != nil {
		return nil, "", errInvalidInput("avatar file is not a valid image")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width > maxAvatarInputDimension || cfg.Height > maxAvatarInputDimension ||
		cfg.Width*cfg.Height > maxAvatarInputPixels {
		return nil, "", errInvalidInput("avatar image dimensions are too large")
	}

	img, err := decodeAvatar(format, body)
	if err != nil {
		return nil, "", errInvalidInput("avatar file is not a valid image")
	}
	normalized := normalizeAvatarCanvas(img)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, normalized, &jpeg.Options{Quality: 85}); err != nil {
		return nil, "", err
	}
	if out.Len() > maxAvatarUploadBytes {
		return nil, "", errInvalidInput("normalized avatar file cannot exceed 2MB")
	}
	return out.Bytes(), normalizedAvatarContentType, nil
}

func avatarInputFormat(body []byte) (string, error) {
	if len(body) >= 3 && body[0] == 0xff && body[1] == 0xd8 && body[2] == 0xff {
		return "jpeg", nil
	}
	if len(body) >= 8 && bytes.Equal(body[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return "png", nil
	}
	if len(body) >= 12 && string(body[:4]) == "RIFF" && string(body[8:12]) == "WEBP" {
		return "webp", nil
	}
	if len(body) >= 6 && (string(body[:6]) == "GIF87a" || string(body[:6]) == "GIF89a") {
		return "", errInvalidInput("avatar file type must be JPEG, PNG, or WebP")
	}
	if looksLikeMarkup(body) {
		return "", errInvalidInput("avatar file type must be JPEG, PNG, or WebP")
	}
	return "", errInvalidInput("avatar file type must be JPEG, PNG, or WebP")
}

func decodeAvatarConfig(format string, body []byte) (image.Config, error) {
	reader := bytes.NewReader(body)
	switch format {
	case "jpeg":
		return jpeg.DecodeConfig(reader)
	case "png":
		return png.DecodeConfig(reader)
	case "webp":
		return webp.DecodeConfig(reader)
	default:
		return image.Config{}, image.ErrFormat
	}
}

func decodeAvatar(format string, body []byte) (image.Image, error) {
	reader := bytes.NewReader(body)
	switch format {
	case "jpeg":
		return jpeg.Decode(reader)
	case "png":
		return png.Decode(reader)
	case "webp":
		return webp.Decode(reader)
	default:
		return nil, image.ErrFormat
	}
}

func normalizeAvatarCanvas(src image.Image) image.Image {
	bounds := src.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width <= 0 || height <= 0 {
		return src
	}
	dstWidth, dstHeight := width, height
	if width > maxAvatarOutputDimension || height > maxAvatarOutputDimension {
		if width >= height {
			dstWidth = maxAvatarOutputDimension
			dstHeight = maxAvatarOutputDimension * height / width
		} else {
			dstHeight = maxAvatarOutputDimension
			dstWidth = maxAvatarOutputDimension * width / height
		}
		if dstWidth < 1 {
			dstWidth = 1
		}
		if dstHeight < 1 {
			dstHeight = 1
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstWidth, dstHeight))
	fill := image.NewUniform(color.White)
	xdraw.Draw(dst, dst.Bounds(), fill, image.Point{}, xdraw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, xdraw.Over, nil)
	return dst
}

func looksLikeMarkup(body []byte) bool {
	prefix, _ := io.ReadAll(io.LimitReader(bytes.NewReader(body), 256))
	trimmed := strings.ToLower(strings.TrimSpace(string(prefix)))
	return strings.HasPrefix(trimmed, "<svg") ||
		strings.HasPrefix(trimmed, "<html") ||
		strings.HasPrefix(trimmed, "<!doctype html") ||
		strings.HasPrefix(trimmed, "<?xml")
}
