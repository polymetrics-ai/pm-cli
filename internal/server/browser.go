package server

import (
	"embed"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed browser_assets/*
var embeddedBrowserAssets embed.FS

type localBrowserFile struct {
	route string
	file  string
}

func registerBrowserRoutes(router *gin.Engine) error {
	if router == nil {
		return errors.New("browser router is required")
	}
	files := []localBrowserFile{
		{route: "/login", file: "browser_assets/login.html"},
		{route: "/login/device", file: "browser_assets/device.html"},
		{route: "/admin/credentials", file: "browser_assets/credentials.html"},
		{route: browserCallbackPath, file: "browser_assets/callback.html"},
		{route: "/assets/pm.css", file: "browser_assets/pm.css"},
		{route: "/assets/login.js", file: "browser_assets/login.js"},
		{route: "/assets/callback.js", file: "browser_assets/callback.js"},
		{route: "/assets/device.js", file: "browser_assets/device.js"},
		{route: "/assets/credentials.js", file: "browser_assets/credentials.js"},
	}
	for _, file := range files {
		body, err := embeddedBrowserAssets.ReadFile(file.file)
		if err != nil {
			return errors.New("embedded browser asset is unavailable")
		}
		contentType := "text/html; charset=utf-8"
		switch file.route {
		case "/assets/pm.css":
			contentType = "text/css; charset=utf-8"
		case "/assets/login.js", "/assets/callback.js", "/assets/device.js", "/assets/credentials.js":
			contentType = "text/javascript; charset=utf-8"
		}
		route := file.route
		router.GET(route, func(context *gin.Context) {
			setBrowserSecurityHeaders(context)
			context.Data(http.StatusOK, contentType, body)
		})
	}
	return nil
}

func setBrowserSecurityHeaders(context *gin.Context) {
	context.Header("Cache-Control", "no-store")
	context.Header("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	context.Header("Cross-Origin-Opener-Policy", "same-origin")
	context.Header("Cross-Origin-Resource-Policy", "same-origin")
	context.Header("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
	context.Header("Referrer-Policy", "no-referrer")
	context.Header("X-Content-Type-Options", "nosniff")
	context.Header("X-Frame-Options", "DENY")
}
