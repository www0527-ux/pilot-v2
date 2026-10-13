package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"shanyangcode.com/pilot/internal/ai/retriever"
	"shanyangcode.com/pilot/internal/httpapi"
	"shanyangcode.com/pilot/pkg/types"
)

// KnowledgeService 是知识库控制器依赖的检索与写入能力。
type KnowledgeService interface {
	IndexDocument(ctx context.Context, doc *types.KnowledgeDocument) error
	Search(ctx context.Context, query string) ([]types.KnowledgeDocument, error)
	SearchDetailed(ctx context.Context, query string) ([]retriever.RetrievedDocument, error)
	DeleteDocument(ctx context.Context, docID string) (int, error)
}

// KnowledgeController 知识库控制器
type KnowledgeController struct {
	retriever KnowledgeService
}

// NewKnowledgeController 创建知识库控制器
func NewKnowledgeController(service KnowledgeService) *KnowledgeController {
	return &KnowledgeController{retriever: service}
}

// UploadRequest 上传文档请求
type UploadRequest struct {
	Title    string            `json:"title"`
	Content  string            `json:"content"`
	Category string            `json:"category"`
	Tags     []string          `json:"tags"`
	Metadata map[string]string `json:"metadata"`
}

// SearchRequest 搜索请求
type SearchRequest struct {
	Query string `json:"query"`
}

// Upload 上传文档
func (c *KnowledgeController) Upload(g *gin.Context) {
	var req UploadRequest
	if err := bindKnowledgeUpload(g, &req); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	if err := normalizeKnowledgeUpload(&req); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	req.Metadata["format"] = "markdown"

	// 添加文档
	doc := &types.KnowledgeDocument{
		Title:    req.Title,
		Content:  req.Content,
		Category: req.Category,
		Tags:     req.Tags,
		Metadata: req.Metadata,
	}
	if err := c.retriever.IndexDocument(g.Request.Context(), doc); err != nil {
		var invalid *retriever.MarkdownInputError
		if errors.As(err, &invalid) {
			httpapi.Fail(g, httpapi.InvalidArgument(invalid.Error()))
			return
		}
		slog.ErrorContext(g.Request.Context(), "add document failed", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("上传文档失败，请稍后重试"))
		return
	}

	httpapi.OK(g, "上传成功", gin.H{"id": doc.DocID, "doc_id": doc.DocID, "chunk_count": doc.ChunkCount})
}

// Keep the existing JSON API for pasted Markdown. File uploads support UTF-8
// Markdown only; PDF, Word, archives and binary data are rejected explicitly.
func bindKnowledgeUpload(g *gin.Context, req *UploadRequest) error {
	g.Request.Body = http.MaxBytesReader(g.Writer, g.Request.Body, 512<<10)
	mediaType, _, err := mime.ParseMediaType(g.GetHeader("Content-Type"))
	if err != nil {
		return fmt.Errorf("无效的 Content-Type")
	}
	if mediaType != "multipart/form-data" {
		if err := g.ShouldBindJSON(req); err != nil {
			return fmt.Errorf("请求体不是合法 JSON")
		}
		if !utf8.ValidString(req.Content) || strings.ContainsRune(req.Content, '\x00') {
			return fmt.Errorf("Markdown 必须是有效的 UTF-8 文本")
		}
		return nil
	}
	if err := g.Request.ParseMultipartForm(128 << 10); err != nil {
		return fmt.Errorf("无法读取文件，上传请求不得超过 512 KiB")
	}
	if g.Request.MultipartForm != nil {
		defer g.Request.MultipartForm.RemoveAll()
	}
	files := g.Request.MultipartForm.File["file"]
	if len(files) != 1 {
		return fmt.Errorf("请选择一个 Markdown 文件")
	}
	header := files[0]
	filename := path.Base(strings.ReplaceAll(header.Filename, "\\", "/"))
	ext := strings.ToLower(path.Ext(filename))
	if ext != ".md" && ext != ".markdown" {
		return fmt.Errorf("目前仅支持 .md 或 .markdown 文件")
	}
	if header.Size > 256<<10 {
		return fmt.Errorf("Markdown 文件不得超过 256 KiB")
	}
	file, err := header.Open()
	if err != nil {
		return fmt.Errorf("无法读取 Markdown 文件")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (256<<10)+1))
	if err != nil || len(data) > 256<<10 {
		return fmt.Errorf("Markdown 文件读取失败或超过大小限制")
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) || bytes.ContainsRune(data, '\x00') || !strings.HasPrefix(http.DetectContentType(data), "text/") {
		return fmt.Errorf("文件不是有效的 UTF-8 Markdown 文本")
	}
	req.Title, req.Category = g.PostForm("title"), g.PostForm("category")
	if strings.TrimSpace(req.Title) == "" {
		req.Title = strings.TrimSuffix(filename, path.Ext(filename))
	}
	if strings.TrimSpace(req.Category) == "" {
		req.Category = "runbook"
	}
	req.Content = string(data)
	req.Metadata = map[string]string{"source_filename": filename}
	return nil
}

// Search 搜索文档
func (c *KnowledgeController) Search(g *gin.Context) {
	req, ok := bindSearchRequest(g)
	if !ok {
		return
	}

	docs, err := c.retriever.Search(g.Request.Context(), req.Query)
	if err != nil {
		slog.ErrorContext(g.Request.Context(), "search documents failed", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("搜索失败，请稍后重试"))
		return
	}

	httpapi.OK(g, "搜索成功", docs)
}

// SearchDetailed 搜索文档并返回 BM25、向量和融合等多路召回信息。
func (c *KnowledgeController) SearchDetailed(g *gin.Context) {
	req, ok := bindSearchRequest(g)
	if !ok {
		return
	}

	results, err := c.retriever.SearchDetailed(g.Request.Context(), req.Query)
	if err != nil {
		slog.ErrorContext(g.Request.Context(), "search detailed documents failed", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("详细搜索失败，请稍后重试"))
		return
	}

	httpapi.OK(g, "搜索成功", results)
}

// Delete 删除文档
func (c *KnowledgeController) Delete(g *gin.Context) {
	docID, err := normalizeKnowledgeDocID(g.Query("id"))
	if err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return
	}

	deleted, err := c.retriever.DeleteDocument(g.Request.Context(), docID)
	if err != nil {
		slog.ErrorContext(g.Request.Context(), "delete document failed", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("删除失败，请稍后重试"))
		return
	}

	httpapi.OK(g, "删除成功", gin.H{"doc_id": docID, "deleted_count": deleted, "existed": deleted > 0})
}

func bindSearchRequest(g *gin.Context) (*SearchRequest, bool) {
	var req SearchRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument("请求体不是合法 JSON"))
		return nil, false
	}
	if err := normalizeKnowledgeSearch(&req); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
		return nil, false
	}
	return &req, true
}
