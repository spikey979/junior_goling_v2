package imagerender

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"

	"github.com/gen2brain/go-fitz"
	"github.com/rs/zerolog/log"
)

// ColorMode defines the color mode for rendering
type ColorMode string

const (
	ColorRGB  ColorMode = "rgb"
	ColorGray ColorMode = "gray"
)

// RenderPageToJPEG renders a PDF page as JPEG image (in-memory)
// Returns JPEG bytes, width, height, error
func RenderPageToJPEG(pdfPath string, pageNum, dpi, quality int, colorMode string) ([]byte, int, int, error) {
	// Open PDF
	doc, err := fitz.New(pdfPath)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to open PDF: %w", err)
	}
	defer doc.Close()

	// Render page at specified DPI (go-fitz uses 0-based indexing)
	img, err := doc.ImageDPI(pageNum-1, float64(dpi))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to render page %d: %w", pageNum, err)
	}

	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	// Convert to desired color mode
	var finalImg image.Image
	if colorMode == "gray" {
		// Convert to grayscale
		grayImg := image.NewGray(bounds)
		draw.Draw(grayImg, bounds, img, image.Point{}, draw.Src)
		finalImg = grayImg
		log.Debug().
			Int("page", pageNum).
			Int("width", width).
			Int("height", height).
			Str("color", "grayscale").
			Msg("rendered page to grayscale")
	} else {
		// Use RGB (original from go-fitz is already RGBA)
		finalImg = img
		log.Debug().
			Int("page", pageNum).
			Int("width", width).
			Int("height", height).
			Str("color", "rgb").
			Msg("rendered page to RGB")
	}

	// Encode as JPEG
	var buf bytes.Buffer
	opts := &jpeg.Options{Quality: quality}
	if err := jpeg.Encode(&buf, finalImg, opts); err != nil {
		return nil, 0, 0, fmt.Errorf("failed to encode JPEG: %w", err)
	}

	jpegBytes := buf.Bytes()

	log.Debug().
		Int("page", pageNum).
		Int("jpeg_size", len(jpegBytes)).
		Int("quality", quality).
		Int("dpi", dpi).
		Msg("encoded page as JPEG")

	return jpegBytes, width, height, nil
}

// EncodeToBase64 converts binary data to base64 string
func EncodeToBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// DecodeFromBase64 converts base64 string back to binary data
func DecodeFromBase64(b64 string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(b64)
}

// GetImageDimensions extracts dimensions from JPEG bytes
func GetImageDimensions(jpegBytes []byte) (width, height int, err error) {
	img, err := jpeg.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		return 0, 0, fmt.Errorf("failed to decode JPEG: %w", err)
	}

	bounds := img.Bounds()
	return bounds.Dx(), bounds.Dy(), nil
}
