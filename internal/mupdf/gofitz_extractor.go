package mupdf

import (
	"fmt"
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

// ExtractTextByPage extracts text from a specific page using go-fitz
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