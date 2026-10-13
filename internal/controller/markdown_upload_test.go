package controller

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"shanyangcode.com/pilot/pkg/types"
)

type captureMarkdownService struct {
	fakeKnowledge
	doc *types.KnowledgeDocument
}

func (s *captureMarkdownService) IndexDocument(_ context.Context, doc *types.KnowledgeDocument) error {
	s.doc = doc
	doc.DocID = "test"
	doc.ChunkCount = 2
	return nil
}

func TestMarkdownFileUploadAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, filename, content string
		status                  int
	}{
		{"markdown", "guide.md", "    SELECT 1;\n\n# Guide\n中文", 200},
		{"word", "guide.docx", "text", 400},
		{"disguised pdf", "guide.md", "%PDF-1.7\nnot Markdown", 400},
		{"binary", "guide.md", "abc\x00def", 400},
		{"invalid utf8", "guide.md", string([]byte{0xff, 0xfe}), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &captureMarkdownService{}
			body := &bytes.Buffer{}
			writer := multipart.NewWriter(body)
			file, err := writer.CreateFormFile("file", tc.filename)
			if err != nil {
				t.Fatal(err)
			}
			file.Write([]byte(tc.content))
			writer.Close()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/upload", body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			response := httptest.NewRecorder()
			newKnowledgeRouter(service).ServeHTTP(response, req)
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			if tc.status == 200 && (service.doc.Content != tc.content || service.doc.Metadata["format"] != "markdown" || service.doc.Metadata["source_filename"] != tc.filename) {
				t.Fatalf("source lost %+v", service.doc)
			}
			if tc.status != 200 && service.doc != nil {
				t.Fatal("invalid file reached indexer")
			}
		})
	}
}
