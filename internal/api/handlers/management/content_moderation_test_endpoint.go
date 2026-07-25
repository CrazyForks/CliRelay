package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
)

func (h *Handler) PostContentModerationProfileTest(c *gin.Context) {
	var body struct {
		Input string `json:"input"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Input) == "" {
		contentModerationBadRequest(c, "input is required")
		return
	}
	profile, err := h.contentModerationStore().GetProfile(c.Request.Context(), effectiveTenantID(c), c.Param("id"))
	if err != nil {
		contentModerationError(c, err)
		return
	}
	// Dry-run evaluates the saved configuration without enabling it for live traffic.
	profile.Mode = contentmoderation.ModePreBlock
	decision := contentmoderation.NewEvaluator(nil).Evaluate(c.Request.Context(), profile, body.Input)
	c.JSON(http.StatusOK, decision)
}
