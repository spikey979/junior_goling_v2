package mupdf

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
)

// Extractor handles text extraction using MuPDF utilities
type Extractor struct{}

// NewExtractor creates a new MuPDF text extractor
func NewExtractor() *Extractor {
	return &Extractor{}
}

// IsAvailable checks if MuPDF tools are available
func (e *Extractor) IsAvailable() bool {
	_, err := exec.LookPath("mutool")
	return err == nil
}

// GetPageCount returns the number of pages in a PDF
func (e *Extractor) GetPageCount(pdfPath string) (int, error) {
	log.Debug().Str("pdf", pdfPath).Msg("Getting page count with mutool")

	// Use mutool info command to get page count
	cmd := exec.Command("mutool", "info", pdfPath)

	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("failed to get PDF info with mutool: %w", err)
	}

	// Parse output to find page count
	// mutool info output format: "Pages: N"
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "Pages:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				count, err := strconv.Atoi(parts[1])
				if err != nil {
					return 0, fmt.Errorf("failed to parse page count: %w", err)
				}
				return count, nil
			}
		}
	}

	// Alternative: try mutool pages command
	cmd = exec.Command("mutool", "pages", pdfPath)
	output, err = cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("failed to get page count: %w", err)
	}

	// Count lines (each line represents a page)
	lines = strings.Split(strings.TrimSpace(string(output)), "\n")
	return len(lines), nil
}

// ExtractText extracts all text from a PDF file
func (e *Extractor) ExtractText(pdfPath string) (string, error) {
	log.Debug().Str("pdf", pdfPath).Msg("Extracting all text with MuPDF")

	// Use mutool draw command to extract text
	cmd := exec.Command("mutool", "draw", "-F", "txt", pdfPath)

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("mutool failed: %s", string(exitErr.Stderr))
		}
		return "", fmt.Errorf("failed to extract text with mutool: %w", err)
	}

	text := string(output)
	log.Debug().Int("chars", len(text)).Msg("Extracted text from PDF")

	return text, nil
}

// ExtractTextByPage extracts text from a specific page
func (e *Extractor) ExtractTextByPage(pdfPath string, pageNum int) (string, error) {
	log.Debug().Str("pdf", pdfPath).Int("page", pageNum).Msg("Extracting page text with MuPDF")

	// Use mutool draw to extract text from specific page
	cmd := exec.Command("mutool", "draw", "-F", "txt", pdfPath, fmt.Sprintf("%d", pageNum))

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("mutool failed for page %d: %s", pageNum, string(exitErr.Stderr))
		}
		return "", fmt.Errorf("failed to extract text from page %d: %w", pageNum, err)
	}

	rawText := string(output)

	// Clean the extracted text
	cleanedText := e.cleanText(rawText, pageNum)

	log.Debug().
		Int("page", pageNum).
		Int("raw_chars", len(rawText)).
		Int("cleaned_chars", len(cleanedText)).
		Msg("Extracted and cleaned page text")

	return cleanedText, nil
}

// cleanText removes headers, footers, and other artifacts
func (e *Extractor) cleanText(text string, pageNum int) string {
	lines := strings.Split(text, "\n")
	var cleanedLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip empty lines
		if trimmed == "" {
			continue
		}

		// Skip lines that are just page numbers
		if e.isPageNumber(trimmed, pageNum) {
			continue
		}

		// Skip lines that look like headers/footers (all caps, short)
		if e.isHeaderFooter(trimmed) {
			continue
		}

		// Skip lines with just special characters or numbers
		if e.isNoise(trimmed) {
			continue
		}

		cleanedLines = append(cleanedLines, line)
	}

	// Join lines and fix broken sentences
	result := strings.Join(cleanedLines, "\n")
	result = e.fixBrokenLines(result)

	return strings.TrimSpace(result)
}

// isPageNumber checks if a line is likely a page number
func (e *Extractor) isPageNumber(line string, pageNum int) bool {
	// Direct page number
	if line == strconv.Itoa(pageNum) {
		return true
	}

	// Page number with prefix/suffix
	patterns := []string{
		fmt.Sprintf("Page %d", pageNum),
		fmt.Sprintf("- %d -", pageNum),
		fmt.Sprintf("[%d]", pageNum),
		fmt.Sprintf("%d.", pageNum),
	}

	for _, pattern := range patterns {
		if strings.EqualFold(line, pattern) {
			return true
		}
	}

	return false
}

// isHeaderFooter checks if a line looks like a header or footer
func (e *Extractor) isHeaderFooter(line string) bool {
	// Too short to be meaningful content
	if len(line) < 3 {
		return true
	}

	// All caps and short (likely header)
	if len(line) < 50 && strings.ToUpper(line) == line {
		// But allow if it contains multiple words (could be a title)
		words := strings.Fields(line)
		if len(words) <= 2 {
			return true
		}
	}

	// Common footer patterns
	footerPatterns := []string{
		"CONFIDENTIAL",
		"COPYRIGHT",
		"ALL RIGHTS RESERVED",
		"PROPRIETARY",
		"PAGE",
	}

	upperLine := strings.ToUpper(line)
	for _, pattern := range footerPatterns {
		if strings.Contains(upperLine, pattern) && len(line) < 100 {
			return true
		}
	}

	return false
}

// isNoise checks if a line is just noise (special chars, standalone numbers, etc)
func (e *Extractor) isNoise(line string) bool {
	// Just numbers
	if _, err := strconv.Atoi(line); err == nil {
		return true
	}

	// Just special characters
	specialOnly := true
	for _, r := range line {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			specialOnly = false
			break
		}
	}

	return specialOnly
}

// fixBrokenLines attempts to fix lines broken mid-sentence
func (e *Extractor) fixBrokenLines(text string) string {
	lines := strings.Split(text, "\n")
	var fixed []string

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		// If line doesn't end with sentence terminator and next line doesn't start with capital
		if i < len(lines)-1 {
			trimmed := strings.TrimSpace(line)
			nextTrimmed := strings.TrimSpace(lines[i+1])

			if trimmed != "" && nextTrimmed != "" {
				lastChar := trimmed[len(trimmed)-1]
				isSentenceEnd := lastChar == '.' || lastChar == '!' || lastChar == '?' || lastChar == ':' || lastChar == ';'

				if !isSentenceEnd && len(nextTrimmed) > 0 {
					firstChar := nextTrimmed[0]
					startsWithLower := firstChar >= 'a' && firstChar <= 'z'

					// If line doesn't end with sentence terminator and next starts with lowercase,
					// it's probably a broken line
					if startsWithLower && !strings.HasSuffix(trimmed, "-") {
						// Join with space
						fixed = append(fixed, trimmed+" "+nextTrimmed)
						i++ // Skip next line since we merged it
						continue
					}
				}
			}
		}

		fixed = append(fixed, line)
	}

	return strings.Join(fixed, "\n")
}

// ExtractAllPages extracts text from all pages and returns it with page separators
func (e *Extractor) ExtractAllPages(pdfPath string) (string, error) {
	pageCount, err := e.GetPageCount(pdfPath)
	if err != nil {
		return "", fmt.Errorf("failed to get page count: %w", err)
	}

	var result strings.Builder

	for i := 1; i <= pageCount; i++ {
		pageText, err := e.ExtractTextByPage(pdfPath, i)
		if err != nil {
			log.Warn().Err(err).Int("page", i).Msg("Failed to extract text from page")
			// Continue with other pages even if one fails
			pageText = "[Page extraction failed]"
		}

		// Add page separator
		if i > 1 {
			result.WriteString("\n\n")
		}
		result.WriteString(fmt.Sprintf("=== Page %d ===\n", i))
		result.WriteString(pageText)
	}

	return result.String(), nil
}