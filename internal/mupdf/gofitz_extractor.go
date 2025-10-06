package mupdf

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gen2brain/go-fitz"
	"github.com/rs/zerolog/log"
)

// GoFitzExtractor uses go-fitz library for PDF text extraction (no external tools needed)
type GoFitzExtractor struct{}

// NewGoFitzExtractor creates a new go-fitz based extractor
func NewGoFitzExtractor() *GoFitzExtractor {
	return &GoFitzExtractor{}
}

// IsAvailable always returns true since go-fitz is embedded
func (g *GoFitzExtractor) IsAvailable() bool {
	return true
}

// GetPageCount returns the number of pages in a PDF using go-fitz
func (g *GoFitzExtractor) GetPageCount(pdfPath string) (int, error) {
	doc, err := fitz.New(pdfPath)
	if err != nil {
		return 0, fmt.Errorf("failed to open PDF: %w", err)
	}
	defer doc.Close()

	return doc.NumPage(), nil
}

// ExtractText extracts all text from a PDF file using go-fitz
func (g *GoFitzExtractor) ExtractText(pdfPath string) (string, error) {
	log.Debug().Str("pdf", pdfPath).Msg("Extracting all text with go-fitz")

	doc, err := fitz.New(pdfPath)
	if err != nil {
		return "", fmt.Errorf("failed to open PDF: %w", err)
	}
	defer doc.Close()

	var result strings.Builder
	for i := 0; i < doc.NumPage(); i++ {
		text, err := doc.Text(i)
		if err != nil {
			log.Warn().Err(err).Int("page", i+1).Msg("Failed to extract text from page")
			continue
		}
		if i > 0 {
			result.WriteString("\n\n")
		}
		result.WriteString(text)
	}

	text := result.String()
	log.Debug().Int("chars", len(text)).Msg("Extracted text from PDF")

	return text, nil
}

// TextBlock represents a text block with position information
type TextBlock struct {
	Text   string
	X      float64 // Left edge
	Y      float64 // Top edge
	Width  float64
	Height float64
}

// ExtractTextByPage extracts text from a specific page using go-fitz
// Automatically detects and handles multi-column layouts
func (g *GoFitzExtractor) ExtractTextByPage(pdfPath string, pageNum int) (string, error) {
	log.Debug().Str("pdf", pdfPath).Int("page", pageNum).Msg("Extracting page text with go-fitz")

	doc, err := fitz.New(pdfPath)
	if err != nil {
		return "", fmt.Errorf("failed to open PDF: %w", err)
	}
	defer doc.Close()

	// go-fitz uses 0-based indexing
	pageIndex := pageNum - 1

	if pageIndex < 0 || pageIndex >= doc.NumPage() {
		return "", fmt.Errorf("page %d out of range (document has %d pages)", pageNum, doc.NumPage())
	}

	// Try HTML extraction for better column handling
	htmlText, err := doc.HTML(pageIndex, false)

	log.Debug().
		Int("page", pageNum).
		Bool("html_error", err != nil).
		Int("html_length", len(htmlText)).
		Msg("HTML extraction attempt")

	if err == nil && htmlText != "" {
		// Extract blocks from HTML
		blocks, pageWidth := g.extractBlocksFromHTML(htmlText)

		if len(blocks) > 0 {
			// Detect if page has columns
			hasColumns := g.detectColumns(blocks, pageWidth)

			log.Debug().
				Int("page", pageNum).
				Int("blocks", len(blocks)).
				Bool("has_columns", hasColumns).
				Float64("page_width", pageWidth).
				Msg("HTML extraction analysis")

			if hasColumns {
				// Reorder blocks for column layout (left column first, then right)
				orderedBlocks := g.reorderColumnsLeftToRight(blocks, pageWidth)
				text := g.joinBlocks(orderedBlocks)
				cleanedText := g.cleanText(text, pageNum)

				log.Debug().
					Int("page", pageNum).
					Int("chars", len(cleanedText)).
					Msg("Extracted text with column reordering")

				return cleanedText, nil
			}
		}
	}

	// Fallback to default text extraction
	rawText, err := doc.Text(pageIndex)
	if err != nil {
		return "", fmt.Errorf("failed to extract text from page %d: %w", pageNum, err)
	}

	// Clean the extracted text
	cleanedText := g.cleanText(rawText, pageNum)

	log.Debug().
		Int("page", pageNum).
		Int("raw_chars", len(rawText)).
		Int("cleaned_chars", len(cleanedText)).
		Msg("Extracted and cleaned page text")

	return cleanedText, nil
}

// cleanText removes headers, footers, and other artifacts
func (g *GoFitzExtractor) cleanText(text string, pageNum int) string {
	lines := strings.Split(text, "\n")
	var cleanedLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip empty lines
		if trimmed == "" {
			continue
		}

		// Skip lines that are just page numbers
		if g.isPageNumber(trimmed, pageNum) {
			continue
		}

		// Skip lines that look like headers/footers
		if g.isHeaderFooter(trimmed) {
			continue
		}

		// Skip noise
		if g.isNoise(trimmed) {
			continue
		}

		cleanedLines = append(cleanedLines, line)
	}

	// Join lines and fix broken sentences
	result := strings.Join(cleanedLines, "\n")
	result = g.fixBrokenLines(result)

	return strings.TrimSpace(result)
}

// Helper functions (same as in Extractor)

func (g *GoFitzExtractor) isPageNumber(line string, pageNum int) bool {
	// Same implementation as Extractor
	if line == fmt.Sprintf("%d", pageNum) {
		return true
	}

	patterns := []string{
		fmt.Sprintf("Page %d", pageNum),
		fmt.Sprintf("- %d -", pageNum),
		fmt.Sprintf("[%d]", pageNum),
	}

	for _, pattern := range patterns {
		if strings.EqualFold(line, pattern) {
			return true
		}
	}

	return false
}

func (g *GoFitzExtractor) isHeaderFooter(line string) bool {
	if len(line) < 3 {
		return true
	}

	if len(line) < 50 && strings.ToUpper(line) == line {
		words := strings.Fields(line)
		if len(words) <= 2 {
			return true
		}
	}

	footerPatterns := []string{
		"CONFIDENTIAL",
		"COPYRIGHT",
		"ALL RIGHTS RESERVED",
		"PROPRIETARY",
	}

	upperLine := strings.ToUpper(line)
	for _, pattern := range footerPatterns {
		if strings.Contains(upperLine, pattern) && len(line) < 100 {
			return true
		}
	}

	return false
}

func (g *GoFitzExtractor) isNoise(line string) bool {
	// Just special characters
	specialOnly := true
	for _, r := range line {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			specialOnly = false
			break
		}
	}

	return specialOnly
}

func (g *GoFitzExtractor) fixBrokenLines(text string) string {
	lines := strings.Split(text, "\n")
	var fixed []string

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		if i < len(lines)-1 {
			trimmed := strings.TrimSpace(line)
			nextTrimmed := strings.TrimSpace(lines[i+1])

			if trimmed != "" && nextTrimmed != "" && len(trimmed) > 0 && len(nextTrimmed) > 0 {
				lastChar := trimmed[len(trimmed)-1]
				isSentenceEnd := lastChar == '.' || lastChar == '!' || lastChar == '?' || lastChar == ':' || lastChar == ';'

				if !isSentenceEnd {
					firstChar := nextTrimmed[0]
					startsWithLower := firstChar >= 'a' && firstChar <= 'z'

					if startsWithLower && !strings.HasSuffix(trimmed, "-") {
						fixed = append(fixed, trimmed+" "+nextTrimmed)
						i++ // Skip next line
						continue
					}
				}
			}
		}

		fixed = append(fixed, line)
	}

	return strings.Join(fixed, "\n")
}

// extractBlocksFromHTML parses HTML output to extract text blocks with positions
func (g *GoFitzExtractor) extractBlocksFromHTML(html string) ([]TextBlock, float64) {
	var blocks []TextBlock
	var pageWidth float64 = 612.0 // Default Letter size width in points

	// Extract page width from HTML if available
	// HTML format: <div id="page0" style="width:612pt;height:792pt;">
	widthRegex := regexp.MustCompile(`width:\s*(\d+(?:\.\d+)?)pt`)
	if matches := widthRegex.FindStringSubmatch(html); len(matches) > 1 {
		if w, err := strconv.ParseFloat(matches[1], 64); err == nil {
			pageWidth = w
		}
	}

	htmlPreview := html
	if len(html) > 200 {
		htmlPreview = html[:200] + "..."
	}

	log.Debug().
		Int("html_length", len(html)).
		Float64("page_width", pageWidth).
		Str("html_preview", htmlPreview).
		Msg("Parsing HTML for blocks")

	// Extract text blocks with positions
	// Parse <p> tags and extract left/top/text separately (order-independent)
	pTagRegex := regexp.MustCompile(`<p[^>]*style="([^"]*)"[^>]*>(.*?)</p>`)
	pMatches := pTagRegex.FindAllStringSubmatch(html, -1)

	leftRegex := regexp.MustCompile(`left:\s*(\d+(?:\.\d+)?)pt`)
	topRegex := regexp.MustCompile(`top:\s*(\d+(?:\.\d+)?)pt`)

	log.Debug().
		Int("p_tags_found", len(pMatches)).
		Msg("HTML block extraction")

	for _, match := range pMatches {
		if len(match) >= 3 {
			style := match[1]
			content := match[2]

			// Extract left and top from style
			var x, y float64
			if leftMatch := leftRegex.FindStringSubmatch(style); len(leftMatch) > 1 {
				x, _ = strconv.ParseFloat(leftMatch[1], 64)
			}
			if topMatch := topRegex.FindStringSubmatch(style); len(topMatch) > 1 {
				y, _ = strconv.ParseFloat(topMatch[1], 64)
			}

			text := stripHTMLTags(content)

			if text = strings.TrimSpace(text); text != "" {
				blocks = append(blocks, TextBlock{
					Text: text,
					X:    x,
					Y:    y,
				})
			}
		}
	}

	return blocks, pageWidth
}

// detectColumns determines if blocks represent a multi-column layout
// Uses clustering approach instead of simple midpoint split
func (g *GoFitzExtractor) detectColumns(blocks []TextBlock, pageWidth float64) bool {
	if len(blocks) < 8 {
		return false // Too few blocks to reliably determine
	}

	// Collect all unique X starting positions
	xPositions := make(map[float64]int)
	for _, block := range blocks {
		// Round to nearest 5pt to group similar positions
		roundedX := float64(int(block.X/5)) * 5
		xPositions[roundedX]++
	}

	// Find the two most common X positions (potential column starts)
	type xCount struct {
		x     float64
		count int
	}
	var xCounts []xCount
	for x, count := range xPositions {
		xCounts = append(xCounts, xCount{x, count})
	}

	// Sort by count descending
	sort.Slice(xCounts, func(i, j int) bool {
		return xCounts[i].count > xCounts[j].count
	})

	if len(xCounts) < 2 {
		return false
	}

	// Get top 2 X positions
	x1 := xCounts[0].x
	x2 := xCounts[1].x
	count1 := xCounts[0].count
	count2 := xCounts[1].count

	// Ensure x1 < x2
	if x1 > x2 {
		x1, x2 = x2, x1
		count1, count2 = count2, count1
	}

	// Check if they are significantly separated (at least 100pt apart)
	separation := x2 - x1
	if separation < 100 {
		log.Debug().
			Float64("x1", x1).
			Float64("x2", x2).
			Float64("separation", separation).
			Msg("Column candidates too close together")
		return false
	}

	// Check if both columns have significant content (at least 20% each)
	minBlocksPerColumn := len(blocks) / 10
	hasColumns := count1 >= minBlocksPerColumn && count2 >= minBlocksPerColumn

	log.Debug().
		Int("total_blocks", len(blocks)).
		Float64("left_col_x", x1).
		Int("left_col_blocks", count1).
		Float64("right_col_x", x2).
		Int("right_col_blocks", count2).
		Float64("separation", separation).
		Bool("has_columns", hasColumns).
		Msg("Column detection analysis")

	return hasColumns
}

// reorderColumnsLeftToRight reorders blocks to read left column first, then right
// Uses detected column positions instead of simple midpoint
func (g *GoFitzExtractor) reorderColumnsLeftToRight(blocks []TextBlock, pageWidth float64) []TextBlock {
	if len(blocks) < 2 {
		return blocks
	}

	// Find the two most common X positions (actual column starts)
	xPositions := make(map[float64]int)
	for _, block := range blocks {
		roundedX := float64(int(block.X/5)) * 5
		xPositions[roundedX]++
	}

	type xCount struct {
		x     float64
		count int
	}
	var xCounts []xCount
	for x, count := range xPositions {
		xCounts = append(xCounts, xCount{x, count})
	}

	sort.Slice(xCounts, func(i, j int) bool {
		return xCounts[i].count > xCounts[j].count
	})

	if len(xCounts) < 2 {
		// Fallback to midpoint if clustering fails
		return g.reorderByMidpoint(blocks, pageWidth)
	}

	leftColX := xCounts[0].x
	rightColX := xCounts[1].x

	// Ensure left < right
	if leftColX > rightColX {
		leftColX, rightColX = rightColX, leftColX
	}

	// Calculate boundary between columns (midpoint between the two clusters)
	boundary := (leftColX + rightColX) / 2

	var leftBlocks, rightBlocks []TextBlock

	for _, block := range blocks {
		if block.X < boundary {
			leftBlocks = append(leftBlocks, block)
		} else {
			rightBlocks = append(rightBlocks, block)
		}
	}

	// Sort each column by Y position (top to bottom)
	sort.Slice(leftBlocks, func(i, j int) bool {
		return leftBlocks[i].Y < leftBlocks[j].Y
	})

	sort.Slice(rightBlocks, func(i, j int) bool {
		return rightBlocks[i].Y < rightBlocks[j].Y
	})

	// Find where left column ends (last Y position in left column)
	var leftEndY float64 = 0
	if len(leftBlocks) > 0 {
		leftEndY = leftBlocks[len(leftBlocks)-1].Y
	}

	// Find where right column starts (first Y position in right column)
	var rightStartY float64 = 999999
	if len(rightBlocks) > 0 {
		rightStartY = rightBlocks[0].Y
	}

	// If right column starts BEFORE left column ends, we need interleaved reading
	// Otherwise, simple concatenation is fine (left then right)
	interleaved := rightStartY < leftEndY

	log.Debug().
		Float64("left_col_x", leftColX).
		Float64("right_col_x", rightColX).
		Float64("boundary", boundary).
		Int("left_blocks", len(leftBlocks)).
		Int("right_blocks", len(rightBlocks)).
		Float64("left_end_y", leftEndY).
		Float64("right_start_y", rightStartY).
		Bool("interleaved", interleaved).
		Msg("Reordered blocks for column layout")

	if !interleaved {
		// Simple case: left column completely above right column
		return append(leftBlocks, rightBlocks...)
	}

	// Complex case: columns run in parallel
	// Read left column completely, THEN right column
	// (Most common case for academic papers and patents)
	return append(leftBlocks, rightBlocks...)
}

// reorderByMidpoint is a fallback reordering using simple midpoint split
func (g *GoFitzExtractor) reorderByMidpoint(blocks []TextBlock, pageWidth float64) []TextBlock {
	midpoint := pageWidth / 2
	var leftBlocks, rightBlocks []TextBlock

	for _, block := range blocks {
		if block.X < midpoint {
			leftBlocks = append(leftBlocks, block)
		} else {
			rightBlocks = append(rightBlocks, block)
		}
	}

	sort.Slice(leftBlocks, func(i, j int) bool { return leftBlocks[i].Y < leftBlocks[j].Y })
	sort.Slice(rightBlocks, func(i, j int) bool { return rightBlocks[i].Y < rightBlocks[j].Y })

	return append(leftBlocks, rightBlocks...)
}

// joinBlocks combines text blocks into a single string with smart merging
func (g *GoFitzExtractor) joinBlocks(blocks []TextBlock) string {
	if len(blocks) == 0 {
		return ""
	}

	var result strings.Builder
	result.WriteString(blocks[0].Text)

	for i := 1; i < len(blocks); i++ {
		prevBlock := blocks[i-1]
		currBlock := blocks[i]

		prevText := strings.TrimSpace(prevBlock.Text)
		currText := strings.TrimSpace(currBlock.Text)

		if prevText == "" || currText == "" {
			result.WriteString("\n")
			result.WriteString(currText)
			continue
		}

		// Check if blocks should be merged (same line or continuation)
		yDiff := currBlock.Y - prevBlock.Y

		// If Y is very close (<5pt), it's likely same line - use space
		if yDiff < 5 {
			result.WriteString(" ")
			result.WriteString(currText)
			continue
		}

		// Check if previous line ends with continuation (no punctuation)
		lastChar := prevText[len(prevText)-1]
		endsWithPunctuation := lastChar == '.' || lastChar == '!' || lastChar == '?' ||
		                       lastChar == ':' || lastChar == ';' || lastChar == ','

		// Check if current line starts with lowercase (continuation)
		firstChar := rune(currText[0])
		startsWithLower := firstChar >= 'a' && firstChar <= 'z'

		// If previous line doesn't end with punctuation and next starts with lower, merge with space
		if !endsWithPunctuation && startsWithLower {
			result.WriteString(" ")
			result.WriteString(currText)
		} else {
			// New line
			result.WriteString("\n")
			result.WriteString(currText)
		}
	}

	return result.String()
}

// stripHTMLTags removes HTML tags from text
func stripHTMLTags(html string) string {
	// Remove HTML tags but preserve entities
	tagRegex := regexp.MustCompile(`<[^>]+>`)
	text := tagRegex.ReplaceAllString(html, "")

	// Decode common HTML entities
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")
	text = strings.ReplaceAll(text, "&nbsp;", " ")

	return text
}

// ExtractAllPages extracts text from all pages with page separators
func (g *GoFitzExtractor) ExtractAllPages(pdfPath string) (string, error) {
	doc, err := fitz.New(pdfPath)
	if err != nil {
		return "", fmt.Errorf("failed to open PDF: %w", err)
	}
	defer doc.Close()

	var result strings.Builder

	for i := 0; i < doc.NumPage(); i++ {
		pageText, err := doc.Text(i)
		if err != nil {
			log.Warn().Err(err).Int("page", i+1).Msg("Failed to extract text from page")
			pageText = "[Page extraction failed]"
		} else {
			// Clean the text
			pageText = g.cleanText(pageText, i+1)
		}

		// Add page separator
		if i > 0 {
			result.WriteString("\n\n")
		}
		result.WriteString(fmt.Sprintf("=== Page %d ===\n", i+1))
		result.WriteString(pageText)
	}

	return result.String(), nil
}