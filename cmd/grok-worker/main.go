package main

import (
	"os"

	"github.com/gin-gonic/gin"
	"github.com/xiaolajiaoyyds/regplatformm/internal/worker"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "7860" // HuggingFace 默认端口
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// 处理端点 — 带并发限流（超限返回 503，CF Worker 自动转发下一节点）
	handler := []gin.HandlerFunc{worker.ConcurrencyLimiter(), worker.GrokProtocolRegisterHandler}
	r.POST("/api/v1/process", handler...)
	// 兼容 VPS 直连隧道域名（不经过 CF Worker 路由重写时路径为 /grok/process）
	r.POST("/grok/process", handler...)

	// 健康检查（精简响应，仅暴露 CF Worker 需要的最小字段）
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok", "concurrency": worker.ConcurrencyStats()})
	})

	// 根路径
	r.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"model":   "bert-base-multilingual",
			"task":    "token-classification",
			"framework": "onnxruntime",
			"version": "2.1.0",
			"status":  "ready",
		})
	})

	r.Run(":" + port)
}
