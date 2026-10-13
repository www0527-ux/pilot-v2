package controller

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"shanyangcode.com/pilot/internal/httpapi"
	"shanyangcode.com/pilot/internal/identity"
	"shanyangcode.com/pilot/internal/knowledge"
)

type ExperienceController struct{ service *knowledge.Service }

func NewExperienceController(s *knowledge.Service) *ExperienceController {
	return &ExperienceController{service: s}
}
func (c *ExperienceController) Register(g *gin.RouterGroup) {
	g.GET("", c.List)
	g.GET("/settings", c.Settings)
	g.POST("/settings", c.SetSettings)
	g.GET("/jobs/:run_id", c.Job)
	g.POST("/jobs/:run_id", c.Enqueue)
	g.GET("/issues", c.Issues)
	g.GET("/unassigned", c.Unassigned)
	g.POST("/issues/:issue_id", c.EditIssue)
	g.POST("/sessions/:session_id/organize", c.OrganizeSession)
	g.POST("/sessions/:session_id/close", c.OrganizeSession)
	g.GET("/:entry_id", c.Get)
	g.GET("/:entry_id/versions", c.Versions)
	g.POST("/:entry_id", c.Update)
}

func (c *ExperienceController) Unassigned(g *gin.Context) {
	items, err := c.service.UnassignedEvents(g.Request.Context(), identity.Owner(g.Request.Context()), g.Query("session_id"))
	if err != nil {
		experienceError(g, err)
		return
	}
	httpapi.OK(g, "待归属事件", items)
}

func (c *ExperienceController) Issues(g *gin.Context) {
	items, err := c.service.ListIssues(g.Request.Context(), identity.Owner(g.Request.Context()), g.Query("session_id"))
	if err != nil {
		experienceError(g, err)
		return
	}
	httpapi.OK(g, "本次问题", items)
}
func (c *ExperienceController) EditIssue(g *gin.Context) {
	var input knowledge.IssueEdit
	g.Request.Body = http.MaxBytesReader(g.Writer, g.Request.Body, 128*1024)
	if err := g.ShouldBindJSON(&input); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument("问题调整格式错误"))
		return
	}
	item, err := c.service.EditIssue(g.Request.Context(), identity.Owner(g.Request.Context()), g.Param("issue_id"), input)
	if err != nil {
		experienceError(g, err)
		return
	}
	httpapi.OK(g, "问题已调整", item)
}
func (c *ExperienceController) OrganizeSession(g *gin.Context) {
	ctx := g.Request.Context()
	owner := identity.Owner(ctx)
	session := g.Param("session_id")
	items, err := c.service.FinalizeSession(ctx, owner, session)
	if err == nil {
		err = c.service.RequeueSession(ctx, owner, session)
	}
	if err != nil {
		experienceError(g, err)
		return
	}
	httpapi.OK(g, "会话整理已排队", items)
}
func experienceError(g *gin.Context, err error) {
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		httpapi.Fail(g, httpapi.NotFound("记录不存在或不属于当前账号"))
	case errors.Is(err, knowledge.ErrConflict):
		httpapi.Fail(g, httpapi.NewError(http.StatusConflict, "CONFLICT", "内容已更新，请重新加载后再提交"))
	case errors.Is(err, knowledge.ErrInvalid):
		httpapi.Fail(g, httpapi.InvalidArgument(err.Error()))
	default:
		slog.WarnContext(g.Request.Context(), "experience request failed", "error", err)
		httpapi.Fail(g, httpapi.DependencyUnavailable("经验知识操作失败，请重试；未保存的编辑会保留"))
	}
}
func (c *ExperienceController) List(g *gin.Context) {
	page, _ := strconv.Atoi(g.Query("page"))
	q := g.Query("query")
	if len([]rune(q)) > 1000 {
		experienceError(g, knowledge.ErrInvalid)
		return
	}
	items, err := c.service.List(g.Request.Context(), identity.Owner(g.Request.Context()), knowledge.Filter{Query: q, Service: g.Query("service"), Type: g.Query("type"), Tab: g.Query("tab"), Review: g.Query("review"), Verification: g.Query("verification"), From: g.Query("from"), To: g.Query("to"), RunID: g.Query("run_id"), Page: page})
	if err != nil {
		experienceError(g, err)
		return
	}
	httpapi.OK(g, "经验列表", items)
}
func (c *ExperienceController) Get(g *gin.Context) {
	version, _ := strconv.Atoi(g.Query("version"))
	d, err := c.service.Get(g.Request.Context(), identity.Owner(g.Request.Context()), g.Param("entry_id"), version)
	if err != nil {
		experienceError(g, err)
		return
	}
	httpapi.OK(g, "经验详情", d)
}
func (c *ExperienceController) Versions(g *gin.Context) {
	items, err := c.service.Versions(g.Request.Context(), identity.Owner(g.Request.Context()), g.Param("entry_id"))
	if err != nil {
		experienceError(g, err)
		return
	}
	httpapi.OK(g, "历史版本", items)
}
func (c *ExperienceController) Update(g *gin.Context) {
	var input knowledge.Edit
	g.Request.Body = http.MaxBytesReader(g.Writer, g.Request.Body, 512*1024)
	if err := g.ShouldBindJSON(&input); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument("请求体不是合法的经验内容"))
		return
	}
	d, err := c.service.Update(g.Request.Context(), identity.Owner(g.Request.Context()), g.Param("entry_id"), input)
	if err != nil {
		experienceError(g, err)
		return
	}
	httpapi.OK(g, "经验已保存", d)
}
func (c *ExperienceController) Settings(g *gin.Context) {
	s, err := c.service.GetSettings(g.Request.Context(), identity.Owner(g.Request.Context()))
	if err != nil {
		experienceError(g, err)
		return
	}
	httpapi.OK(g, "整理设置", s)
}
func (c *ExperienceController) SetSettings(g *gin.Context) {
	var input struct {
		Automatic bool `json:"automatic"`
	}
	if err := g.ShouldBindJSON(&input); err != nil {
		httpapi.Fail(g, httpapi.InvalidArgument("设置格式错误"))
		return
	}
	s, err := c.service.SetAutomatic(g.Request.Context(), identity.Owner(g.Request.Context()), input.Automatic)
	if err != nil {
		experienceError(g, err)
		return
	}
	httpapi.OK(g, "整理设置已保存", s)
}
func (c *ExperienceController) Job(g *gin.Context) {
	job, err := c.service.GetJob(g.Request.Context(), identity.Owner(g.Request.Context()), g.Param("run_id"))
	if err != nil {
		experienceError(g, err)
		return
	}
	httpapi.OK(g, "整理任务", job)
}
func (c *ExperienceController) Enqueue(g *gin.Context) {
	job, err := c.service.Enqueue(g.Request.Context(), identity.Owner(g.Request.Context()), g.Param("run_id"), true)
	if err != nil {
		experienceError(g, err)
		return
	}
	httpapi.OK(g, "整理任务已创建", job)
}
