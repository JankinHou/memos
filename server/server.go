package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pkg/errors"
	"github.com/usememos/memos/api"
	"github.com/usememos/memos/server/profile"
	"github.com/usememos/memos/store"
	"github.com/usememos/memos/store/db"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

type Server struct {
	e  *echo.Echo
	db *sql.DB

	ID      string
	Profile *profile.Profile
	Store   *store.Store
}

func NewServer(ctx context.Context, profile *profile.Profile) (*Server, error) {
	e := echo.New()
	e.Debug = true
	e.HideBanner = true
	e.HidePort = true

	db := db.NewDB(profile)
	if err := db.Open(ctx); err != nil {
		return nil, errors.Wrap(err, "cannot open db")
	}

	s := &Server{
		e:       e,
		db:      db.DBInstance,
		Profile: profile,
	}
	storeInstance := store.New(db.DBInstance, profile)
	s.Store = storeInstance

	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Format: `{"time":"${time_rfc3339}",` +
			`"method":"${method}","uri":"${uri}",` +
			`"status":${status},"error":"${error}"}` + "\n",
	}))

	e.Use(middleware.Gzip())

	e.Use(middleware.CORS())

	e.Use(middleware.SecureWithConfig(middleware.SecureConfig{
		Skipper:            defaultGetRequestSkipper,
		XSSProtection:      "1; mode=block",
		ContentTypeNosniff: "nosniff",
		XFrameOptions:      "SAMEORIGIN",
		HSTSPreloadEnabled: false,
	}))

	e.Use(middleware.TimeoutWithConfig(middleware.TimeoutConfig{
		ErrorMessage: "Request timeout",
		Timeout:      30 * time.Second,
	}))

	serverID, err := s.getSystemServerID(ctx)
	if err != nil {
		return nil, err
	}
	s.ID = serverID

	embedFrontend(e)

	secret := "usememos"
	if profile.Mode == "prod" {
		secret, err = s.getSystemSecretSessionName(ctx)
		if err != nil {
			return nil, err
		}
	}

	rootGroup := e.Group("")
	s.registerRSSRoutes(rootGroup)

	publicGroup := e.Group("/o")
	publicGroup.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return JWTMiddleware(s, next, secret)
	})
	registerGetterPublicRoutes(publicGroup)
	s.registerResourcePublicRoutes(publicGroup)

	apiGroup := e.Group("/api")
	apiGroup.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return JWTMiddleware(s, next, secret)
	})
	s.registerSystemRoutes(apiGroup)
	s.registerAuthRoutes(apiGroup, secret)
	s.registerUserRoutes(apiGroup)
	s.registerMemoRoutes(apiGroup)
	s.registerShortcutRoutes(apiGroup)
	s.registerResourceRoutes(apiGroup)
	s.registerTagRoutes(apiGroup)
	s.registerStorageRoutes(apiGroup)
	s.registerIdentityProviderRoutes(apiGroup)
	s.registerOpenAIRoutes(apiGroup)

	return s, nil
}

func (s *Server) Start(ctx context.Context) error {
	if err := s.createServerStartActivity(ctx); err != nil {
		return errors.Wrap(err, "failed to create activity")
	}
	return s.e.Start(fmt.Sprintf(":%d", s.Profile.Port))
}

func (s *Server) Shutdown(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Shutdown echo server
	if err := s.e.Shutdown(ctx); err != nil {
		fmt.Printf("failed to shutdown server, error: %v\n", err)
	}

	// Close database connection
	if err := s.db.Close(); err != nil {
		fmt.Printf("failed to close database, error: %v\n", err)
	}

	fmt.Printf("memos stopped properly\n")
}

func (s *Server) GetEcho() *echo.Echo {
	return s.e
}

func (s *Server) createServerStartActivity(ctx context.Context) error {
	payload := api.ActivityServerStartPayload{
		ServerID: s.ID,
		Profile:  s.Profile,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return errors.Wrap(err, "failed to marshal activity payload")
	}
	activity, err := s.Store.CreateActivity(ctx, &api.ActivityCreate{
		CreatorID: api.UnknownID,
		Type:      api.ActivityServerStart,
		Level:     api.ActivityInfo,
		Payload:   string(payloadBytes),
	})
	if err != nil || activity == nil {
		return errors.Wrap(err, "failed to create activity")
	}
	return err
}
