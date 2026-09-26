package docparser

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ledongthuc/pdf"
)

const MaxDocumentLength = 60000 // ~15,000 tokens safe limit for LLM prompt

// ParseDocument extracts plain text from supported document formats (.md, .txt, .csv, .json, .pdf, .docx).
func ParseDocument(filename string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("document is empty")
	}

	ext := strings.ToLower(filepath.Ext(filename))
	var text string
	var err error

	switch ext {
	case ".md", ".txt", ".csv", ".json", ".log":
		text = string(data)

	case ".pdf":
		text, err = parsePDF(data)

	case ".docx":
		text, err = parseDOCX(data)

	default:
		// Attempt plain text read as fallback
		text = string(data)
	}

	if err != nil {
		return "", err
	}

	trimmed := strings.TrimSpace(text)
	if len(trimmed) > MaxDocumentLength {
		trimmed = trimmed[:MaxDocumentLength] + "\n\n[... document truncated for length ...]"
	}

	return trimmed, nil
}

func parsePDF(data []byte) (string, error) {
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("failed to open PDF: %w", err)
	}

	plainText, err := reader.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("failed to extract PDF text: %w", err)
	}

	var buf bytes.Buffer
	_, err = buf.ReadFrom(plainText)
	if err != nil {
		return "", fmt.Errorf("failed to read PDF stream: %w", err)
	}

	return buf.String(), nil
}

func parseDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("failed to read docx zip archive: %w", err)
	}

	var docFile *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docFile = f
			break
		}
	}

	if docFile == nil {
		return "", fmt.Errorf("invalid docx: word/document.xml not found")
	}

	rc, err := docFile.Open()
	if err != nil {
		return "", fmt.Errorf("failed to open word/document.xml: %w", err)
	}
	defer rc.Close()

	decoder := xml.NewDecoder(rc)
	var textBuilder strings.Builder

	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("error parsing docx xml: %w", err)
		}

		switch elem := tok.(type) {
		case xml.StartElement:
			// New paragraph
			if elem.Name.Local == "p" {
				textBuilder.WriteString("\n")
			}
		case xml.CharData:
			textBuilder.Write(elem)
		}
	}

	return strings.TrimSpace(textBuilder.String()), nil
}
