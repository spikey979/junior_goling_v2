package pdftest

import (
	fitz "github.com/gen2brain/go-fitz"
)

// fitzOpener implements Opener using github.com/gen2brain/go-fitz.
type fitzOpener struct{}

func (fitzOpener) Open(path string) (Doc, error) {
	doc, err := fitz.New(path)
	if err != nil {
		return nil, err
	}
	return fitzDoc{doc}, nil
}

// Ensure default opener is set to fitz-based implementation.
func init() {
	setDefaultOpener(fitzOpener{})
}

// --- Adapters ---

type fitzDoc struct{ *fitz.Document }

func (d fitzDoc) Page(i int) (Page, error) {
	text, err := d.Document.Text(i)
	if err != nil {
		return nil, err
	}
	return &fitzPage{text: text}, nil
}

type fitzPage struct {
	text string
}

func (p *fitzPage) Text() (string, error) { return p.text, nil }
func (p *fitzPage) Close()                 { /* no-op */ }
