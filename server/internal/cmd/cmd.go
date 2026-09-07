/*
SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
SPDX-License-Identifier: MIT
*/

package cmd

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"stackChan/internal/boot"
	"stackChan/internal/controller/admin"
	"stackChan/internal/controller/appstore"
	"stackChan/internal/controller/dance"
	"stackChan/internal/controller/device"
	"stackChan/internal/controller/file"
	"stackChan/internal/controller/friend"
	"stackChan/internal/controller/pano"
	"stackChan/internal/controller/post"
	"stackChan/internal/controller/stackchandevice"
	"stackChan/internal/controller/user"
	"stackChan/internal/controller/xiaozhi"
	"stackChan/internal/meetingmetrics"
	"stackChan/internal/middleware"
	"stackChan/internal/pairing"
	"stackChan/internal/web_socket"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"
	"github.com/gogf/gf/v2/os/gcmd"
	"github.com/gogf/gf/v2/os/gfile"
)

var (
	Main = gcmd.Command{
		Name:  "main",
		Usage: "main",
		Brief: "start http server",
		Func: func(ctx context.Context, parser *gcmd.Parser) (err error) {
			closeMeetingCluster, err := web_socket.ConfigureMeetingCluster(ctx)
			if err != nil {
				return err
			}
			defer closeMeetingCluster()
			closePairingRepository, err := pairing.ConfigureDefaultRepository(ctx)
			if err != nil {
				return err
			}
			defer closePairingRepository()
			closeMeetingStore, err := configureMeetingSessionStore(ctx)
			if err != nil {
				return err
			}
			defer closeMeetingStore()

			s := g.Server()
			s.SetClientMaxBodySize(100 * 1024 * 1024)

			s.Use(middleware.CORS)

			s.BindHandler("/stackChan/ws", web_socket.Handler)
			authenticateUser := pairing.UserAuthenticator(pairing.LocalUserAuthenticator)
			if jwksURL := os.Getenv("SUPABASE_JWKS_URL"); jwksURL != "" {
				audience := os.Getenv("SUPABASE_JWT_AUDIENCE")
				if audience == "" {
					audience = "authenticated"
				}
				jwtAuthenticator := &pairing.JWTAuthenticator{
					Issuer:   os.Getenv("SUPABASE_JWT_ISSUER"),
					Audience: audience,
					JWKSURL:  jwksURL,
				}
				// Keep the explicit local-dev credential usable for the local demo
				// even when the same binary also has production JWT settings.
				// All other requests are verified against Supabase JWKS.
				jwtAuthenticate := jwtAuthenticator.Authenticate
				authenticateUser = func(r *ghttp.Request) (string, error) {
					if userID, localErr := pairing.LocalUserAuthenticator(r); localErr == nil {
						return userID, nil
					}
					return jwtAuthenticate(r)
				}
			}
			pairingHandlers := pairing.HTTPHandlers{
				Repository:         pairing.DefaultRepository,
				AuthenticateDevice: web_socket.GetMac,
				AuthenticateUser:   authenticateUser,
				DeviceGeneration:   web_socket.DeviceConnectionGeneration,
				RequestPolicy:      web_socket.MeetingRequestPolicy(),
				RateLimiter:        web_socket.NewPairingRateLimiter(),
			}
			s.BindHandler("POST:/stackChan/pairing-nonce", pairingHandlers.PairingNonce)
			s.BindHandler("POST:/stackChan/bind", pairingHandlers.Bind)
			s.BindHandler("GET:/stackChan/devices", pairingHandlers.Devices)
			s.BindHandler("POST:/stackChan/unbind", pairingHandlers.Unbind)
			s.BindHandler("DELETE:/stackChan/bind/:mac", pairingHandlers.UnbindPath)
			s.BindHandler("POST:/stackChan/ws-ticket", pairingHandlers.WSTicket)
			if metricsToken := os.Getenv("STACKCHAN_METRICS_TOKEN"); metricsToken != "" {
				metricsHandler := meetingmetrics.AuthorizedHandler(meetingmetrics.Default, metricsToken)
				s.BindHandler("GET:/stackChan/metrics", func(r *ghttp.Request) {
					metricsHandler.ServeHTTP(r.Response.Writer, r.Request)
				})
			}

			// heartBeat
			boot.InitCron()

			///Configuration file access
			s.Group("/file", func(group *ghttp.RouterGroup) {
				group.GET("/*filepath", func(r *ghttp.Request) {
					relativePath := r.Get("filepath").String()
					if relativePath == "" {
						r.Response.WriteHeader(http.StatusNotFound)
						r.Response.Write("File not found")
						return
					}
					filePath := filepath.Join("file", relativePath)
					if !gfile.Exists(filePath) {
						r.Response.WriteHeader(http.StatusNotFound)
						r.Response.Write("File not found")
						return
					}
					r.Response.ServeFile(filePath)
				})
			})

			s.Group("/stackChan/v2", func(group *ghttp.RouterGroup) {
				group.Middleware(middleware.V2TokenAuthMiddleware, ghttp.MiddlewareHandlerResponse)
				group.Bind(user.NewV2(), dance.NewV2(), device.NewV2())
			})

			s.Group("/stackChan", func(group *ghttp.RouterGroup) {
				group.Middleware(middleware.TokenAuthMiddleware, ghttp.MiddlewareHandlerResponse)
				group.Bind(device.NewV1(), friend.NewV1(), dance.NewV1(), file.NewV1(), post.NewV1(), pano.NewV1(), appstore.NewV1(), xiaozhi.NewV1(), stackchandevice.NewV2())
			})

			s.Group("/admin/stackChan", func(group *ghttp.RouterGroup) {
				group.Middleware(middleware.AdminTokenAuthMiddleware, ghttp.MiddlewareHandlerResponse)
				group.Bind(admin.NewV1(), file.NewV1())
			})

			// The open-source server checkout does not always include the optional
			// management frontend. API/WebSocket startup must not fail without it.
			if gfile.Exists("web/management") {
				s.SetServerRoot("web/management")
			}

			s.SetPort(12800)
			s.Run()
			return nil
		},
	}
)
