package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/models"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt"
	"github.com/google/uuid"
	"github.com/toolkits/pkg/ginx"
)

const (
	DefaultTokenKey = "X-User-Token"
)

type AccessDetails struct {
	AccessUuid   string
	UserIdentity string
}

func (rt *Router) handleProxyUser(c *gin.Context) *models.User {
	headerUserNameKey := rt.HTTP.ProxyAuth.HeaderUserNameKey
	username := c.GetHeader(headerUserNameKey)
	if username == "" {
		ginx.Bomb(http.StatusUnauthorized, "unauthorized")
	}

	user, err := models.UserGetByUsername(rt.Ctx, username)
	if err != nil {
		ginx.Bomb(http.StatusInternalServerError, err.Error())
	}

	if user == nil {
		now := time.Now().Unix()
		user = &models.User{
			Username: username,
			Nickname: username,
			Roles:    strings.Join(rt.HTTP.ProxyAuth.DefaultRoles, " "),
			CreateAt: now,
			UpdateAt: now,
			CreateBy: "system",
			UpdateBy: "system",
		}
		err = user.Add(rt.Ctx)
		if err != nil {
			ginx.Bomb(http.StatusInternalServerError, err.Error())
		}
	}
	return user
}

func (rt *Router) proxyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		user := rt.handleProxyUser(c)
		c.Set("userid", user.Id)
		c.Set("username", user.Username)
		c.Next()
	}
}

// tokenAuth 支持两种方式的认证，固定 token 和 jwt token
// 因为不太好区分用户使用哪个方式，所以两种方式放在一个中间件里
func (rt *Router) tokenAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 先验证固定 token
		tokenKey := rt.HTTP.Token.HeaderUserTokenKey
		if tokenKey == "" {
			tokenKey = DefaultTokenKey
		}
		token := c.GetHeader(tokenKey)

		user, err := models.GetUserByToken(rt.Ctx, token)
		if err == nil && user != nil && user.Username != "" {
			c.Set("userid", user.Id)
			c.Set("username", user.Username)
			c.Next()
			return
		}
		// 再验证 jwt token
		metadata, err := rt.extractTokenMetadata(c.Request)
		if err != nil {
			ginx.Bomb(http.StatusUnauthorized, "unauthorized")
		}

		userIdentity, err := rt.fetchAuth(c.Request.Context(), metadata.AccessUuid)
		if err != nil {
			ginx.Bomb(http.StatusUnauthorized, "unauthorized")
		}

		// ${userid}-${username}
		arr := strings.SplitN(userIdentity, "-", 2)
		if len(arr) != 2 {
			ginx.Bomb(http.StatusUnauthorized, "unauthorized")
		}

		userid, err := strconv.ParseInt(arr[0], 10, 64)
		if err != nil {
			ginx.Bomb(http.StatusUnauthorized, "unauthorized")
		}

		c.Set("userid", userid)
		c.Set("username", arr[1])

		c.Next()
	}
}

func (rt *Router) Auth() gin.HandlerFunc {
	return rt.auth()
}

func (rt *Router) auth() gin.HandlerFunc {
	if rt.HTTP.ProxyAuth.Enable {
		return rt.proxyAuth()
	} else {
		return rt.tokenAuth()
	}
}

// if proxy auth is enabled, mock jwt login/logout/refresh request
func (rt *Router) jwtMock() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !rt.HTTP.ProxyAuth.Enable {
			c.Next()
			return
		}
		if strings.Contains(c.FullPath(), "logout") {
			ginx.Bomb(http.StatusBadRequest, "logout is not supported when proxy auth is enabled")
		}
		user := rt.handleProxyUser(c)
		ginx.NewRender(c).Data(gin.H{
			"user":          user,
			"access_token":  "",
			"refresh_token": "",
		}, nil)
		c.Abort()
	}
}

func (rt *Router) User() gin.HandlerFunc {
	return rt.user()
}

func (rt *Router) user() gin.HandlerFunc {
	return func(c *gin.Context) {
		username := c.MustGet("username").(string)

		user, err := models.UserGetByUsername(rt.Ctx, username)
		if err != nil {
			ginx.Bomb(http.StatusUnauthorized, "unauthorized")
		}

		if user == nil {
			ginx.Bomb(http.StatusUnauthorized, "unauthorized")
		}

		c.Set("user", user)
		c.Set("isadmin", user.IsAdmin())
		// Update user.LastActiveTime
		rt.UserCache.SetLastActiveTime(user.Id, time.Now().Unix())
		c.Next()
	}
}

func (rt *Router) userGroupWrite() gin.HandlerFunc {
	return func(c *gin.Context) {
		me := c.MustGet("user").(*models.User)
		ug := UserGroup(rt.Ctx, ginx.UrlParamInt64(c, "id"))

		can, err := me.CanModifyUserGroup(rt.Ctx, ug)
		ginx.Dangerous(err)

		if !can {
			ginx.Bomb(http.StatusForbidden, "forbidden")
		}

		c.Set("user_group", ug)
		c.Next()
	}
}

func (rt *Router) bgro() gin.HandlerFunc {
	return func(c *gin.Context) {
		me := c.MustGet("user").(*models.User)
		bg := BusiGroup(rt.Ctx, ginx.UrlParamInt64(c, "id"))

		can, err := me.CanDoBusiGroup(rt.Ctx, bg)
		ginx.Dangerous(err)

		if !can {
			ginx.Bomb(http.StatusForbidden, "forbidden")
		}

		c.Set("busi_group", bg)
		c.Next()
	}
}

// bgrw 逐步要被干掉，不安全
func (rt *Router) Bgrw() gin.HandlerFunc {
	return rt.bgrw()
}

func (rt *Router) bgrw() gin.HandlerFunc {
	return func(c *gin.Context) {
		me := c.MustGet("user").(*models.User)
		bg := BusiGroup(rt.Ctx, ginx.UrlParamInt64(c, "id"))

		can, err := me.CanDoBusiGroup(rt.Ctx, bg, "rw")
		ginx.Dangerous(err)

		if !can {
			ginx.Bomb(http.StatusForbidden, "forbidden")
		}

		c.Set("busi_group", bg)
		c.Next()
	}
}

// bgrwCheck 要逐渐替换掉bgrw方法，更安全
func (rt *Router) bgrwCheck(c *gin.Context, bgid int64) {
	me := c.MustGet("user").(*models.User)
	bg := BusiGroup(rt.Ctx, bgid)

	can, err := me.CanDoBusiGroup(rt.Ctx, bg, "rw")
	ginx.Dangerous(err)

	if !can {
		ginx.Bomb(http.StatusForbidden, "forbidden")
	}

	c.Set("busi_group", bg)
}

func (rt *Router) bgrwChecks(c *gin.Context, bgids []int64) {
	set := make(map[int64]struct{})

	for i := 0; i < len(bgids); i++ {
		if _, has := set[bgids[i]]; has {
			continue
		}

		rt.bgrwCheck(c, bgids[i])
		set[bgids[i]] = struct{}{}
	}
}

func (rt *Router) bgroCheck(c *gin.Context, bgid int64) {
	me := c.MustGet("user").(*models.User)
	bg := BusiGroup(rt.Ctx, bgid)

	can, err := me.CanDoBusiGroup(rt.Ctx, bg)
	ginx.Dangerous(err)

	if !can {
		ginx.Bomb(http.StatusForbidden, "forbidden")
	}

	c.Set("busi_group", bg)
}

func (rt *Router) Perm(operation string) gin.HandlerFunc {
	return rt.perm(operation)
}

func (rt *Router) perm(operation string) gin.HandlerFunc {
	return func(c *gin.Context) {
		me := c.MustGet("user").(*models.User)

		can, err := me.CheckPerm(rt.Ctx, operation)
		ginx.Dangerous(err)

		if !can {
			ginx.Bomb(http.StatusForbidden, "forbidden")
		}

		c.Next()
	}
}

func (rt *Router) admin() gin.HandlerFunc {
	return func(c *gin.Context) {
		userid := c.MustGet("userid").(int64)

		user, err := models.UserGetById(rt.Ctx, userid)
		if err != nil {
			ginx.Bomb(http.StatusUnauthorized, "unauthorized")
		}

		if user == nil {
			ginx.Bomb(http.StatusUnauthorized, "unauthorized")
		}

		roles := strings.Fields(user.Roles)
		found := false
		for i := 0; i < len(roles); i++ {
			if roles[i] == models.AdminRole {
				found = true
				break
			}
		}

		if !found {
			ginx.Bomb(http.StatusForbidden, "forbidden")
		}

		c.Set("user", user)
		c.Next()
	}
}

func (rt *Router) extractTokenMetadata(r *http.Request) (*AccessDetails, error) {
	token, err := rt.verifyToken(rt.HTTP.JWTAuth.SigningKey, rt.extractToken(r))
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if ok && token.Valid {
		accessUuid, ok := claims["access_uuid"].(string)
		if !ok {
			return nil, errors.New("failed to parse access_uuid from jwt")
		}

		return &AccessDetails{
			AccessUuid:   accessUuid,
			UserIdentity: claims["user_identity"].(string),
		}, nil
	}

	return nil, err
}

func (rt *Router) extractToken(r *http.Request) string {
	tok := r.Header.Get("Authorization")

	if len(tok) > 6 && strings.ToUpper(tok[0:7]) == "BEARER " {
		return tok[7:]
	}

	return ""
}

func (rt *Router) createAuth(ctx context.Context, userIdentity string, td *TokenDetails) error {
	at := time.Unix(td.AtExpires, 0)
	rte := time.Unix(td.RtExpires, 0)
	now := time.Now()

	errAccess := rt.Redis.Set(ctx, rt.wrapJwtKey(td.AccessUuid), userIdentity, at.Sub(now)).Err()
	if errAccess != nil {
		return errAccess
	}

	errRefresh := rt.Redis.Set(ctx, rt.wrapJwtKey(td.RefreshUuid), userIdentity, rte.Sub(now)).Err()
	if errRefresh != nil {
		return errRefresh
	}

	return nil
}

func (rt *Router) fetchAuth(ctx context.Context, givenUuid string) (string, error) {
	return rt.Redis.Get(ctx, rt.wrapJwtKey(givenUuid)).Result()
}

func (rt *Router) deleteAuth(ctx context.Context, givenUuid string) error {
	return rt.Redis.Del(ctx, rt.wrapJwtKey(givenUuid)).Err()
}

func (rt *Router) deleteTokens(ctx context.Context, authD *AccessDetails) error {
	// get the refresh uuid
	refreshUuid := authD.AccessUuid + "++" + authD.UserIdentity

	// delete access token
	err := rt.Redis.Del(ctx, rt.wrapJwtKey(authD.AccessUuid)).Err()
	if err != nil {
		return err
	}

	// delete refresh token
	err = rt.Redis.Del(ctx, rt.wrapJwtKey(refreshUuid)).Err()
	if err != nil {
		return err
	}

	return nil
}

func (rt *Router) wrapJwtKey(key string) string {
	return rt.HTTP.JWTAuth.RedisKeyPrefix + key
}

type TokenDetails struct {
	AccessToken  string
	RefreshToken string
	AccessUuid   string
	RefreshUuid  string
	AtExpires    int64
	RtExpires    int64
}

func (rt *Router) createTokens(signingKey, userIdentity string) (*TokenDetails, error) {
	td := &TokenDetails{}
	td.AtExpires = time.Now().Add(time.Minute * time.Duration(rt.HTTP.JWTAuth.AccessExpired)).Unix()
	td.AccessUuid = uuid.NewString()

	td.RtExpires = time.Now().Add(time.Minute * time.Duration(rt.HTTP.JWTAuth.RefreshExpired)).Unix()
	td.RefreshUuid = td.AccessUuid + "++" + userIdentity

	var err error
	// Creating Access Token
	atClaims := jwt.MapClaims{}
	atClaims["authorized"] = true
	atClaims["access_uuid"] = td.AccessUuid
	atClaims["user_identity"] = userIdentity
	atClaims["exp"] = td.AtExpires
	at := jwt.NewWithClaims(jwt.SigningMethodHS256, atClaims)
	td.AccessToken, err = at.SignedString([]byte(signingKey))
	if err != nil {
		return nil, err
	}

	// Creating Refresh Token
	rtClaims := jwt.MapClaims{}
	rtClaims["refresh_uuid"] = td.RefreshUuid
	rtClaims["user_identity"] = userIdentity
	rtClaims["exp"] = td.RtExpires
	jrt := jwt.NewWithClaims(jwt.SigningMethodHS256, rtClaims)
	td.RefreshToken, err = jrt.SignedString([]byte(signingKey))
	if err != nil {
		return nil, err
	}

	return td, nil
}

func (rt *Router) verifyToken(signingKey, tokenString string) (*jwt.Token, error) {
	if tokenString == "" {
		return nil, fmt.Errorf("bearer token not found")
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected jwt signing method: %v", token.Header["alg"])
		}
		return []byte(signingKey), nil
	})
	if err != nil {
		return nil, err
	}
	return token, nil
}
