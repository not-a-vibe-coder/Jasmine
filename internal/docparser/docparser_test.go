package docparser

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestParseMarkdownAndText(t *testing.T) {
	mdContent := "# Whitepaper\n\nThis is a sample markdown whitepaper with **bold** text."
	res, err := ParseDocument("doc.md", []byte(mdContent))
	if err != nil {
		t.Fatalf("unexpected error parsing md: %v", err)
	}
	if !strings.Contains(res, "Whitepaper") {
		t.Errorf("expected res to contain Whitepaper, got: %s", res)
	}
}

func TestParseDOCX(t *testing.T) {
	// Construct a synthetic docx zip in memory
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	docXml := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p>
      <w:r><w:t>Hello world from Word!</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t>Second paragraph discussing terms and conditions.</w:t></w:r>
    </w:p>
  </w:body>
</w:document>`

	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("failed to create zip entry: %v", err)
	}
	_, _ = w.Write([]byte(docXml))
	_ = zw.Close()

	res, err := ParseDocument("test.docx", buf.Bytes())
	if err != nil {
		t.Fatalf("unexpected error parsing docx: %v", err)
	}

	if !strings.Contains(res, "Hello world from Word!") {
		t.Errorf("expected text not found in docx result: %s", res)
	}
	if !strings.Contains(res, "Second paragraph") {
		t.Errorf("expected second paragraph not found in docx result: %s", res)
	}
}
