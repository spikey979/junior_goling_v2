package filetype

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gabriel-vasile/mimetype"
	"github.com/rs/zerolog/log"
)

// FileTypeInfo contains detected file type information
type FileTypeInfo struct {
	MIMEType    string
	Extension   string
	IsText      bool
	NeedsOCR    bool
	Supported   bool
	Description string
}

// Detector handles file type detection using magic bytes
type Detector struct{}

// New creates a new file type detector
func New() *Detector {
	return &Detector{}
}

// Detect detects the actual file type using magic bytes, not filename
func (d *Detector) Detect(filePath string) (*FileTypeInfo, error) {
	// Detect MIME type using magic bytes
	mtype, err := mimetype.DetectFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to detect file type: %w", err)
	}

	mimeType := mtype.String()
	extension := mtype.Extension()

	log.Debug().Str("mime", mimeType).Str("ext", extension).Str("file", filePath).Msg("detected file type")

	// Special handling for ZIP-based Office formats
	// Many modern Office formats are actually ZIP files with specific structure
	if mimeType == "application/zip" || strings.Contains(mimeType, "application/x-zip") {
		// Check file extension for known Office formats
		ext := strings.ToLower(filepath.Ext(filePath))
		log.Debug().Str("zip_ext", ext).Msg("ZIP detected, checking extension")

		switch ext {
		case ".docx":
			mimeType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
			extension = ".docx"
		case ".xlsx":
			mimeType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
			extension = ".xlsx"
		case ".pptx":
			mimeType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
			extension = ".pptx"
		case ".vsdx":
			mimeType = "application/vnd.ms-visio.drawing.main+xml"
			extension = ".vsdx"
		case ".odt":
			mimeType = "application/vnd.oasis.opendocument.text"
			extension = ".odt"
		case ".ods":
			mimeType = "application/vnd.oasis.opendocument.spreadsheet"
			extension = ".ods"
		case ".odp":
			mimeType = "application/vnd.oasis.opendocument.presentation"
			extension = ".odp"
		default:
			// If it's a ZIP but not a recognized Office format, keep it as unsupported
			log.Warn().Str("ext", ext).Msg("ZIP file with unrecognized extension")
		}

		if mimeType != "application/zip" {
			log.Debug().Str("original", mtype.String()).Str("override", mimeType).Msg("overriding ZIP detection based on extension")
		}
	}

	// Special handling for OLE/CFB-based Office formats (legacy .doc, .xls, .ppt)
	// These are detected as application/x-ole-storage or application/x-cfb
	if mimeType == "application/x-ole-storage" || mimeType == "application/x-cfb" {
		// Check file extension for known legacy Office formats
		ext := strings.ToLower(filepath.Ext(filePath))
		log.Debug().Str("ole_ext", ext).Msg("OLE storage detected, checking extension")

		switch ext {
		case ".doc":
			mimeType = "application/msword"
			extension = ".doc"
		case ".xls":
			mimeType = "application/vnd.ms-excel"
			extension = ".xls"
		case ".ppt":
			mimeType = "application/vnd.ms-powerpoint"
			extension = ".ppt"
		case ".vsd":
			mimeType = "application/vnd.ms-visio.drawing"
			extension = ".vsd"
		default:
			// If it's OLE but not a recognized Office format, keep as generic
			log.Warn().Str("ext", ext).Msg("OLE storage with unrecognized extension")
		}

		if mimeType != "application/x-ole-storage" && mimeType != "application/x-cfb" {
			log.Debug().Str("original", mtype.String()).Str("override", mimeType).Msg("overriding OLE detection based on extension")
		}
	}

	info := &FileTypeInfo{
		MIMEType:  mimeType,
		Extension: extension,
	}

	// Classify the file type and determine processing needs
	d.classify(info)

	return info, nil
}

// classify determines file characteristics and processing requirements
func (d *Detector) classify(info *FileTypeInfo) {
	mimeType := info.MIMEType

	switch {
	// Plain text files - no processing needed
	case strings.HasPrefix(mimeType, "text/"):
		info.IsText = true
		info.NeedsOCR = false
		info.Supported = true
		info.Description = "Plain text file"

	// PDF files - direct OCR processing
	case mimeType == "application/pdf":
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "PDF document"

	// Microsoft Office documents - need LibreOffice conversion
	case mimeType == "application/vnd.openxmlformats-officedocument.wordprocessingml.document": // .docx
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "Microsoft Word document"

	case mimeType == "application/vnd.openxmlformats-officedocument.presentationml.presentation": // .pptx
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "Microsoft PowerPoint presentation"

	case mimeType == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": // .xlsx
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "Microsoft Excel spreadsheet"

	case mimeType == "application/msword": // .doc
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "Microsoft Word document (legacy)"

	case mimeType == "application/vnd.ms-powerpoint": // .ppt
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "Microsoft PowerPoint presentation (legacy)"

	case mimeType == "application/vnd.ms-excel": // .xls
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "Microsoft Excel spreadsheet (legacy)"

	// LibreOffice/OpenOffice documents
	case mimeType == "application/vnd.oasis.opendocument.text": // .odt
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "OpenDocument text"

	case mimeType == "application/vnd.oasis.opendocument.presentation": // .odp
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "OpenDocument presentation"

	case mimeType == "application/vnd.oasis.opendocument.spreadsheet": // .ods
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "OpenDocument spreadsheet"

	// Rich Text Format
	case mimeType == "application/rtf":
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "Rich Text Format"

	// Image files - OCR only
	case strings.HasPrefix(mimeType, "image/"):
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "Image file"

	// HTML files - extract text content
	case mimeType == "text/html":
		info.IsText = true
		info.NeedsOCR = false
		info.Supported = true
		info.Description = "HTML document"

	// XML files - treat as text
	case mimeType == "application/xml" || mimeType == "text/xml":
		info.IsText = true
		info.NeedsOCR = false
		info.Supported = true
		info.Description = "XML document"

	// JSON files - treat as text
	case mimeType == "application/json":
		info.IsText = true
		info.NeedsOCR = false
		info.Supported = true
		info.Description = "JSON document"

	// Visio documents (both legacy and modern XML-based)
	case mimeType == "application/vnd.ms-visio.drawing",
		mimeType == "application/vnd.ms-visio.drawing.main+xml":
		info.IsText = false
		info.NeedsOCR = true
		info.Supported = true
		info.Description = "Microsoft Visio drawing"

	// Default: unsupported
	default:
		info.IsText = false
		info.NeedsOCR = false
		info.Supported = false
		info.Description = fmt.Sprintf("Unsupported file type: %s", mimeType)
	}
}

// RequiresConversion checks if a file needs LibreOffice conversion to PDF
func (d *Detector) RequiresConversion(filePath string) (bool, error) {
	info, err := d.Detect(filePath)
	if err != nil {
		return false, err
	}

	// PDF files and text files don't need conversion
	if info.MIMEType == "application/pdf" || info.IsText {
		return false, nil
	}

	// Everything else that's supported needs conversion
	return info.Supported, nil
}
