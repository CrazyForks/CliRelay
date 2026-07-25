package routes

import (
	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
)

func registerManagementContentModerationRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	group.GET("/content-moderation/metrics", h.GetContentModerationMetrics)
	group.GET("/content-moderation/profiles", h.GetContentModerationProfiles)
	group.POST("/content-moderation/profiles", h.PostContentModerationProfile)
	group.GET("/content-moderation/profiles/:id", h.GetContentModerationProfile)
	group.PATCH("/content-moderation/profiles/:id", h.PatchContentModerationProfile)
	group.DELETE("/content-moderation/profiles/:id", h.DeleteContentModerationProfile)
	group.POST("/content-moderation/profiles/:id/test", h.PostContentModerationProfileTest)
	group.GET("/content-moderation/channels", h.GetContentModerationChannels)
	group.PATCH("/content-moderation/channel-bindings", h.PatchContentModerationChannelBindings)
}
