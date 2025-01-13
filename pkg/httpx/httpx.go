package httpx

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/pkg/aop"
	"github.com/ccfos/nightingale/v6/pkg/version"

	"github.com/gin-contrib/pprof"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	Host             string
	Port             int
	CertFile         string
	KeyFile          string
	PProf            bool
	PrintAccessLog   bool
	PrintBody        bool
	ExposeMetrics    bool
	ShutdownTimeout  int
	MaxContentLength int64
	ReadTimeout      int
	WriteTimeout     int
	IdleTimeout      int
	JWTAuth          JWTAuth
	ProxyAuth        ProxyAuth
	ShowCaptcha      ShowCaptcha
	APIForAgent      BasicAuths
	APIForService    BasicAuths
	RSA              RSAConfig
	Token            Token
}

type RSAConfig struct {
	OpenRSA           bool
	RSAPublicKey      []byte
	RSAPublicKeyPath  string
	RSAPrivateKey     []byte
	RSAPrivateKeyPath string
	RSAPassWord       string
}

type ShowCaptcha struct {
	Enable bool
}

type BasicAuths struct {
	BasicAuth gin.Accounts
	Enable    bool
}

type ProxyAuth struct {
	Enable            bool
	HeaderUserNameKey string
	DefaultRoles      []string
}

type JWTAuth struct {
	SigningKey     string
	AccessExpired  int64
	RefreshExpired int64
	RedisKeyPrefix string
}

func GinEngine(mode string, cfg Config, printBodyPaths func() map[string]struct{},
	printAccessLog func() bool) *gin.Engine {
	gin.SetMode(mode)

	loggerMid := aop.Logger(aop.LoggerConfig{PrintAccessLog: printAccessLog,
		PrintBodyPaths: printBodyPaths})
	recoveryMid := aop.Recovery()

	if strings.ToLower(mode) == "release" {
		aop.DisableConsoleColor()
	}

	r := gin.New()

	r.Use(recoveryMid)

	r.Use(loggerMid)

	if cfg.PProf {
		pprof.Register(r, "/api/debug/pprof")
	}

	r.GET("/ping", func(c *gin.Context) {
		c.String(200, "pong")
	})

	r.GET("/pid", func(c *gin.Context) {
		c.String(200, fmt.Sprintf("%d", os.Getpid()))
	})

	r.GET("/ppid", func(c *gin.Context) {
		c.String(200, fmt.Sprintf("%d", os.Getppid()))
	})

	r.GET("/addr", func(c *gin.Context) {
		c.String(200, c.Request.RemoteAddr)
	})

	r.GET("/api/n9e/version", func(c *gin.Context) {
		c.String(200, version.Version)
	})

	if cfg.ExposeMetrics {
		r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	}

	return r
}

func Init(cfg Config, handler http.Handler) func() {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  time.Duration(cfg.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(cfg.WriteTimeout) * time.Second,
		IdleTimeout:  time.Duration(cfg.IdleTimeout) * time.Second,
	}

	go func() {
		fmt.Println("http server listening on:", addr)

		var err error
		if cfg.CertFile != "" && cfg.KeyFile != "" {
			srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			err = srv.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*time.Duration(cfg.ShutdownTimeout))
		defer cancel()

		srv.SetKeepAlivesEnabled(false)
		if err := srv.Shutdown(ctx); err != nil {
			fmt.Println("cannot shutdown http server:", err)
		}

		select {
		case <-ctx.Done():
			fmt.Println("http exiting")
		default:
			fmt.Println("http server stopped")
		}
	}
}

type Token struct {
	Enable             bool
	HeaderUserTokenKey string
}
