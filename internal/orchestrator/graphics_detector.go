package orchestrator

import (
	"image"
	"image/color"
	"math"

	"github.com/gen2brain/go-fitz"
	"github.com/rs/zerolog/log"
)

const (
	// DPI for rendering pages for analysis
	AnalysisDPI = 150.0

	// Minimum size in cm to consider as graphics
	MinGraphicsSizeCM = 2.0

	// Binary threshold for separating text/graphics from background
	BinaryThreshold = 200 // 0-255, higher = more aggressive (keeps only dark pixels)

	// Minimum component size in pixels to consider (filters noise)
	MinComponentPixels = 100
)

// GraphicsDetector detects graphic elements on PDF pages
type GraphicsDetector struct{}

// NewGraphicsDetector creates a new graphics detector
func NewGraphicsDetector() *GraphicsDetector {
	return &GraphicsDetector{}
}

// HasLargeGraphics checks if a page contains graphics larger than minSizeCM x minSizeCM
func (gd *GraphicsDetector) HasLargeGraphics(pdfPath string, pageNum int, minSizeCM float64) (bool, error) {
	// Open PDF
	doc, err := fitz.New(pdfPath)
	if err != nil {
		return false, err
	}
	defer doc.Close()

	// Render page to grayscale image at analysis DPI
	// go-fitz uses 0-based indexing
	img, err := doc.ImageDPI(pageNum-1, AnalysisDPI)
	if err != nil {
		return false, err
	}

	// Convert to grayscale if needed
	grayImg := toGrayscale(img)

	// Apply binary threshold to separate content from background
	binaryImg := applyThreshold(grayImg, BinaryThreshold)

	// Find connected components
	components := findConnectedComponents(binaryImg, MinComponentPixels)

	// Calculate pixel size in cm based on DPI
	cmPerPixel := 2.54 / AnalysisDPI // 1 inch = 2.54 cm

	// Check if any component is larger than minSizeCM x minSizeCM
	for _, comp := range components {
		widthCM := float64(comp.Width) * cmPerPixel
		heightCM := float64(comp.Height) * cmPerPixel

		// Check if BOTH dimensions are >= minSizeCM
		if widthCM >= minSizeCM && heightCM >= minSizeCM {
			log.Debug().
				Int("page", pageNum).
				Float64("width_cm", widthCM).
				Float64("height_cm", heightCM).
				Int("pixels", comp.PixelCount).
				Msg("large graphics detected")
			return true, nil
		}
	}

	return false, nil
}

// Component represents a connected component
type Component struct {
	MinX       int
	MinY       int
	MaxX       int
	MaxY       int
	Width      int
	Height     int
	PixelCount int
}

// toGrayscale converts an image to grayscale
func toGrayscale(img image.Image) *image.Gray {
	bounds := img.Bounds()
	gray := image.NewGray(bounds)

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			grayColor := color.GrayModel.Convert(img.At(x, y))
			gray.Set(x, y, grayColor)
		}
	}

	return gray
}

// applyThreshold converts grayscale to binary (0 or 255)
func applyThreshold(img *image.Gray, threshold uint8) *image.Gray {
	bounds := img.Bounds()
	binary := image.NewGray(bounds)

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			gray := img.GrayAt(x, y).Y
			if gray < threshold {
				// Dark pixel (likely content)
				binary.SetGray(x, y, color.Gray{Y: 0})
			} else {
				// Light pixel (background)
				binary.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}

	return binary
}

// findConnectedComponents finds connected components using flood-fill
func findConnectedComponents(img *image.Gray, minPixels int) []Component {
	bounds := img.Bounds()
	visited := make([][]bool, bounds.Dy())
	for i := range visited {
		visited[i] = make([]bool, bounds.Dx())
	}

	var components []Component

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			// Skip if already visited or is background
			if visited[y-bounds.Min.Y][x-bounds.Min.X] || img.GrayAt(x, y).Y == 255 {
				continue
			}

			// Found new component - flood fill to find its extent
			comp := floodFill(img, visited, x, y, bounds)

			// Only keep components larger than minimum size (filters noise)
			if comp.PixelCount >= minPixels {
				components = append(components, comp)
			}
		}
	}

	return components
}

// floodFill performs flood fill and returns component info
func floodFill(img *image.Gray, visited [][]bool, startX, startY int, bounds image.Rectangle) Component {
	comp := Component{
		MinX: startX,
		MinY: startY,
		MaxX: startX,
		MaxY: startY,
	}

	// Use stack-based flood fill (iterative, not recursive to avoid stack overflow)
	stack := []image.Point{{X: startX, Y: startY}}

	for len(stack) > 0 {
		// Pop from stack
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		x, y := p.X, p.Y

		// Skip if out of bounds
		if x < bounds.Min.X || x >= bounds.Max.X || y < bounds.Min.Y || y >= bounds.Max.Y {
			continue
		}

		// Skip if already visited or is background
		if visited[y-bounds.Min.Y][x-bounds.Min.X] || img.GrayAt(x, y).Y == 255 {
			continue
		}

		// Mark as visited
		visited[y-bounds.Min.Y][x-bounds.Min.X] = true
		comp.PixelCount++

		// Update bounding box
		if x < comp.MinX {
			comp.MinX = x
		}
		if x > comp.MaxX {
			comp.MaxX = x
		}
		if y < comp.MinY {
			comp.MinY = y
		}
		if y > comp.MaxY {
			comp.MaxY = y
		}

		// Add 4-connected neighbors to stack
		stack = append(stack,
			image.Point{X: x + 1, Y: y},
			image.Point{X: x - 1, Y: y},
			image.Point{X: x, Y: y + 1},
			image.Point{X: x, Y: y - 1},
		)
	}

	// Calculate dimensions
	comp.Width = comp.MaxX - comp.MinX + 1
	comp.Height = comp.MaxY - comp.MinY + 1

	return comp
}

// AnalyzePageGraphics provides detailed graphics analysis for debugging
func (gd *GraphicsDetector) AnalyzePageGraphics(pdfPath string, pageNum int) (map[string]interface{}, error) {
	doc, err := fitz.New(pdfPath)
	if err != nil {
		return nil, err
	}
	defer doc.Close()

	img, err := doc.ImageDPI(pageNum-1, AnalysisDPI)
	if err != nil {
		return nil, err
	}

	grayImg := toGrayscale(img)
	binaryImg := applyThreshold(grayImg, BinaryThreshold)
	components := findConnectedComponents(binaryImg, MinComponentPixels)

	cmPerPixel := 2.54 / AnalysisDPI

	// Prepare analysis results
	result := map[string]interface{}{
		"page":              pageNum,
		"dpi":               AnalysisDPI,
		"cm_per_pixel":      cmPerPixel,
		"total_components":  len(components),
		"threshold":         BinaryThreshold,
		"min_component_px":  MinComponentPixels,
		"image_width_px":    img.Bounds().Dx(),
		"image_height_px":   img.Bounds().Dy(),
		"image_width_cm":    float64(img.Bounds().Dx()) * cmPerPixel,
		"image_height_cm":   float64(img.Bounds().Dy()) * cmPerPixel,
	}

	// Find largest component
	var largestComp *Component
	var largestArea int
	for i := range components {
		area := components[i].Width * components[i].Height
		if area > largestArea {
			largestArea = area
			largestComp = &components[i]
		}
	}

	if largestComp != nil {
		result["largest_component"] = map[string]interface{}{
			"width_px":    largestComp.Width,
			"height_px":   largestComp.Height,
			"width_cm":    float64(largestComp.Width) * cmPerPixel,
			"height_cm":   float64(largestComp.Height) * cmPerPixel,
			"pixel_count": largestComp.PixelCount,
		}
	}

	// Count how many components are >= 2x2 cm
	largeGraphicsCount := 0
	for _, comp := range components {
		widthCM := float64(comp.Width) * cmPerPixel
		heightCM := float64(comp.Height) * cmPerPixel
		if widthCM >= MinGraphicsSizeCM && heightCM >= MinGraphicsSizeCM {
			largeGraphicsCount++
		}
	}
	result["large_graphics_count"] = largeGraphicsCount

	return result, nil
}

// Helper to calculate distance between two points
func distance(x1, y1, x2, y2 int) float64 {
	dx := float64(x2 - x1)
	dy := float64(y2 - y1)
	return math.Sqrt(dx*dx + dy*dy)
}
