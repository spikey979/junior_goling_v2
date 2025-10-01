package pdftest

import (
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"time"
)

// PageProbe captures the result of probing a single PDF page.
type PageProbe struct {
	PageIndex int    `json:"page_index"`
	CharCount int    `json:"char_count"`
	Sampled   bool   `json:"sampled"`
	Err       string `json:"err,omitempty"`
}

// Diagnostics provides detailed information about the text-extractability check.
type Diagnostics struct {
	FilePath           string      `json:"file_path"`
	TotalPages         int         `json:"total_pages"`
	SampledPages       []int       `json:"sampled_pages"`
	TotalCharsInSample int         `json:"total_chars_in_sample"`
	Threshold          int         `json:"threshold"`
	Probes             []PageProbe `json:"probes"`
	HasExtractableText bool        `json:"has_extractable_text"`
	DurationMs         int64       `json:"duration_ms"`
}

// DefaultThreshold is used when a non-positive threshold is passed in.
const DefaultThreshold = 300

// whitespaceRegex matches any whitespace (Unicode-aware). Used to strip whitespace.
var whitespaceRegex = regexp.MustCompile(`\s+`)

// stripWhitespace removes all Unicode whitespace from the given string.
func stripWhitespace(s string) string {
	return whitespaceRegex.ReplaceAllString(s, "")
}

// Doc abstracts a PDF document for text extraction.
type Doc interface {
	NumPage() int
	Page(i int) (Page, error)
	Close() error
}

// Page abstracts a single PDF page for text extraction.
type Page interface {
	Text() (string, error)
	Close()
}

// Opener abstracts opening a PDF path into a Doc.
type Opener interface {
	Open(path string) (Doc, error)
}

// defaultOpener is provided in doc_open_fitz.go using go-fitz.
var defaultOpener Opener

// setDefaultOpener allows swapping the default opener, useful for tests or alternate backends.
func setDefaultOpener(o Opener) { defaultOpener = o }

// HasExtractableText checks if a PDF at pdfPath contains extractable text using sampling.
// If threshold <= 0, DefaultThreshold is used.
func HasExtractableText(pdfPath string, threshold int) (bool, *Diagnostics, error) {
	return HasExtractableTextWithPages(pdfPath, threshold, nil)
}

// HasExtractableTextWithPages is like HasExtractableText but allows specifying explicit page indices to sample.
// If pages is nil, the standard sampling heuristic is used.
func HasExtractableTextWithPages(pdfPath string, threshold int, pages []int) (bool, *Diagnostics, error) {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}

	if defaultOpener == nil {
		return false, nil, errors.New("no PDF opener configured")
	}

	start := time.Now()
	d, err := defaultOpener.Open(pdfPath)
	if err != nil {
		return false, nil, fmt.Errorf("failed to open PDF: %w", err)
	}
	defer d.Close()

	total := d.NumPage()
	if total <= 0 {
		return false, &Diagnostics{
			FilePath:           pdfPath,
			TotalPages:         total,
			SampledPages:       []int{},
			TotalCharsInSample: 0,
			Threshold:          threshold,
			Probes:             nil,
			HasExtractableText: false,
			DurationMs:         time.Since(start).Milliseconds(),
		}, nil
	}

	var sampleIdx []int
	if pages != nil {
		sampleIdx = normalizeAndClampPages(pages, total)
	} else {
		sampleIdx = sampleIndices(total)
	}

	probes := make([]PageProbe, 0, len(sampleIdx))
	totalChars := 0

	for _, idx := range sampleIdx {
		probe := PageProbe{PageIndex: idx, Sampled: true}
		p, perr := d.Page(idx)
		if perr != nil {
			probe.Err = perr.Error()
			probes = append(probes, probe)
			continue
		}
		text, terr := p.Text()
		p.Close()
		if terr != nil {
			probe.Err = terr.Error()
			probes = append(probes, probe)
			continue
		}

		cleaned := stripWhitespace(text)
		// Unicode-aware: count runes after removing whitespace
		count := len([]rune(cleaned))
		probe.CharCount = count
		totalChars += count
		probes = append(probes, probe)

		if totalChars >= threshold {
			// Early exit for speed
			break
		}
	}

	diag := &Diagnostics{
		FilePath:           pdfPath,
		TotalPages:         total,
		SampledPages:       sampleIdx,
		TotalCharsInSample: totalChars,
		Threshold:          threshold,
		Probes:             probes,
		HasExtractableText: totalChars >= threshold,
		DurationMs:         time.Since(start).Milliseconds(),
	}

	return diag.HasExtractableText, diag, nil
}

// sampleIndices implements the sampling heuristic:
// up to 5 pages: 0, mid, last, plus 1–2 random distinct if N >= 6.
// If N < 5, sample all pages [0..N-1].
func sampleIndices(total int) []int {
	if total <= 0 {
		return []int{}
	}
	if total <= 5 {
		idx := make([]int, total)
		for i := 0; i < total; i++ {
			idx[i] = i
		}
		return idx
	}

	// Base: first, mid, last
	mid := total / 2
	base := map[int]struct{}{0: {}, mid: {}, total - 1: {}}

	// Add up to two random distinct indices not in base
	need := 5 - len(base)
	if need < 0 {
		need = 0
	}
	// Seed per call – fine for sampling
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	for len(base) < 5 {
		cand := rnd.Intn(total)
		if _, ok := base[cand]; ok {
			continue
		}
		base[cand] = struct{}{}
	}

	out := make([]int, 0, 5)
	for i := range base {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}

// normalizeAndClampPages ensures indices are unique, in-range, and sorted.
func normalizeAndClampPages(pages []int, total int) []int {
	m := make(map[int]struct{})
	for _, p := range pages {
		if p < 0 || p >= total {
			continue
		}
		m[p] = struct{}{}
	}
	out := make([]int, 0, len(m))
	for i := range m {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}
